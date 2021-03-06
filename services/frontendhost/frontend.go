// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package frontendhost

import (
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	c "github.com/uber/cherami-thrift/.generated/go/cherami"
	ccli "github.com/uber/cherami-client-go/client/cherami"
	"github.com/uber/cherami-thrift/.generated/go/controller"
	m "github.com/uber/cherami-thrift/.generated/go/metadata"
	"github.com/uber/cherami-thrift/.generated/go/shared"
	"github.com/uber/cherami-server/common"
	"github.com/uber/cherami-server/common/configure"
	dconfig "github.com/uber/cherami-server/common/dconfigclient"
	"github.com/uber/cherami-server/common/metrics"

	"github.com/pborman/uuid"
	"github.com/uber-common/bark"
	"github.com/uber/tchannel-go/hyperbahn"
	"github.com/uber/tchannel-go/thrift"
)

const (
	maxSizeCacheDestinationPathForUUID = 1000
	defaultDLQConsumedRetention        = 7 * 24 * 3600 // One Week
	defaultDLQUnconsumedRetention      = 7 * 24 * 3600 // One Week
	defaultDLQOwnerEmail               = "default@uber"
)

var nilRequestError = &c.BadRequestError{Message: `Request must not be nil`}

// destinationUUID is the UUID as a string for a destination
type destinationUUID string

// TODO -= Verify that all the metadata vs. cherami status enums are directly convertible =-

// Frontend is the main server class for Frontends
type Frontend struct {
	common.SCommon
	metaClnt                    m.TChanMetadataService
	metadata                    common.MetadataMgr
	hostIDHeartbeater           common.HostIDHeartbeater
	AppConfig                   configure.CommonAppConfig
	hyperbahnClient             *hyperbahn.Client
	cacheDestinationPathForUUID map[destinationUUID]string // Read/Write protected by lk
	lk                          sync.RWMutex
	logger                      bark.Logger
	cacheMutex                  sync.RWMutex
	cClient                     ccli.Client
	publishers                  map[string]*publisherInstance
	consumers                   map[string]*consumerInstance
	outputClientByUUID          map[string]c.TChanBOut
	m3Client                    metrics.Client
	dClient                     dconfig.Client
	useWebsocket                int32 // flag of whether ask client to use websocket to connect with input/output, under uConfig control
}

type publisherInstance struct {
	ccli.Publisher

	// The reference count for this publisher instance.
	refCount int32

	// The timestamp when the reference count last reached zero.
	idleTS time.Time

	receiptsChan chan *ccli.PublisherReceipt
}

type consumerInstance struct {
	ccli.Consumer

	// The reference count for this consumer instance.
	refCount int32

	// The timestamp when the reference count last reached zero.
	idleTS time.Time

	deliveryChan chan ccli.Delivery
}

// interface implementation check
var _ c.TChanBFrontend = &Frontend{}

// Shutdown shuts-down the Frontend cleanly
func (h *Frontend) Shutdown() {
	h.logger.Info("Shutdown()")
	if h.hyperbahnClient != nil {
		h.hyperbahnClient.Close()
	}
}

// Stop stops the service
func (h *Frontend) Stop() {
	h.hostIDHeartbeater.Stop()
	h.SCommon.Stop()
}

// NewFrontendHost is the constructor for Frontend
func NewFrontendHost(serviceName string, sVice common.SCommon, metadataClient m.TChanMetadataService, config configure.CommonAppConfig) (*Frontend, []thrift.TChanServer) {
	// Get the deployment name for logger field
	deploymentName := sVice.GetConfig().GetDeploymentName()

	// update the serviceName for frontend
	bs := Frontend{
		logger:                      (sVice.GetConfig().GetLogger()).WithFields(bark.Fields{common.TagFrnt: common.FmtFrnt(sVice.GetHostUUID()), common.TagDplName: common.FmtDplName(deploymentName)}),
		SCommon:                     sVice,
		metaClnt:                    metadataClient,
		cacheDestinationPathForUUID: make(map[destinationUUID]string),
		cClient:                     nil,
		publishers:                  make(map[string]*publisherInstance),
		consumers:                   make(map[string]*consumerInstance),
		outputClientByUUID:          make(map[string]c.TChanBOut),
		AppConfig:                   config,
	}

	bs.m3Client = metrics.NewClient(sVice.GetMetricsReporter(), metrics.Frontend)

	bs.metadata = common.NewMetadataMgr(bs.metaClnt, bs.m3Client, bs.logger)

	// manage uconfig, regiester handerFunc and verifyFunc for uConfig values
	bs.dClient = sVice.GetDConfigClient()
	bs.dynamicConfigManage()

	// Add the frontend id as a field on all subsequent log lines in this module
	bs.logger.WithFields(bark.Fields{`serviceName`: serviceName, `metaClnt`: bs.metaClnt}).Info(`New Frontend`)
	return &bs, []thrift.TChanServer{c.NewTChanBFrontendServer(&bs)}
	//, clientgen.NewTChanBFrontendServer(&bs)}
}

// Start starts the frontend service and advertises in hyperbahn
func (h *Frontend) Start(thriftService []thrift.TChanServer) {
	h.SCommon.Start(thriftService)
	// we heartbeat the uuid in cassandra just for debugging, request path doesn't depend on this
	h.hostIDHeartbeater = common.NewHostIDHeartbeater(h.metaClnt, h.GetHostUUID(), h.GetHostPort(), h.GetHostName(), h.logger)
	h.hostIDHeartbeater.Start()
}

//
// Helper functions
//

// Cache access

// readCacheDestinationPathForUUID will return the path for a given destination UUID if it is available in the cache; empty string otherwise
func (h *Frontend) readCacheDestinationPathForUUID(dst destinationUUID) (destPath string) {
	h.lk.RLock()
	destPath = h.cacheDestinationPathForUUID[dst]
	h.lk.RUnlock()
	return
}

// writeCacheDestinationPathForUUID adds a UUID->Path mapping to the cache. If the cache has grown too large, it may be dumped before the add.
func (h *Frontend) writeCacheDestinationPathForUUID(dst destinationUUID, destPath string) {
	h.lk.Lock() // Writer lock

	// Dump entire cache if the size gets too large
	if len(h.cacheDestinationPathForUUID) > maxSizeCacheDestinationPathForUUID {
		h.logger.WithField(`maxSizeCacheDestinationPathForUUID`, maxSizeCacheDestinationPathForUUID).Error(`Dumped DestinationPathForUUID Cache; more than elements.`)
		h.cacheDestinationPathForUUID = make(map[destinationUUID]string)
	}

	h.cacheDestinationPathForUUID[dst] = destPath
	h.lk.Unlock()
}

// Metadata <-> Cherami type conversions

// convertDestZoneConfigFromInternal converts internal shared DestinationZoneConfig to Cherami DestinationZoneConfig
func convertDestZoneConfigFromInternal(internalDestZoneCfg *shared.DestinationZoneConfig) *c.DestinationZoneConfig {
	destZoneCfg := c.NewDestinationZoneConfig()
	destZoneCfg.Zone = common.StringPtr(internalDestZoneCfg.GetZone())
	destZoneCfg.AllowPublish = common.BoolPtr(internalDestZoneCfg.GetAllowPublish())
	destZoneCfg.AllowConsume = common.BoolPtr(internalDestZoneCfg.GetAllowConsume())
	destZoneCfg.AlwaysReplicateTo = common.BoolPtr(internalDestZoneCfg.GetAlwaysReplicateTo())
	destZoneCfg.RemoteExtentReplicaNum = common.Int32Ptr(internalDestZoneCfg.GetRemoteExtentReplicaNum())
	return destZoneCfg
}

// convertDestZoneConfigToInternal converts Cherami DestinationZoneConfig to internal shared DestinationZoneConfig
func convertDestZoneConfigToInternal(destZoneCfg *c.DestinationZoneConfig) *shared.DestinationZoneConfig {
	internalDestZoneCfg := shared.NewDestinationZoneConfig()
	internalDestZoneCfg.Zone = common.StringPtr(destZoneCfg.GetZone())
	internalDestZoneCfg.AllowPublish = common.BoolPtr(destZoneCfg.GetAllowPublish())
	internalDestZoneCfg.AllowConsume = common.BoolPtr(destZoneCfg.GetAllowConsume())
	internalDestZoneCfg.AlwaysReplicateTo = common.BoolPtr(destZoneCfg.GetAlwaysReplicateTo())
	internalDestZoneCfg.RemoteExtentReplicaNum = common.Int32Ptr(destZoneCfg.GetRemoteExtentReplicaNum())
	return internalDestZoneCfg
}

// convertCreateDestRequestToInternal converts Cherami CreateDestinationRequest to internal shared CreateDestinationRequest
func convertCreateDestRequestToInternal(createRequest *c.CreateDestinationRequest) *shared.CreateDestinationRequest {
	internalCreateRequest := shared.NewCreateDestinationRequest()
	internalCreateRequest.Path = common.StringPtr(createRequest.GetPath())
	internalCreateRequest.Type = common.InternalDestinationTypePtr(shared.DestinationType(createRequest.GetType()))
	internalCreateRequest.ConsumedMessagesRetention = common.Int32Ptr(createRequest.GetConsumedMessagesRetention())
	internalCreateRequest.UnconsumedMessagesRetention = common.Int32Ptr(createRequest.GetUnconsumedMessagesRetention())
	internalCreateRequest.OwnerEmail = common.StringPtr(createRequest.GetOwnerEmail())
	internalCreateRequest.ChecksumOption = common.InternalChecksumOptionPtr(shared.ChecksumOption(createRequest.GetChecksumOption()))
	internalCreateRequest.OwnerEmail = common.StringPtr(createRequest.GetOwnerEmail())
	internalCreateRequest.IsMultiZone = common.BoolPtr(createRequest.GetIsMultiZone())
	if createRequest.IsSetZoneConfigs() {
		internalCreateRequest.ZoneConfigs = make([]*shared.DestinationZoneConfig, 0, len(createRequest.GetZoneConfigs().GetConfigs()))
		for _, destZoneCfg := range createRequest.GetZoneConfigs().GetConfigs() {
			internalCreateRequest.ZoneConfigs = append(internalCreateRequest.ZoneConfigs, convertDestZoneConfigToInternal(destZoneCfg))
		}
	}
	return internalCreateRequest
}

// convertUpdateDestRequestToInternal converts Cherami UpdateDestinationRequest to internal shared UpdateDestinationRequest
func convertUpdateDestRequestToInternal(updateRequest *c.UpdateDestinationRequest, destUUID string) *shared.UpdateDestinationRequest {
	internalUpdateRequest := shared.NewUpdateDestinationRequest()
	internalUpdateRequest.DestinationUUID = common.StringPtr(destUUID)
	internalUpdateRequest.Status = common.InternalDestinationStatusPtr(shared.DestinationStatus(updateRequest.GetStatus()))
	internalUpdateRequest.ConsumedMessagesRetention = common.Int32Ptr(updateRequest.GetConsumedMessagesRetention())
	internalUpdateRequest.UnconsumedMessagesRetention = common.Int32Ptr(updateRequest.GetUnconsumedMessagesRetention())
	internalUpdateRequest.OwnerEmail = common.StringPtr(updateRequest.GetOwnerEmail())
	internalUpdateRequest.ChecksumOption = common.InternalChecksumOptionPtr(shared.ChecksumOption(updateRequest.GetChecksumOption()))
	return internalUpdateRequest
}

// convertDeleteDestRequestToInternal converts Cherami DeleteDestinationRequest to internal shared DeleteDestinationRequest
func convertDeleteDestRequestToInternal(deleteRequest *c.DeleteDestinationRequest) *shared.DeleteDestinationRequest {
	internalDeleteRequest := shared.NewDeleteDestinationRequest()
	internalDeleteRequest.Path = common.StringPtr(deleteRequest.GetPath())
	return internalDeleteRequest
}

// convertDestinationFromInternal converts internal shared DestinationDescription to Cherami DestinationDescription
func convertDestinationFromInternal(internalDestDesc *shared.DestinationDescription) (destDesc *c.DestinationDescription) {
	destDesc = c.NewDestinationDescription()
	destDesc.Path = common.StringPtr(internalDestDesc.GetPath())
	destDesc.ConsumedMessagesRetention = common.Int32Ptr(internalDestDesc.GetConsumedMessagesRetention())
	destDesc.UnconsumedMessagesRetention = common.Int32Ptr(internalDestDesc.GetUnconsumedMessagesRetention())
	destDesc.Status = c.DestinationStatusPtr(c.DestinationStatus(internalDestDesc.GetStatus()))
	destDesc.Type = c.DestinationTypePtr(c.DestinationType(internalDestDesc.GetType()))
	destDesc.DestinationUUID = common.StringPtr(internalDestDesc.GetDestinationUUID())
	destDesc.OwnerEmail = common.StringPtr(internalDestDesc.GetOwnerEmail())
	destDesc.ChecksumOption = c.ChecksumOption(internalDestDesc.GetChecksumOption())
	destDesc.IsMultiZone = common.BoolPtr(internalDestDesc.GetIsMultiZone())
	if internalDestDesc.IsSetZoneConfigs() {
		destDesc.ZoneConfigs = c.NewDestinationZoneConfigs()
		destDesc.ZoneConfigs.Configs = make([]*c.DestinationZoneConfig, 0, len(internalDestDesc.GetZoneConfigs()))
		for _, _destZoneCfg := range internalDestDesc.GetZoneConfigs() {
			destDesc.ZoneConfigs.Configs = append(destDesc.ZoneConfigs.Configs, convertDestZoneConfigFromInternal(_destZoneCfg))
		}
	}
	return destDesc
}

// convertDestZoneConfigFromInternal converts internal shared ConsumerGroupZoneConfig to Cherami ConsumerGroupZoneConfig
func convertCGZoneConfigFromInternal(internalCGZoneCfg *shared.ConsumerGroupZoneConfig) *c.ConsumerGroupZoneConfig {
	cgZoneCfg := c.NewConsumerGroupZoneConfig()
	cgZoneCfg.Zone = common.StringPtr(internalCGZoneCfg.GetZone())
	cgZoneCfg.Visible = common.BoolPtr(internalCGZoneCfg.GetVisible())
	return cgZoneCfg
}

// convertDestZoneConfigToInternal converts Cherami ConsumerGroupZoneConfig to internal shared ConsumerGroupZoneConfig
func convertCGZoneConfigToInternal(cgZoneCfg *c.ConsumerGroupZoneConfig) *shared.ConsumerGroupZoneConfig {
	internalCGZoneCfg := shared.NewConsumerGroupZoneConfig()
	internalCGZoneCfg.Zone = common.StringPtr(cgZoneCfg.GetZone())
	internalCGZoneCfg.Visible = common.BoolPtr(cgZoneCfg.GetVisible())
	return internalCGZoneCfg
}

// convertCreateCGRequestToInternal converts Cherami CreateConsumerGroupRequest to internal shared CreateConsumerGroupRequest
func convertCreateCGRequestToInternal(createRequest *c.CreateConsumerGroupRequest) *shared.CreateConsumerGroupRequest {
	internalCreateRequest := shared.NewCreateConsumerGroupRequest()
	internalCreateRequest.DestinationPath = common.StringPtr(createRequest.GetDestinationPath())
	internalCreateRequest.ConsumerGroupName = common.StringPtr(createRequest.GetConsumerGroupName())
	internalCreateRequest.LockTimeoutSeconds = common.Int32Ptr(createRequest.GetLockTimeoutInSeconds())
	internalCreateRequest.MaxDeliveryCount = common.Int32Ptr(createRequest.GetMaxDeliveryCount())
	internalCreateRequest.SkipOlderMessagesSeconds = common.Int32Ptr(createRequest.GetSkipOlderMessagesInSeconds())
	internalCreateRequest.StartFrom = common.Int64Ptr(createRequest.GetStartFrom())
	internalCreateRequest.OwnerEmail = common.StringPtr(createRequest.GetOwnerEmail())
	internalCreateRequest.IsMultiZone = common.BoolPtr(createRequest.GetIsMultiZone())
	if createRequest.IsSetZoneConfigs() {
		internalCreateRequest.ActiveZone = common.StringPtr(createRequest.GetZoneConfigs().GetActiveZone())
		internalCreateRequest.ZoneConfigs = make([]*shared.ConsumerGroupZoneConfig, 0, len(createRequest.GetZoneConfigs().GetConfigs()))
		for _, cgZoneCfg := range createRequest.GetZoneConfigs().GetConfigs() {
			internalCreateRequest.ZoneConfigs = append(internalCreateRequest.ZoneConfigs, convertCGZoneConfigToInternal(cgZoneCfg))
		}
	}
	return internalCreateRequest
}

// convertUpdateCGRequestToInternal converts Cherami UpdateConsumerGroupRequest to internal shared UpdateConsumerGroupRequest
func convertUpdateCGRequestToInternal(updateRequest *c.UpdateConsumerGroupRequest) *shared.UpdateConsumerGroupRequest {
	internalUpdateRequest := shared.NewUpdateConsumerGroupRequest()
	internalUpdateRequest.DestinationPath = common.StringPtr(updateRequest.GetDestinationPath())
	internalUpdateRequest.ConsumerGroupName = common.StringPtr(updateRequest.GetConsumerGroupName())
	internalUpdateRequest.LockTimeoutSeconds = common.Int32Ptr(updateRequest.GetLockTimeoutInSeconds())
	internalUpdateRequest.MaxDeliveryCount = common.Int32Ptr(updateRequest.GetMaxDeliveryCount())
	internalUpdateRequest.SkipOlderMessagesSeconds = common.Int32Ptr(updateRequest.GetSkipOlderMessagesInSeconds())
	internalUpdateRequest.Status = common.InternalConsumerGroupStatusPtr(shared.ConsumerGroupStatus(updateRequest.GetStatus()))
	internalUpdateRequest.DeadLetterQueueDestinationUUID = common.StringPtr(``) // Metadata interprets an empty string as "don't change", and nil, as clear
	internalUpdateRequest.OwnerEmail = common.StringPtr(updateRequest.GetOwnerEmail())
	return internalUpdateRequest
}

// convertDeleteCGRequestToInternal converts Cherami DeleteConsumerGroupRequest to internal shared DeleteConsumerGroupRequest
func convertDeleteCGRequestToInternal(deleteRequest *c.DeleteConsumerGroupRequest) *shared.DeleteConsumerGroupRequest {
	internalDeleteRequest := shared.NewDeleteConsumerGroupRequest()
	internalDeleteRequest.ConsumerGroupName = common.StringPtr(deleteRequest.GetConsumerGroupName())

	if common.UUIDRegex.MatchString(deleteRequest.GetDestinationPath()) { // This allows deletion of DLQ consumer groups
		internalDeleteRequest.DestinationUUID = common.StringPtr(deleteRequest.GetDestinationPath())
	} else {
		internalDeleteRequest.DestinationPath = common.StringPtr(deleteRequest.GetDestinationPath())
	}
	return internalDeleteRequest
}

// convertConsumerGroupFromInternal converts internal shared ConsumerGroupDescription to Cherami ConsumerGroupDescription
func (h *Frontend) convertConsumerGroupFromInternal(ctx thrift.Context, _cgDesc *shared.ConsumerGroupDescription) (cgDesc *c.ConsumerGroupDescription, err error) {

	// Check cache to map the destination UUID to destination path
	// DEVNOTE: don't need to worry about cache invalidation here, since a destination UUID will never map to another path; just not vice-versa

	destPath := h.readCacheDestinationPathForUUID(destinationUUID(_cgDesc.GetDestinationUUID()))

	if len(destPath) == 0 {
		var destDesc *shared.DestinationDescription

		destDesc, err = h.metadata.ReadDestination(_cgDesc.GetDestinationUUID(), "") // TODO: -= Maybe a GetDestinationPathForUUID =-

		if err != nil || len(destDesc.GetPath()) == 0 {
			h.logger.WithFields(bark.Fields{
				common.TagDst: common.FmtDst(_cgDesc.GetDestinationUUID()),
				common.TagErr: err,
			}).Error(`Failed to get destination path`)
			return
		}

		destPath = destDesc.GetPath()

		h.writeCacheDestinationPathForUUID(destinationUUID(_cgDesc.GetDestinationUUID()), destPath)
	}

	cgDesc = c.NewConsumerGroupDescription()
	cgDesc.DestinationPath = common.StringPtr(destPath)
	cgDesc.ConsumerGroupName = common.StringPtr(_cgDesc.GetConsumerGroupName())
	cgDesc.Status = c.ConsumerGroupStatusPtr(c.ConsumerGroupStatus(_cgDesc.GetStatus()))
	cgDesc.StartFrom = common.Int64Ptr(_cgDesc.GetStartFrom())
	cgDesc.SkipOlderMessagesInSeconds = common.Int32Ptr(_cgDesc.GetSkipOlderMessagesSeconds())
	cgDesc.MaxDeliveryCount = common.Int32Ptr(_cgDesc.GetMaxDeliveryCount())
	cgDesc.LockTimeoutInSeconds = common.Int32Ptr(_cgDesc.GetLockTimeoutSeconds())
	cgDesc.DeadLetterQueueDestinationUUID = common.StringPtr(_cgDesc.GetDeadLetterQueueDestinationUUID())
	cgDesc.DestinationUUID = common.StringPtr(_cgDesc.GetDestinationUUID())
	cgDesc.ConsumerGroupUUID = common.StringPtr(_cgDesc.GetConsumerGroupUUID())
	cgDesc.OwnerEmail = common.StringPtr(_cgDesc.GetOwnerEmail())
	cgDesc.IsMultiZone = common.BoolPtr(_cgDesc.GetIsMultiZone())
	if _cgDesc.IsSetZoneConfigs() {
		cgDesc.ZoneConfigs = c.NewConsumerGroupZoneConfigs()
		cgDesc.ZoneConfigs.Configs = make([]*c.ConsumerGroupZoneConfig, 0, len(_cgDesc.GetZoneConfigs()))
		for _, _cgZoneCfg := range _cgDesc.GetZoneConfigs() {
			cgDesc.ZoneConfigs.Configs = append(cgDesc.ZoneConfigs.Configs, convertCGZoneConfigFromInternal(_cgZoneCfg))
		}
		cgDesc.ZoneConfigs.ActiveZone = common.StringPtr(_cgDesc.GetActiveZone())
	}
	return
}

// Constants for getUUIDForDestination
const (
	rejectDisabled = true  // destinations with status DestinationStatusDisabled will NOT be returned
	acceptDisabled = false // destinations with status DestinationStatusDisabled will     be returned
)

// GetUUIDForDestination tries to retreive a DestinationUUID given a destination path.
// rejectDisabled indicates that disabled destinations should not be returned. The opposite is
// that any destination that is returned by the metadata server will be returned to the client
// TODO: Add a cache here with time-based retention
func (h *Frontend) getUUIDForDestination(ctx thrift.Context, path string, rejectDisabled bool) (UUID string, err error) {

	destDesc, err := h.metadata.ReadDestination("", path)

	if err != nil {
		h.logger.WithField(common.TagDstPth, common.FmtDstPth(path)).
			WithField(common.TagErr, err).Error(`Couldn't return UUID for destination`)
		return
	}

	if destDesc != nil { // i.e. err == nil
		if rejectDisabled && destDesc.GetStatus() != shared.DestinationStatus_ENABLED {
			h.logger.WithField(common.TagDst, common.FmtDst(destDesc.GetDestinationUUID())).
				WithField(common.TagDstPth, common.FmtDstPth(destDesc.GetPath())).
				WithField(`Status`, destDesc.GetStatus()).Error(`Couldn't return UUID for destination: not enabled`)
			err = c.NewEntityDisabledError()
		} else {
			UUID = destDesc.GetDestinationUUID()
		}
		return
	}

	h.logger.WithField(common.TagDstPth, common.FmtDstPth(path)).
		WithField(common.TagErr, err).Error(`Couldn't return UUID for destination: nil result with no error`)
	return
}

// buildHostAddressesFromHostIds turns hostname:port strings into the internal Cherami hostAddress data structure
func buildHostAddressesFromHostIds(hostIds []string, logger bark.Logger) (hostAddresses []*c.HostAddress) {
	hostAddresses = make([]*c.HostAddress, 0, len(hostIds))
	var hostStr string
	var portStr string
	var err error

	for _, host := range hostIds {
		hA := c.NewHostAddress()

		// Hosts are in "host:port" format; need to split this out into components
		hostStr, portStr, err = net.SplitHostPort(host)
		hA.Host = common.StringPtr(hostStr)

		if err == nil {
			var port int
			port, err = strconv.Atoi(portStr)
			hA.Port = common.Int32Ptr(int32(port))
		}

		if err == nil {
			hostAddresses = append(hostAddresses, hA)
		} else {
			logger.WithFields(bark.Fields{`host`: host, common.TagErr: err}).Error(`Couldn't convert host address`)
		}
	}

	if len(hostAddresses) > 0 {
		return
	}

	return nil
}

// isRunnerDestination checks whether the destination was created by cherami-runner
func isRunnerDestination(destPath string) bool {
	return strings.HasPrefix(destPath, "/runner")
}

// getHostAddressWithProtocol returns host address with different protocols with correct ports, together with deprecation info
// this could be moved once ringpop supports rich meta information so we can store mutiple ports for different protocols
func (h *Frontend) getHostAddressWithProtocol(hostAddresses []*c.HostAddress, serviceName string, forceUseWebsocket bool) []*c.HostProtocol {

	tchannelHosts := &c.HostProtocol{
		HostAddresses: make([]*c.HostAddress, 0, len(hostAddresses)),
		Protocol:      c.ProtocolPtr(c.Protocol_TCHANNEL),
		Deprecated:    common.BoolPtr(forceUseWebsocket || h.GetUseWebsocket() > 0),
	}
	websocketHosts := &c.HostProtocol{
		HostAddresses: make([]*c.HostAddress, 0, len(hostAddresses)),
		Protocol:      c.ProtocolPtr(c.Protocol_WS),
		Deprecated:    common.BoolPtr(false),
	}

	websocketPort := int32(h.AppConfig.GetServiceConfig(serviceName).GetWebsocketPort())
	for _, host := range hostAddresses {
		tchannelHosts.HostAddresses = append(tchannelHosts.HostAddresses, host)
		websocketHosts.HostAddresses = append(websocketHosts.HostAddresses, &c.HostAddress{Host: host.Host, Port: common.Int32Ptr(websocketPort)})
	}

	return []*c.HostProtocol{tchannelHosts, websocketHosts}
}

//
// TChanBFrontendServer implementation
//

// HostPort implements thrift function "HostPort" to return
// the IP address of current instance
func (h *Frontend) HostPort(ctx thrift.Context) (string, error) {
	return h.GetHostPort(), nil
}

// CreateDestination implements TChanBFrontendServer::CreateDestination
func (h *Frontend) CreateDestination(ctx thrift.Context, createRequest *c.CreateDestinationRequest) (destDesc *c.DestinationDescription, err error) {
	sw := h.m3Client.StartTimer(metrics.CreateDestinationScope, metrics.FrontendLatencyTimer)
	defer func() { sw.Stop(); h.epilog(metrics.CreateDestinationScope, destDesc, &err) }()
	if _, err = h.prolog(ctx, createRequest); err != nil {
		return
	}

	lclLg := h.logger.WithField(common.TagDstPth, common.FmtDstPth(createRequest.GetPath()))

	// Request to controller
	cClient, err := h.getControllerClient()
	if err != nil {
		lclLg.WithField(common.TagErr, err).Error(`Can't talk to Controller service, no hosts found`)
		return nil, err
	}

	_destDesc, err := cClient.CreateDestination(ctx, convertCreateDestRequestToInternal(createRequest))
	if _destDesc != nil {
		destDesc = convertDestinationFromInternal(_destDesc)
	}
	if destDesc == nil { // err != nil
		lclLg.WithField(common.TagErr, err).Warn(`Error creating destination`)
		return nil, err
	}

	lclLg.WithFields(bark.Fields{
		common.TagDst:                 common.FmtDst(destDesc.GetDestinationUUID()),
		`Type`:                        destDesc.GetType(),
		`Status`:                      destDesc.GetStatus(),
		`ConsumedMessagesRetention`:   destDesc.GetConsumedMessagesRetention(),
		`UnconsumedMessagesRetention`: destDesc.GetUnconsumedMessagesRetention(),
		`OwnerEmail`:                  destDesc.GetOwnerEmail(),
		`ChecksumOption`:              destDesc.GetChecksumOption(),
		`IsMultiZone`:                 destDesc.GetIsMultiZone(),
	}).Info(`Created Destination`)
	return
}

// DeleteDestination implements TChanBFrontendServer::DeleteDestination
func (h *Frontend) DeleteDestination(ctx thrift.Context, deleteRequest *c.DeleteDestinationRequest) (err error) {
	var allowMutate bool
	sw := h.m3Client.StartTimer(metrics.DeleteDestinationScope, metrics.FrontendLatencyTimer)
	defer func() { sw.Stop(); h.epilogErr(h.logger, metrics.DeleteDestinationScope, &err) }()
	if allowMutate, err = h.prolog(ctx, deleteRequest); err != nil {
		return
	}

	// Disallow delete destination for non-test destinations without a password
	// TODO: remove when appropriate authentication is in place
	if !allowMutate {
		err = &c.BadRequestError{Message: fmt.Sprintf("Contact Cherami team to delete this path: %v", deleteRequest.GetPath())}
		h.logger.Error(err.Error())
		return
	}

	lclLg := h.logger.WithField(common.TagDstPth, common.FmtDstPth(deleteRequest.GetPath()))

	// Request to controller
	cClient, err := h.getControllerClient()
	if err != nil {
		lclLg.WithField(common.TagErr, err).Error(`Can't talk to Controller service, no hosts found`)
		return
	}

	err = cClient.DeleteDestination(ctx, convertDeleteDestRequestToInternal(deleteRequest))

	if err != nil {
		lclLg.WithField(common.TagErr, err).Error(`Error deleting destination`)
		return
	}

	lclLg.Info("Deleted destination")
	return
}

// ReadDestination implements TChanBFrontendServer::ReadDestination
func (h *Frontend) ReadDestination(ctx thrift.Context, readRequest *c.ReadDestinationRequest) (destDesc *c.DestinationDescription, err error) {
	sw := h.m3Client.StartTimer(metrics.ReadDestinationScope, metrics.FrontendLatencyTimer)
	defer func() { sw.Stop(); h.epilog(metrics.ReadDestinationScope, destDesc, &err) }()
	if _, err = h.prolog(ctx, readRequest); err != nil {
		return
	}
	var mReadRequest m.ReadDestinationRequest

	var destUUID, destPath string

	if common.UUIDRegex.MatchString(readRequest.GetPath()) {
		destUUID, destPath = readRequest.GetPath(), ""
	} else {
		destUUID, destPath = "", readRequest.GetPath()
	}

	_destDesc, err := h.metadata.ReadDestination(destUUID, destPath)

	if _destDesc != nil {
		destDesc = convertDestinationFromInternal(_destDesc)
	}

	if err != nil {
		h.logger.WithField(common.TagDstPth, common.FmtDstPth(mReadRequest.GetPath())).
			WithField(common.TagErr, err).Debug(`Error reading destination`)
		return nil, err
	}

	h.logger.WithFields(bark.Fields{
		common.TagDst:                 common.FmtDst(destDesc.GetDestinationUUID()),
		common.TagDstPth:              common.FmtDstPth(destDesc.GetPath()),
		`Type`:                        destDesc.GetType(),
		`Status`:                      destDesc.GetStatus(),
		`ConsumedMessagesRetention`:   destDesc.GetConsumedMessagesRetention(),
		`UnconsumedMessagesRetention`: destDesc.GetUnconsumedMessagesRetention(),
		`OwnerEmail`:                  destDesc.GetOwnerEmail(),
		`ChecksumOption`:              destDesc.GetChecksumOption(),
	}).Debug(`Read destination`)
	return
}

// ReadPublisherOptions implements TChanBFrontendServer::ReadPublisherOptions
// This will replace the ReadDestinationHosts evantually
func (h *Frontend) ReadPublisherOptions(ctx thrift.Context, r *c.ReadPublisherOptionsRequest) (result *c.ReadPublisherOptionsResult_, err error) {
	sw := h.m3Client.StartTimer(metrics.ReadDestinationHostsScope, metrics.FrontendLatencyTimer)
	defer func() { sw.Stop(); h.epilog(metrics.ReadDestinationHostsScope, result, &err) }()
	if _, err = h.prolog(ctx, r); err != nil {
		return
	}

	lclLg := h.logger.WithField(common.TagDstPth,
		common.FmtDstPth(r.GetPath()))

	var destUUID string
	if common.UUIDRegex.MatchString(r.GetPath()) {
		destUUID = r.GetPath()
		// TODO: detect disabled destinations, unless this is already handled by metadata
	} else {
		destUUID, err = h.getUUIDForDestination(ctx, r.GetPath(), rejectDisabled)

		if err != nil || len(destUUID) == 0 {
			if len(destUUID) != 0 {
				lclLg = lclLg.WithField(common.TagDst,
					common.FmtDst(destUUID))
			}

			lclLg.WithField(common.TagErr, err).Error(`Couldn't read destination hosts`)
			return nil, err
		}
	}

	var destDesc *shared.DestinationDescription
	destDesc, err = h.metadata.ReadDestination("", r.GetPath())

	if err != nil {
		return nil, err
	}

	checksumOption := destDesc.GetChecksumOption()

	lclLg = lclLg.WithField(common.TagDst, common.FmtDst(destUUID))

	// Request to the extent controller
	var cClient controller.TChanController
	cClient, err = h.getControllerClient()
	if err != nil {
		lclLg.WithField(common.TagErr, err).Error(`Can't talk to Controller service, no hosts found`)
		return nil, err
	}

	getInputHostReq := &controller.GetInputHostsRequest{DestinationUUID: common.StringPtr(destUUID)}
	var getInputHostResp *controller.GetInputHostsResult_
	getInputHostResp, err = cClient.GetInputHosts(ctx, getInputHostReq)
	if err != nil || len(getInputHostResp.GetInputHostIds()) < 1 {
		lclLg.WithField(common.TagDstPth, common.FmtDstPth(r.GetPath())).
			WithField(common.TagDst, common.FmtDst(destUUID)).
			WithField(common.TagErr, err).
			Error(`ReadPublisherOptions: No hosts returned from controller`)
		return nil, err
	}

	inputHostIds := getInputHostResp.GetInputHostIds()

	forceUseWebsocket := isRunnerDestination(r.GetPath()) // force runners to use websocket

	// Build our result
	rDHResult := c.NewReadPublisherOptionsResult_()
	rDHResult.HostAddresses = buildHostAddressesFromHostIds(inputHostIds, h.logger)
	rDHResult.HostProtocols = h.getHostAddressWithProtocol(rDHResult.HostAddresses, common.InputServiceName, forceUseWebsocket)
	rDHResult.ChecksumOption = c.ChecksumOptionPtr(c.ChecksumOption(checksumOption))

	if len(rDHResult.HostAddresses) > 0 {
		return rDHResult, nil
	}

	lclLg.Error("No hosts extracted from IDs")
	return nil, &c.InternalServiceError{
		Message: "No hosts extracted from IDs",
	}
}

// ReadDestinationHosts implements TChanBFrontendServer::ReadDestinationHosts
// This will be replaced by the ReadPublisherOptions evantually
func (h *Frontend) ReadDestinationHosts(ctx thrift.Context, r *c.ReadDestinationHostsRequest) (result *c.ReadDestinationHostsResult_, err error) {
	sw := h.m3Client.StartTimer(metrics.ReadDestinationHostsScope, metrics.FrontendLatencyTimer)
	defer func() { sw.Stop(); h.epilog(metrics.ReadDestinationHostsScope, result, &err) }()
	if _, err = h.prolog(ctx, r); err != nil {
		return
	}

	// Local logger with additional fields
	lclLg := h.logger.WithField(common.TagDstPth,
		common.FmtDstPth(r.GetPath()))

	var destUUID string
	if common.UUIDRegex.MatchString(r.GetPath()) {
		destUUID = r.GetPath()
		// TODO: detect disabled destinations, unless this is already handled by metadata
	} else {
		destUUID, err = h.getUUIDForDestination(ctx, r.GetPath(), rejectDisabled)

		if err != nil || len(destUUID) == 0 {
			if len(destUUID) != 0 {
				lclLg = lclLg.WithField(common.TagDst,
					common.FmtDst(destUUID))
			}

			lclLg.WithField(common.TagErr, err).Error(`Couldn't read destination hosts`)
			return nil, err
		}
	}

	lclLg = lclLg.WithField(common.TagDst, common.FmtDst(destUUID))

	// Request to the extent controller
	cClient, err := h.getControllerClient()
	if err != nil {
		lclLg.WithField(common.TagErr, err).Error(`Can't talk to Controller service, no hosts found`)
		return nil, err
	}

	getInputHostReq := &controller.GetInputHostsRequest{DestinationUUID: common.StringPtr(destUUID)}
	getInputHostResp, err := cClient.GetInputHosts(ctx, getInputHostReq)
	if err != nil || len(getInputHostResp.GetInputHostIds()) < 1 {
		lclLg.WithField(common.TagDstPth, common.FmtDstPth(r.GetPath())).
			WithField(common.TagDst, common.FmtDst(destUUID)).
			WithField(common.TagErr, err).
			Error(`ReadDestinationHosts: No hosts returned from controller`)
		return nil, err
	}

	inputHostIds := getInputHostResp.GetInputHostIds()

	forceUseWebsocket := isRunnerDestination(r.GetPath()) // force runners to use websocket

	// Build our result
	rDHResult := c.NewReadDestinationHostsResult_()
	rDHResult.HostAddresses = buildHostAddressesFromHostIds(inputHostIds, h.logger)
	rDHResult.HostProtocols = h.getHostAddressWithProtocol(rDHResult.HostAddresses, common.InputServiceName, forceUseWebsocket)

	if len(rDHResult.HostAddresses) > 0 {
		return rDHResult, nil
	}

	lclLg.Error("No hosts extracted from IDs")
	return nil, &c.InternalServiceError{
		Message: "No hosts extracted from IDs",
	}
}

// UpdateDestination implements TChanBFrontendServer::UpdateDestination
func (h *Frontend) UpdateDestination(ctx thrift.Context, updateRequest *c.UpdateDestinationRequest) (destDesc *c.DestinationDescription, err error) {
	var allowMutate bool
	sw := h.m3Client.StartTimer(metrics.UpdateDestinationScope, metrics.FrontendLatencyTimer)
	defer func() { sw.Stop(); h.epilog(metrics.UpdateDestinationScope, destDesc, &err) }()
	if allowMutate, err = h.prolog(ctx, updateRequest); err != nil {
		return
	}

	// Disallow delete destination for non-test destinations without a password
	// TODO: remove when appropriate authentication is in place
	if !allowMutate {
		err := &c.BadRequestError{Message: fmt.Sprintf("Contact Cherami team to update this path: %v", updateRequest.GetPath())}
		h.logger.Error(err.Error())
		return nil, err
	}

	// Local logger with additional fields
	lclLg := h.logger.WithField(common.TagDstPth, common.FmtDstPth(updateRequest.GetPath()))

	// Lookup the destination UUID
	// TODO Caching? Seems like update destination will be low volume
	destUUID, err := h.getUUIDForDestination(ctx, updateRequest.GetPath(), acceptDisabled)

	if err != nil || len(destUUID) == 0 {
		if len(destUUID) != 0 {
			lclLg = lclLg.WithField(common.TagDst,
				common.FmtDst(updateRequest.GetPath()))
		}

		lclLg.WithField(common.TagErr, err).Error(`Error updating destination`)
		return
	}

	lclLg = lclLg.WithField(common.TagDst, common.FmtDst(destUUID))

	// Request to controller
	cClient, err := h.getControllerClient()
	if err != nil {
		lclLg.WithField(common.TagErr, err).Error(`Can't talk to Controller service, no hosts found`)
		return nil, err
	}

	_destDesc, err := cClient.UpdateDestination(ctx, convertUpdateDestRequestToInternal(updateRequest, destUUID))

	if _destDesc != nil {
		destDesc = convertDestinationFromInternal(_destDesc)
	}
	if err != nil {
		lclLg.WithField(common.TagErr, err).Debug(`Error updating destination`)
		return
	}

	lclLg.WithFields(bark.Fields{
		`Type`:                        destDesc.GetType(),
		`Status`:                      destDesc.GetStatus(),
		`ConsumedMessagesRetention`:   destDesc.GetConsumedMessagesRetention(),
		`UnconsumedMessagesRetention`: destDesc.GetUnconsumedMessagesRetention(),
		`OwnerEmail`:                  destDesc.GetOwnerEmail(),
		`ChecksumOption`:              destDesc.GetChecksumOption()}).
		Info(`Updated destination`)
	return
}

// ReadConsumerGroup retrieves a consumer group description from the metadata service
func (h *Frontend) ReadConsumerGroup(ctx thrift.Context, readRequest *c.ReadConsumerGroupRequest) (cGDesc *c.ConsumerGroupDescription, err error) {
	sw := h.m3Client.StartTimer(metrics.ReadConsumerGroupScope, metrics.FrontendLatencyTimer)
	defer func() { sw.Stop(); h.epilog(metrics.ReadConsumerGroupScope, cGDesc, &err) }()
	if _, err = h.prolog(ctx, readRequest); err != nil {
		return
	}

	lclLg := h.logger.WithFields(bark.Fields{
		common.TagDstPth: common.FmtDstPth(readRequest.GetDestinationPath()),
		common.TagCnsPth: common.FmtCnsPth(readRequest.GetConsumerGroupName()),
	})

	// Build a metadata version of the consumer group request
	mCGDesc, err := h.metadata.ReadConsumerGroup("", readRequest.GetDestinationPath(), "", readRequest.GetConsumerGroupName())
	if mCGDesc != nil {
		cGDesc, err = h.convertConsumerGroupFromInternal(ctx, mCGDesc)
		lclLg = lclLg.WithFields(bark.Fields{
			common.TagDst:  common.FmtDst(mCGDesc.GetDestinationUUID()),
			common.TagCnsm: common.FmtCnsm(mCGDesc.GetConsumerGroupUUID()),
		})
	}

	if err != nil {
		lclLg.WithField(common.TagErr, err).Debug(`Error reading consumer group`)
		return
	}

	return
}

// ReadConsumerGroupHosts reads some outputhosts for a destination + consumer group
func (h *Frontend) ReadConsumerGroupHosts(ctx thrift.Context, readRequest *c.ReadConsumerGroupHostsRequest) (rCGHResult *c.ReadConsumerGroupHostsResult_, err error) {
	sw := h.m3Client.StartTimer(metrics.ReadConsumerGroupHostsScope, metrics.FrontendLatencyTimer)
	defer func() { sw.Stop(); h.epilog(metrics.ReadConsumerGroupHostsScope, rCGHResult, &err) }()
	if _, err = h.prolog(ctx, readRequest); err != nil {
		return
	}

	lclLg := h.logger.WithFields(bark.Fields{
		common.TagDstPth: common.FmtDstPth(readRequest.GetDestinationPath()),
		common.TagCnsPth: common.FmtCnsPth(readRequest.GetConsumerGroupName()),
	})

	// Build a metadata version of the consumer group request
	mCGDesc, err := h.metadata.ReadConsumerGroup("", readRequest.GetDestinationPath(), "", readRequest.GetConsumerGroupName())
	if err != nil {
		return nil, err
	}

	if mCGDesc.GetStatus() != shared.ConsumerGroupStatus_ENABLED {
		return nil, &c.EntityDisabledError{Message: fmt.Sprintf("Consumer group is not enabled")}
	}

	// Request to the extent controller
	cClient, err := h.getControllerClient()
	if err != nil {
		lclLg.WithField(common.TagErr, err).Error(`Can't talk to Controller service, no hosts found`)
		return nil, err
	}

	getOutputHostReq := &controller.GetOutputHostsRequest{DestinationUUID: common.StringPtr(mCGDesc.GetDestinationUUID()), ConsumerGroupUUID: common.StringPtr(mCGDesc.GetConsumerGroupUUID())}
	getOutputHostResp, err := cClient.GetOutputHosts(ctx, getOutputHostReq)
	if err != nil || len(getOutputHostResp.GetOutputHostIds()) < 1 {
		lclLg.WithFields(bark.Fields{
			common.TagDstPth: readRequest.GetDestinationPath(),
			common.TagCnsPth: readRequest.GetConsumerGroupName(),
			common.TagErr:    err.Error(),
		}).Error("No hosts returned from controller")
		return nil, err
	}

	outputHostIds := getOutputHostResp.GetOutputHostIds()

	forceUseWebsocket := isRunnerDestination(readRequest.GetDestinationPath()) // force runners to use websocket

	// Build our result
	rCGHResult = c.NewReadConsumerGroupHostsResult_()
	rCGHResult.HostAddresses = buildHostAddressesFromHostIds(outputHostIds, h.logger)
	rCGHResult.HostProtocols = h.getHostAddressWithProtocol(rCGHResult.HostAddresses, common.OutputServiceName, forceUseWebsocket)

	if len(rCGHResult.HostAddresses) > 0 {
		return
	}

	lclLg.WithField("id", outputHostIds).Error("No hosts extracted from IDs for consumer group")
	return nil, &c.InternalServiceError{
		Message: fmt.Sprintf("No hosts extracted from IDs for consumer group. IDs: %v", outputHostIds),
	}
}

// DeleteConsumerGroup deletes a consumer group
func (h *Frontend) DeleteConsumerGroup(ctx thrift.Context, deleteRequest *c.DeleteConsumerGroupRequest) (err error) {
	sw := h.m3Client.StartTimer(metrics.DeleteConsumerGroupScope, metrics.FrontendLatencyTimer)
	defer func() { sw.Stop(); h.epilogErr(h.logger, metrics.DeleteConsumerGroupScope, &err) }()
	if _, err = h.prolog(ctx, deleteRequest); err != nil {
		return
	}

	lclLg := h.logger.WithFields(bark.Fields{
		common.TagDstPth: common.FmtDstPth(deleteRequest.GetDestinationPath()),
		common.TagCnsPth: common.FmtCnsPth(deleteRequest.GetConsumerGroupName()),
	})

	// Request to controller
	cClient, err := h.getControllerClient()
	if err != nil {
		lclLg.WithField(common.TagErr, err).Error(`Can't talk to Controller service, no hosts found`)
		return err
	}

	err = cClient.DeleteConsumerGroup(ctx, convertDeleteCGRequestToInternal(deleteRequest))
	if err != nil {
		lclLg.WithField(common.TagErr, err).Warn(`Error deleting consumer group`)
		return
	}

	lclLg.Info("Consumer group deleted")
	return
}

// CreateConsumerGroup defines a new consumer group in the metadata
func (h *Frontend) CreateConsumerGroup(ctx thrift.Context, createRequest *c.CreateConsumerGroupRequest) (cgDesc *c.ConsumerGroupDescription, err error) {
	sw := h.m3Client.StartTimer(metrics.CreateConsumerGroupScope, metrics.FrontendLatencyTimer)
	defer func() { sw.Stop(); h.epilog(metrics.CreateConsumerGroupScope, cgDesc, &err) }()
	if _, err = h.prolog(ctx, createRequest); err != nil {
		return
	}

	lclLg := h.logger.WithFields(bark.Fields{
		common.TagDstPth: common.FmtDstPth(createRequest.GetDestinationPath()),
		common.TagCnsPth: common.FmtCnsPth(createRequest.GetConsumerGroupName()),
	})

	// request to controller
	cClient, err := h.getControllerClient()
	if err != nil {
		lclLg.WithField(common.TagErr, err).Error(`Can't talk to Controller service, no hosts found`)
		return nil, err
	}
	_createRequest := convertCreateCGRequestToInternal(createRequest)

	// Dead Letter Queue destination creation

	// Only non-UUID (non-DLQ) destinations get a DLQ for the corresponding consumer groups
	// We may create a consumer group consume a DLQ destination and no DLQ destination creation needed in this case
	if common.PathRegex.MatchString(createRequest.GetDestinationPath()) {
		dlqCreateRequest := shared.NewCreateDestinationRequest()
		dlqCreateRequest.ConsumedMessagesRetention = common.Int32Ptr(defaultDLQConsumedRetention)
		dlqCreateRequest.UnconsumedMessagesRetention = common.Int32Ptr(defaultDLQUnconsumedRetention)
		dlqCreateRequest.OwnerEmail = common.StringPtr(defaultDLQOwnerEmail)
		dlqCreateRequest.Type = common.InternalDestinationTypePtr(shared.DestinationType_PLAIN)
		dlqCreateRequest.DLQConsumerGroupUUID = common.StringPtr(uuid.New()) // This is the UUID that the new consumer group will be created with
		dlqPath, _ := common.GetDLQPathNameFromCGName(createRequest.GetConsumerGroupName())
		dlqCreateRequest.Path = common.StringPtr(dlqPath)

		dlqDestDesc, err := cClient.CreateDestination(ctx, dlqCreateRequest)

		if err != nil || dlqDestDesc == nil {
			switch err.(type) {
			case *shared.EntityAlreadyExistsError:
				lclLg.Info("DeadLetterQueue destination already existed")
				dlqDestDesc, err = h.metadata.ReadDestination("", dlqPath)

				if err != nil || dlqDestDesc == nil {
					lclLg.WithField(common.TagErr, err).Error(`Can't read existing DeadLetterQueue destination`)
					return nil, err
				}

				// We continue to consumer group creation if err == nil and dlqDestDesc != nil after the read
			default:
				lclLg.WithField(common.TagErr, err).Error(`Can't create DeadLetterQueue destination`)
				return nil, err
			}
		}

		// Set the DLQ destination UUID in the consumer group
		_createRequest.DeadLetterQueueDestinationUUID = common.StringPtr(dlqDestDesc.GetDestinationUUID())
	} else {
		lclLg.Info("DeadLetterQueue destination not being created")
	}

	// Consumer group creation
	_cgDesc, err := cClient.CreateConsumerGroup(ctx, _createRequest)
	if _cgDesc != nil {
		cgDesc, err = h.convertConsumerGroupFromInternal(ctx, _cgDesc)
		lclLg = lclLg.WithFields(bark.Fields{
			common.TagDst:  common.FmtDst(_cgDesc.GetDestinationUUID()),
			common.TagCnsm: common.FmtCnsm(_cgDesc.GetConsumerGroupUUID()),
		})
	}

	if cgDesc == nil { // err != nil
		lclLg.WithField(common.TagErr, err).Error(`Error creating consumer group`)
		return nil, err
	}

	lclLg.Info("Created consumer group")
	return
}

// UpdateConsumerGroup updates a consumer group
func (h *Frontend) UpdateConsumerGroup(ctx thrift.Context, updateRequest *c.UpdateConsumerGroupRequest) (cgDesc *c.ConsumerGroupDescription, err error) {
	sw := h.m3Client.StartTimer(metrics.UpdateConsumerGroupScope, metrics.FrontendLatencyTimer)
	defer func() { sw.Stop(); h.epilog(metrics.UpdateConsumerGroupScope, cgDesc, &err) }()
	if _, err = h.prolog(ctx, updateRequest); err != nil {
		return
	}

	lclLg := h.logger.WithFields(bark.Fields{
		common.TagDstPth: common.FmtDstPth(updateRequest.GetDestinationPath()),
		common.TagCnsPth: common.FmtCnsPth(updateRequest.GetConsumerGroupName()),
	})

	// Request to controller
	cClient, err := h.getControllerClient()
	if err != nil {
		lclLg.WithField(common.TagErr, err).Error(`Can't talk to Controller service, no hosts found`)
		return nil, err
	}

	_cgDesc, err := cClient.UpdateConsumerGroup(ctx, convertUpdateCGRequestToInternal(updateRequest))
	if _cgDesc != nil {
		cgDesc, err = h.convertConsumerGroupFromInternal(ctx, _cgDesc)
		lclLg = lclLg.WithFields(bark.Fields{
			common.TagDst:  common.FmtDst(_cgDesc.GetDestinationUUID()),
			common.TagCnsm: common.FmtCnsm(_cgDesc.GetConsumerGroupUUID()),
		})
	}

	if cgDesc == nil { // err != nil
		lclLg.WithField(common.TagErr, err).Error(`Error updating consumer group`)
		return nil, err
	}

	lclLg.Info("Updated consumer group")
	return
}

// ListConsumerGroups list all the consumer groups
func (h *Frontend) ListConsumerGroups(ctx thrift.Context, listRequest *c.ListConsumerGroupRequest) (result *c.ListConsumerGroupResult_, resultError error) {
	sw := h.m3Client.StartTimer(metrics.ListConsumerGroupsScope, metrics.FrontendLatencyTimer)
	defer func() { sw.Stop(); h.epilog(metrics.ListConsumerGroupsScope, result, &resultError) }()
	if _, resultError = h.prolog(ctx, listRequest); resultError != nil {
		return
	}

	if len(listRequest.GetConsumerGroupName()) == 0 && listRequest.GetLimit() <= 0 {
		err := &c.BadRequestError{Message: fmt.Sprintf("Invalid limit %d when no consumer group name specified", listRequest.GetLimit())}
		h.logger.Error(err.Error())
		return nil, err
	}

	lclLg := h.logger.WithFields(bark.Fields{
		common.TagDstPth: common.FmtDstPth(listRequest.GetDestinationPath()),
		common.TagCnsPth: common.FmtCnsPth(listRequest.GetConsumerGroupName()),
	})

	mListRequest := m.NewListConsumerGroupRequest()
	mListRequest.ConsumerGroupName = common.StringPtr(listRequest.GetConsumerGroupName())
	mListRequest.DestinationPath = common.StringPtr(listRequest.GetDestinationPath())
	mListRequest.PageToken = listRequest.PageToken
	mListRequest.Limit = common.Int64Ptr(listRequest.GetLimit())

	listResult, err := h.metadata.ListConsumerGroupsPage(mListRequest)

	if err != nil {
		lclLg.WithField(common.TagErr, err).Warn(`List consumer groups failed with error`)
		return nil, err
	}

	result = &c.ListConsumerGroupResult_{
		ConsumerGroups: make([]*c.ConsumerGroupDescription, 0),
		NextPageToken:  listResult.NextPageToken,
	}
	for _, mCGDesc := range listResult.GetConsumerGroups() {
		cg, e := h.convertConsumerGroupFromInternal(ctx, mCGDesc)
		if e != nil {
			lclLg.WithField(common.TagErr, e).Error(`Error converting consumer group list`)
			continue
		}
		result.ConsumerGroups = append(result.ConsumerGroups, cg)
	}
	return result, nil
}

// ListDestinations returns a list of destinations that begin with a given prefix, start with offset and maximum number per limit
func (h *Frontend) ListDestinations(ctx thrift.Context, listRequest *c.ListDestinationsRequest) (result *c.ListDestinationsResult_, resultError error) {
	sw := h.m3Client.StartTimer(metrics.ListDestinationsScope, metrics.FrontendLatencyTimer)
	defer func() { sw.Stop(); h.epilog(metrics.ListDestinationsScope, result, &resultError) }()
	if _, resultError = h.prolog(ctx, listRequest); resultError != nil {
		return
	}

	if listRequest.GetLimit() <= 0 {
		err := &c.BadRequestError{Message: fmt.Sprintf("Invalid limit %d", listRequest.GetLimit())}
		h.logger.Error(err.Error())
		return nil, err
	}

	// TODO Prefix regex?
	mListRequest := shared.NewListDestinationsRequest()
	mListRequest.Prefix = common.StringPtr(listRequest.GetPrefix())
	mListRequest.PageToken = listRequest.PageToken
	mListRequest.Limit = common.Int64Ptr(listRequest.GetLimit())

	lclLg := h.logger.WithField(common.TagDstPth,
		common.FmtDstPth(listRequest.GetPrefix())) // TODO : Prefix might need it's own tag

	// This is the same routine on the metadata library, from which we are forwarding destinations
	listResult, err := h.metadata.ListDestinationsPage(mListRequest)

	if err != nil {
		lclLg.WithFields(bark.Fields{`Prefix`: listRequest.GetPrefix(), common.TagErr: err}).Warn(`List destinations for prefix failed with error`)
		return nil, err
	}

	result = &c.ListDestinationsResult_{
		Destinations:  make([]*c.DestinationDescription, 0),
		NextPageToken: listResult.NextPageToken,
	}
	for _, d := range listResult.GetDestinations() {
		result.Destinations = append(result.Destinations, convertDestinationFromInternal(d))
	}
	return result, nil
}

// PurgeDLQForConsumerGroup purges a DLQ for a consumer group
func (h *Frontend) PurgeDLQForConsumerGroup(ctx thrift.Context, purgeRequest *c.PurgeDLQForConsumerGroupRequest) (err error) {
	sw := h.m3Client.StartTimer(metrics.PurgeDLQForConsumerGroupScope, metrics.FrontendLatencyTimer)
	defer func() { sw.Stop(); h.epilogErr(h.logger, metrics.PurgeDLQForConsumerGroupScope, &err) }()
	if _, err = h.prolog(ctx, purgeRequest); err != nil {
		return
	}

	err = h.dlqOperationForConsumerGroup(ctx, purgeRequest.GetDestinationPath(), purgeRequest.GetConsumerGroupName(), true)
	return
}

func (h *Frontend) dlqOperationForConsumerGroup(ctx thrift.Context, destinationPath, consumerGroupName string, purge bool) (err error) {
	var lclLg bark.Logger
	var mCGDesc *shared.ConsumerGroupDescription

	if purge {
		lclLg = h.logger.WithField(`operation`, `purge`)
	} else {
		lclLg = h.logger.WithField(`operation`, `merge`)
	}

	if common.PathRegex.MatchString(consumerGroupName) { // Normal CG path, not UUID
		lclLg = lclLg.WithFields(bark.Fields{
			common.TagDstPth: common.FmtDstPth(destinationPath),
			common.TagCnsPth: common.FmtCnsPth(consumerGroupName),
		})

		// First, determine the DLQ destination UUID
		mCGDesc, err = h.metadata.ReadConsumerGroup("", destinationPath, "", consumerGroupName)

		if err != nil || mCGDesc == nil {
			lclLg.WithFields(bark.Fields{common.TagErr: err, `mCGDesc`: mCGDesc}).Error(`ReadConsumerGroup failed`)
			return
		}
	} else { // Lookup by CG UUID
		lclLg = lclLg.WithField(common.TagCnsm, common.FmtCnsm(consumerGroupName))

		// First, determine the DLQ destination UUID
		mCGDesc, err = h.metadata.ReadConsumerGroupByUUID(consumerGroupName)

		if err != nil || mCGDesc == nil {
			lclLg.WithFields(bark.Fields{common.TagErr: err, `mCGDesc`: mCGDesc}).Error(`ReadConsumerGroup failed`)
			return
		}
	}

	// Read the destination to see if we should allow this request
	destDesc, err := h.metadata.ReadDestination(mCGDesc.GetDeadLetterQueueDestinationUUID(), "")

	if err != nil || destDesc == nil {
		lclLg.WithFields(bark.Fields{common.TagErr: err, `mCGDesc`: mCGDesc}).Error(`ReadDestination failed`)
		return
	}

	// Now create the merge/purge request, which is simply a cursor update on the DLQ destination
	var now, mergeBefore, purgeBefore common.UnixNanoTime

	now = common.Now()

	if purge {
		mergeBefore, purgeBefore = -1, now
	} else {
		mergeBefore, purgeBefore = now, -1
	}

	mergeTimeExisting := common.UnixNanoTime(destDesc.GetDLQMergeBefore())
	purgeTimeExisting := common.UnixNanoTime(destDesc.GetDLQPurgeBefore())
	mergeActive := mergeTimeExisting != 0
	purgeActive := purgeTimeExisting != 0

	// We disallow updating the timestamp if the opposite operation is still ongoing.
	// Controller will reset the existing timestamp to zero when it is done.
	if purge && mergeActive || !purge && purgeActive {
		msg := `DLQ operation must wait for previous operations to settle`
		lclLg.WithFields(bark.Fields{
			`mergeTime`: common.UnixNanoTime(now - mergeTimeExisting).ToSecondsFmt(),
			`purgeTime`: common.UnixNanoTime(now - purgeTimeExisting).ToSecondsFmt(),
		}).Error(msg)

		e := c.NewInternalServiceError()
		e.Message = msg
		return e
	}

	err = h.metadata.UpdateDestinationDLQCursors(mCGDesc.GetDeadLetterQueueDestinationUUID(), mergeBefore, purgeBefore)

	if err != nil {
		lclLg.WithField(common.TagErr, err).Warn(`Could not merge/purge DLQ for consumer group`)
		return
	}

	lclLg.Info("Consumer group DLQ merged/purged")
	return
}

// MergeDLQForConsumerGroup merges a DLQ for a consumer group
func (h *Frontend) MergeDLQForConsumerGroup(ctx thrift.Context, mergeRequest *c.MergeDLQForConsumerGroupRequest) (err error) {
	sw := h.m3Client.StartTimer(metrics.MergeDLQForConsumerGroupScope, metrics.FrontendLatencyTimer)
	defer func() { sw.Stop(); h.epilogErr(h.logger, metrics.MergeDLQForConsumerGroupScope, &err) }()
	if _, err = h.prolog(ctx, mergeRequest); err != nil {
		return
	}

	err = h.dlqOperationForConsumerGroup(ctx, mergeRequest.GetDestinationPath(), mergeRequest.GetConsumerGroupName(), false)
	return
}

// GetQueueDepthInfo return queue depth info based on the key provided
func (h *Frontend) GetQueueDepthInfo(ctx thrift.Context, queueRequest *c.GetQueueDepthInfoRequest) (result *c.GetQueueDepthInfoResult_, resultError error) {
	defer func() { h.epilog(-1, result, &resultError) }()
	if _, resultError = h.prolog(ctx, queueRequest); resultError != nil {
		return
	}

	cgUUID := queueRequest.GetKey()

	if !common.UUIDRegex.MatchString(cgUUID) { // Special handling to ensure only a UUID is allowed
		resultError = &c.BadRequestError{Message: fmt.Sprintf("Consumer group must be given as UUID, not \"%v\"", cgUUID)}
		return
	}

	// Request to the extent controller
	cClient, err := h.getControllerClient()
	if err != nil {
		h.logger.WithField(common.TagErr, err).Error(`Can't talk to Controller service, no hosts found`)
		return nil, err
	}
	getQueueInfoReq := &controller.GetQueueDepthInfoRequest{Key: common.StringPtr(cgUUID)}
	output, err := cClient.GetQueueDepthInfo(ctx, getQueueInfoReq)

	if err != nil {
		return nil, &c.QueueCacheMissError{Message: fmt.Sprintf("%v", err)}
	}
	value := output.GetValue()
	queueDepthResult := &c.GetQueueDepthInfoResult_{Value: &value}
	return queueDepthResult, nil
}

func (h *Frontend) allowMutatePath(path *string) bool {
	var password string

	if path == nil {
		return false
	}

	split := strings.Split(*path, `+`)
	if len(split) > 1 {
		password = split[1]
	}

	*path = split[0] // Strip the +.... password off

	// In development environments, consider all paths to be test paths
	if common.IsDevelopmentEnvironment(h.GetConfig().GetDeploymentName()) {
		return true
	}

	if h.AppConfig != nil && len(h.AppConfig.GetFrontendConfig().GetMutatePathRegex()) > 0 {
		r, err := regexp.Compile(h.AppConfig.GetFrontendConfig().GetMutatePathRegex())
		if err != nil || r == nil {
			h.logger.WithField(common.TagErr, err).Error(`Failed to compile FrontendConfig.AllowMutatePathRegex`)
		} else {
			if r.MatchString(*path) {
				return true
			}
		}
	}

	if len(password) > 0 && h.AppConfig != nil && len(h.AppConfig.GetFrontendConfig().GetMutatePathPassword()) > 0 {
		hasher := sha1.New()
		hasher.Write([]byte(password))
		sha := base64.URLEncoding.EncodeToString(hasher.Sum(nil))

		if sha == h.AppConfig.GetFrontendConfig().GetMutatePathPassword() {
			return true
		}
	}

	return false
}

// SetUseWebsocket sets the flag of whether ask client to use websocket to connect with input/output
func (h *Frontend) SetUseWebsocket(useWebsocket int32) {
	atomic.StoreInt32(&h.useWebsocket, useWebsocket)
}

// GetUseWebsocket gets the flag of whether ask client to use websocket to connect with input/output
func (h *Frontend) GetUseWebsocket() int {
	return int(atomic.LoadInt32(&h.useWebsocket))
}

func (h *Frontend) incFailureCounterHelper(scope int, errC metrics.ErrorClass, err error) {
	if scope >= 0 {
		h.m3Client.IncCounter(scope, metrics.FrontendFailures)
		if errC == metrics.UserError {
			h.m3Client.IncCounter(scope, metrics.FrontendUserFailures)
		} else {
			h.m3Client.IncCounter(scope, metrics.FrontendInternalFailures)
		}

		if _, ok := err.(*c.EntityNotExistsError); ok {
			h.m3Client.IncCounter(scope, metrics.FrontendEntityNotExist)
		}
	}
}

// Constants to be used with validateName
type uuidPolicy bool
type emptyPolicy bool
type nameType int

const (
	destinationName = nameType(iota + 1)
	consumerGroupName
)

const (
	validateAllowUUID    = uuidPolicy(true)
	validateDisallowUUID = uuidPolicy(false)
)
const (
	validateAllowEmpty    = emptyPolicy(true)
	validateDisallowEmpty = emptyPolicy(false)
)

func (h *Frontend) validateName(path *string, nameType nameType, allowUUID uuidPolicy, allowEmpty emptyPolicy) (allowMutate bool, err error) {
	nameTypeString := `consumer group name`
	if nameType == destinationName {
		nameTypeString = `destination path`
	}

	if path == nil {
		if allowEmpty {
			return
		}
		err = &c.BadRequestError{Message: fmt.Sprintf("%v in request must not be nil", nameTypeString)}
		return
	}

	if bool(allowEmpty) && *path == `` {
		return
	}

	allowEmptyString := ` may be nil/empty, or `
	if !allowEmpty {
		allowEmptyString = ` `
	}

	allowMutate = h.allowMutatePath(path) // Note that this changes the path, stripping the password if there was one

	if bool(allowUUID) && !common.PathRegexAllowUUID.MatchString(*path) {
		err = &c.BadRequestError{Message: fmt.Sprintf("%v in request%vmust be a UUID or resemble \"/foo/bar\", allowing [a-zA-Z0-9._], with at least one letter in each label; \"%v\" does not", nameTypeString, allowEmptyString, *path)}
		return false, err
	}

	if !bool(allowUUID) && !common.PathRegex.MatchString(*path) {
		err = &c.BadRequestError{Message: fmt.Sprintf("%v in request%vmust resemble \"/foo/bar\", allowing [a-zA-Z0-9._], with at least one letter in each label; \"%v\" does not", nameTypeString, allowEmptyString, *path)}
		return false, err
	}

	return
}

// prolog is executed before every frontend API. If it returns a non-nil err, the API should return that error immediately
func (h *Frontend) prolog(ctx thrift.Context, request interface{}) (allowMutate bool, err error) {

	var eD, eC error

	if request == nil || reflect.ValueOf(request).IsNil() {
		err = nilRequestError
		return
	}
	switch v := request.(type) {
	case *c.CreateConsumerGroupRequest:
		_, eD = h.validateName(v.DestinationPath, destinationName, validateAllowUUID, validateDisallowEmpty)
		_, eC = h.validateName(v.ConsumerGroupName, consumerGroupName, validateDisallowUUID, validateDisallowEmpty)
	case *c.CreateDestinationRequest:
		_, eD = h.validateName(v.Path, destinationName, validateDisallowUUID, validateDisallowEmpty)
	case *c.DeleteConsumerGroupRequest:
		_, eD = h.validateName(v.DestinationPath, destinationName, validateAllowUUID, validateDisallowEmpty)
		_, eC = h.validateName(v.ConsumerGroupName, consumerGroupName, validateDisallowUUID, validateDisallowEmpty)
	case *c.DeleteDestinationRequest:
		allowMutate, eD = h.validateName(v.Path, destinationName, validateDisallowUUID, validateDisallowEmpty)
	case *c.GetQueueDepthInfoRequest:
		_, eC = h.validateName(v.Key, consumerGroupName, validateAllowUUID, validateDisallowEmpty)
	case *c.ListConsumerGroupRequest:
		_, eD = h.validateName(v.DestinationPath, destinationName, validateDisallowUUID, validateDisallowEmpty)
		_, eC = h.validateName(v.ConsumerGroupName, consumerGroupName, validateDisallowUUID, validateAllowEmpty)
	case *c.ListDestinationsRequest:
		// There is a prefix for ListDestinations, but we have no regex for it
	case *c.MergeDLQForConsumerGroupRequest:
		_, eD = h.validateName(v.DestinationPath, destinationName, validateDisallowUUID, validateAllowEmpty)
		_, eC = h.validateName(v.ConsumerGroupName, consumerGroupName, validateAllowUUID, validateDisallowEmpty)
	case *c.PurgeDLQForConsumerGroupRequest:
		_, eD = h.validateName(v.DestinationPath, destinationName, validateDisallowUUID, validateAllowEmpty)
		_, eC = h.validateName(v.ConsumerGroupName, consumerGroupName, validateAllowUUID, validateDisallowEmpty)
	case *c.ReadConsumerGroupHostsRequest:
		_, eD = h.validateName(v.DestinationPath, destinationName, validateAllowUUID, validateDisallowEmpty)
		_, eC = h.validateName(v.ConsumerGroupName, consumerGroupName, validateDisallowUUID, validateDisallowEmpty)
	case *c.ReadConsumerGroupRequest:
		_, eD = h.validateName(v.DestinationPath, destinationName, validateAllowUUID, validateDisallowEmpty)
		_, eC = h.validateName(v.ConsumerGroupName, consumerGroupName, validateDisallowUUID, validateDisallowEmpty)
	case *c.ReadDestinationHostsRequest:
		_, eD = h.validateName(v.Path, destinationName, validateAllowUUID, validateDisallowEmpty)
	case *c.ReadDestinationRequest:
		_, eD = h.validateName(v.Path, destinationName, validateAllowUUID, validateDisallowEmpty)
	case *c.ReadPublisherOptionsRequest:
		_, eD = h.validateName(v.Path, destinationName, validateAllowUUID, validateDisallowEmpty)
	case *c.UpdateConsumerGroupRequest:
		_, eD = h.validateName(v.DestinationPath, destinationName, validateAllowUUID, validateDisallowEmpty)
		_, eC = h.validateName(v.ConsumerGroupName, consumerGroupName, validateDisallowUUID, validateDisallowEmpty)
	case *c.UpdateDestinationRequest:
		allowMutate, eD = h.validateName(v.Path, destinationName, validateDisallowUUID, validateDisallowEmpty)
	default:
		panic(fmt.Sprintf(`Request type %v not handled`, v))
	}

	if eD != nil {
		return false, eD
	}
	if eC != nil {
		return false, eC
	}
	return
}

// epilog is only executed after frontend APIs that return a result and error pair. It must call epilogErr, which is called after all API functions
func (h *Frontend) epilog(scope int, r interface{}, err *error) {
	if *err != nil && !reflect.ValueOf(r).IsNil() {
		panic(`set set`)
	}
	if *err == nil && reflect.ValueOf(r).IsNil() {
		*err = &c.InternalServiceError{Message: `Nil result, nil error`} // This is a placeholder error for the case that we would return nil, nil
	}
	h.epilogErr(h.logger, scope, err)
}

// epilogErr is executed after each frontend API returns. It can modify the error as needed, e.g. through epilogErr
func (h *Frontend) epilogErr(l bark.Logger, scope int, err *error) {
	var errC metrics.ErrorClass
	h.m3Client.IncCounter(scope, metrics.FrontendRequests)
	if *err != nil {
		errC, *err = common.ConvertDownstreamErrors(l, *err)
		h.incFailureCounterHelper(scope, errC, *err)
	}
}

// Helper to get controller client in a safe way because h.GetClientFactory()
// could return nil when a handler is invoked before the client factory is
// created.
func (h *Frontend) getControllerClient() (controller.TChanController, error) {
	cf := h.GetClientFactory()
	if cf == nil {
		return nil, &c.InternalServiceError{
			Message: "Service is not ready",
		}
	}

	return cf.GetControllerClient()
}
