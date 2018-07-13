// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v2

import (
	"errors"
	"io"
	"reflect"
	"sort"
	"sync"
	"time"

	adminapi "github.com/envoyproxy/go-control-plane/envoy/admin/v2alpha"
	xdsapi "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	ads "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v2"
	"github.com/gogo/protobuf/types"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"istio.io/istio/pilot/pkg/model"
	istiolog "istio.io/istio/pkg/log"
)

var (
	adsLog = istiolog.RegisterScope("ads", "ads debugging", 0)

	// adsClients reflect active gRPC channels, for both ADS and EDS.
	adsClients      = map[string]*XdsConnection{}
	adsClientsMutex sync.RWMutex

	// Map of sidecar IDs to XdsConnections, first key is sidecarID, second key is connID
	// This is a map due to an edge case during envoy restart whereby the 'old' envoy
	// reconnects after the 'new/restarted' envoy
	adsSidecarIDConnectionsMap = map[string]map[string]*XdsConnection{}

	// SendTimeout is the max time to wait for a ADS send to complete. This helps detect
	// clients in a bad state (not reading). In future it may include checking for ACK
	SendTimeout = 5 * time.Second

	// PushTimeout is the time to wait for a push on a client. Pilot iterates over
	// clients and pushes them serially for now, to avoid large CPU/memory spikes.
	// We measure and reports cases where pusing a client takes longer.
	PushTimeout = 5 * time.Second
)

var (
	timeZero time.Time
)

var (
	// experiment on getting some monitoring on config errors.
	cdsReject = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pilot_xds_cds_reject",
		Help: "Pilot rejected CSD configs.",
	}, []string{"node", "err"})

	edsReject = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pilot_xds_eds_reject",
		Help: "Pilot rejected EDS.",
	}, []string{"node", "err"})

	edsInstances = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pilot_xds_eds_instances",
		Help: "Instances for each cluster, as of last push. Zero instances is an error",
	}, []string{"cluster"})

	ldsReject = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pilot_xds_lds_reject",
		Help: "Pilot rejected LDS.",
	}, []string{"node", "err"})

	rdsReject = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pilot_xds_rds_reject",
		Help: "Pilot rejected RDS.",
	}, []string{"node", "err"})

	monServices = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pilot_services",
		Help: "Total services known to pilot",
	})

	monVServices = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pilot_virt_services",
		Help: "Total virtual services known to pilot",
	})

	xdsClients = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pilot_xds",
		Help: "Number of endpoints connected to this pilot using XDS",
	})

	writeTimeout = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pilot_xds_write_timeout",
		Help: "Pilot write timeout",
	})

	pushTimeouts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pilot_xds_push_timeout",
		Help: "Pilot push timeout",
	})

	pushes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pilot_xds_pushes",
		Help: "Pilot push timeout",
	}, []string{"type"})

	pushErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pilot_xds_push_errors",
		Help: "Number of errors (timeouts) pushing to sidecars.",
	}, []string{"type"})
)

func init() {
	prometheus.MustRegister(cdsReject)
	prometheus.MustRegister(edsReject)
	prometheus.MustRegister(ldsReject)
	prometheus.MustRegister(rdsReject)
	prometheus.MustRegister(edsInstances)
	prometheus.MustRegister(monServices)
	prometheus.MustRegister(monVServices)
	prometheus.MustRegister(xdsClients)
	prometheus.MustRegister(writeTimeout)
	prometheus.MustRegister(pushTimeouts)
	prometheus.MustRegister(pushes)
	prometheus.MustRegister(pushErrors)
}

// DiscoveryStream is a common interface for EDS and ADS. It also has a
// shorter name.
type DiscoveryStream interface {
	Send(*xdsapi.DiscoveryResponse) error
	Recv() (*xdsapi.DiscoveryRequest, error)
	grpc.ServerStream
}

// XdsConnection is a listener connection type.
type XdsConnection struct {
	// Mutex to protect changes to this XDS connection
	mu sync.RWMutex

	// PeerAddr is the address of the client envoy, from network layer
	PeerAddr string

	// Time of connection, for debugging
	Connect time.Time

	// ConID is the connection identifier, used as a key in the connection table.
	// Currently based on the node name and a counter.
	ConID string

	modelNode *model.Proxy

	// Sending on this channel results in  push. We may also make it a channel of objects so
	// same info can be sent to all clients, without recomputing.
	pushChannel chan *XdsEvent

	// doneChannel will be closed when the client is closed.
	doneChannel chan int

	// TODO: migrate other fields as needed from model.Proxy and replace it

	//HttpConnectionManagers map[string]*http_conn.HttpConnectionManager

	HTTPListeners []*xdsapi.Listener                    `json:"-"`
	RouteConfigs  map[string]*xdsapi.RouteConfiguration `json:"-"`
	HTTPClusters  []*xdsapi.Cluster

	// Last nonce sent and ack'd (timestamps) used for debugging
	ClusterNonceSent, ClusterNonceAcked   string
	ListenerNonceSent, ListenerNonceAcked string
	RouteNonceSent, RouteNonceAcked       string
	EndpointNonceSent, EndpointNonceAcked string
	RoutePercent, EndpointPercent         int

	// current list of clusters monitored by the client
	Clusters []string

	// TODO: TcpListeners (may combine mongo/etc)

	// Both ADS and EDS streams implement this interface
	stream DiscoveryStream

	// Routes is the list of watched Routes.
	Routes []string

	// LDSWatch is set if the remote server is watching Listeners
	LDSWatch bool
	// CDSWatch is set if the remote server is watching Clusters
	CDSWatch bool

	// added will be true if at least one discovery request was received, and the connection
	// is added to the map of active.
	added bool

	// Time of last push
	LastPush time.Time

	// Time of last push failure.
	LastPushFailure time.Time
}

// configDump converts the connection internal state into an Envoy Admin API config dump proto
// It is used in debugging to create a consistent object for comparison between Envoy and Pilot outputs
func (s *DiscoveryServer) configDump(conn *XdsConnection) (*adminapi.ConfigDump, error) {
	configDump := &adminapi.ConfigDump{Configs: map[string]types.Any{}}

	dynamicActiveClusters := []adminapi.ClustersConfigDump_DynamicCluster{}
	clusters, err := s.generateRawClusters(conn)
	if err != nil {
		return nil, err
	}
	for _, cs := range clusters {
		dynamicActiveClusters = append(dynamicActiveClusters, adminapi.ClustersConfigDump_DynamicCluster{Cluster: cs})
	}
	clustersAny, err := types.MarshalAny(&adminapi.ClustersConfigDump{
		VersionInfo:           versionInfo(),
		DynamicActiveClusters: dynamicActiveClusters,
	})
	if err != nil {
		return nil, err
	}
	configDump.Configs["clusters"] = *clustersAny

	dynamicActiveListeners := []adminapi.ListenersConfigDump_DynamicListener{}
	listeners, err := s.generateRawListeners(conn)
	if err != nil {
		return nil, err
	}
	for _, cs := range listeners {
		dynamicActiveListeners = append(dynamicActiveListeners, adminapi.ListenersConfigDump_DynamicListener{Listener: cs})
	}
	listenersAny, err := types.MarshalAny(&adminapi.ListenersConfigDump{
		VersionInfo:            versionInfo(),
		DynamicActiveListeners: dynamicActiveListeners,
	})
	if err != nil {
		return nil, err
	}
	configDump.Configs["listeners"] = *listenersAny

	routes, err := s.generateRawRoutes(conn)
	if err != nil {
		return nil, err
	}
	if len(routes) > 0 {
		dynamicRouteConfig := []adminapi.RoutesConfigDump_DynamicRouteConfig{}
		for _, rs := range routes {
			dynamicRouteConfig = append(dynamicRouteConfig, adminapi.RoutesConfigDump_DynamicRouteConfig{RouteConfig: rs})
		}
		routeConfigAny, err := types.MarshalAny(&adminapi.RoutesConfigDump{DynamicRouteConfigs: dynamicRouteConfig})
		if err != nil {
			return nil, err
		}
		configDump.Configs["routes"] = *routeConfigAny
	}

	return configDump, nil
}

// XdsEvent represents a config or registry event that results in a push.
type XdsEvent struct {

	// If not empty, it is used to indicate the event is caused by a change in the clusters.
	// Only EDS for the listed clusters will be sent.
	clusters []string
}

func newXdsConnection(peerAddr string, stream DiscoveryStream) *XdsConnection {
	return &XdsConnection{
		pushChannel:   make(chan *XdsEvent, 1),
		doneChannel:   make(chan int, 1),
		PeerAddr:      peerAddr,
		Clusters:      []string{},
		Connect:       time.Now(),
		stream:        stream,
		HTTPListeners: []*xdsapi.Listener{},
		RouteConfigs:  map[string]*xdsapi.RouteConfiguration{},
	}
}

func receiveThread(con *XdsConnection, reqChannel chan *xdsapi.DiscoveryRequest, errP *error) {
	defer close(reqChannel) // indicates close of the remote side.
	for {
		req, err := con.stream.Recv()
		if err != nil {
			if status.Code(err) == codes.Canceled || err == io.EOF {
				adsLog.Infof("ADS: %q %s terminated %v", con.PeerAddr, con.ConID, err)
				return
			}
			*errP = err
			adsLog.Errorf("ADS: %q %s terminated with errors %v", con.PeerAddr, con.ConID, err)
			return
		}
		reqChannel <- req
	}
}

// StreamAggregatedResources implements the ADS interface.
func (s *DiscoveryServer) StreamAggregatedResources(stream ads.AggregatedDiscoveryService_StreamAggregatedResourcesServer) error {
	peerInfo, ok := peer.FromContext(stream.Context())
	peerAddr := "0.0.0.0"
	if ok {
		peerAddr = peerInfo.Addr.String()
	}
	var discReq *xdsapi.DiscoveryRequest

	if s.services == nil {
		// first call - lazy loading.
		s.updateModel()
	}

	con := newXdsConnection(peerAddr, stream)
	defer close(con.doneChannel)

	// Do not call: defer close(con.pushChannel) !
	// the push channel will be garbage collected when the connection is no longer used.
	// Closing the channel can cause subtle race conditions with push. According to the spec:
	// "It's only necessary to close a channel when it is important to tell the receiving goroutines that all data
	// have been sent."

	// Reading from a stream is a blocking operation. Each connection needs to read
	// discovery requests and wait for push commands on config change, so we add a
	// go routine. If go grpc adds gochannel support for streams this will not be needed.
	// This also detects close.
	var receiveError error
	reqChannel := make(chan *xdsapi.DiscoveryRequest, 1)
	go receiveThread(con, reqChannel, &receiveError)

	for {
		// Block until either a request is received or the ticker ticks
		select {
		case discReq, ok = <-reqChannel:
			if !ok {
				// Remote side closed connection.
				return receiveError
			}
			if discReq.Node.Id == "" {
				adsLog.Infof("Missing node id %s", discReq.String())
				continue
			}
			nt, err := model.ParseServiceNode(discReq.Node.Id)
			if err != nil {
				return err
			}
			nt.Metadata = model.ParseMetadata(discReq.Node.Metadata)
			con.mu.Lock()
			con.modelNode = &nt
			con.mu.Unlock()
			if con.ConID == "" {
				// first request
				con.ConID = connectionID(discReq.Node.Id)
			}

			switch discReq.TypeUrl {
			case ClusterType:
				if con.CDSWatch {
					// Already received a cluster watch request, this is an ACK
					if discReq.ErrorDetail != nil {
						adsLog.Warnf("ADS:CDS: ACK ERROR %v %s %v", peerAddr, con.ConID, discReq.String())
						cdsReject.With(prometheus.Labels{"node": discReq.Node.Id, "err": discReq.ErrorDetail.Message}).Add(1)
					} else if discReq.ResponseNonce != "" {
						con.ClusterNonceAcked = discReq.ResponseNonce
					}
					adsLog.Debugf("ADS:CDS: ACK %v %v", peerAddr, discReq.String())
					continue
				}
				adsLog.Infof("ADS:CDS: REQ %s %v raw: %s ", con.ConID, peerAddr, discReq.String())
				con.CDSWatch = true
				err := s.pushCds(con)
				if err != nil {
					return err
				}

			case ListenerType:
				if con.LDSWatch {
					// Already received a cluster watch request, this is an ACK
					if discReq.ErrorDetail != nil {
						adsLog.Warnf("ADS:LDS: ACK ERROR %v %s %v", peerAddr, con.modelNode.ID, discReq.String())
						ldsReject.With(prometheus.Labels{"node": discReq.Node.Id, "err": discReq.ErrorDetail.Message}).Add(1)
					} else if discReq.ResponseNonce != "" {
						con.ListenerNonceAcked = discReq.ResponseNonce
					}
					adsLog.Debugf("ADS:LDS: ACK %v", discReq.String())
					continue
				}
				adsLog.Infof("ADS:LDS: REQ %s %v", con.ConID, peerAddr)
				con.LDSWatch = true
				err := s.pushLds(con)
				if err != nil {
					return err
				}

			case RouteType:
				routes := discReq.GetResourceNames()
				if len(routes) == len(con.Routes) || len(routes) == 0 {
					if discReq.ErrorDetail != nil {
						adsLog.Warnf("ADS:RDS: ACK ERROR %v %s (%s) %v", peerAddr, con.ConID, con.modelNode, discReq.String())
						rdsReject.With(prometheus.Labels{"node": discReq.Node.Id, "err": discReq.ErrorDetail.Message}).Add(1)
					}
					// Not logging full request, can be very long.
					adsLog.Debugf("ADS:RDS: ACK %s %s (%s) %s %s", peerAddr, con.ConID, con.modelNode, discReq.VersionInfo, discReq.ResponseNonce)
					if len(con.Routes) > 0 {
						// Already got a list of routes to watch and has same length as the request, this is an ack
						if discReq.ErrorDetail == nil && discReq.ResponseNonce != "" {
							con.mu.Lock()
							con.RouteNonceAcked = discReq.ResponseNonce
							con.mu.Unlock()
						}
						continue
					}
				}
				con.Routes = routes
				adsLog.Infof("ADS:RDS: REQ %s %s (%s) routes: %d", peerAddr, con.ConID, con.modelNode, len(con.Routes))
				err := s.pushRoute(con)
				if err != nil {
					return err
				}

			case EndpointType:
				clusters := discReq.GetResourceNames()
				if discReq.ErrorDetail != nil {
					adsLog.Warnf("ADS:EDS: ACK ERROR %v %s %v", peerAddr, con.ConID, discReq.String())
					edsReject.With(prometheus.Labels{"node": discReq.Node.Id, "err": discReq.ErrorDetail.Message}).Add(1)
				}

				sort.Strings(clusters)
				sort.Strings(con.Clusters)

				// Already got a list of endpoints to watch and it is the same as the request, this is an ack
				if reflect.DeepEqual(con.Clusters, clusters) {
					if discReq.ErrorDetail == nil && discReq.ResponseNonce != "" {
						con.EndpointNonceAcked = discReq.ResponseNonce
						if len(edsClusters) != 0 {
							con.EndpointPercent = int((float64(len(clusters)) / float64(len(edsClusters))) * float64(100))
						}
					}
					continue
				}

				for _, cn := range con.Clusters {
					s.removeEdsCon(cn, con.ConID, con)
				}

				for _, cn := range clusters {
					s.addEdsCon(cn, con.ConID, con)
				}

				con.Clusters = clusters
				adsLog.Infof("ADS:EDS: REQ %s %s clusters: %d", peerAddr, con.ConID, len(con.Clusters))
				err := s.pushEds(con)
				if err != nil {
					return err
				}

			default:
				adsLog.Warnf("ADS: Unknown watched resources %s", discReq.String())
			}

			if !con.added {
				con.added = true
				s.addCon(con.ConID, con)
				defer s.removeCon(con.ConID, con)
			}
		case <-con.pushChannel:
			// It is called when config changes.
			// This is not optimized yet - we should detect what changed based on event and only
			// push resources that need to be pushed.
			if con.CDSWatch {
				err := s.pushCds(con)
				if err != nil {
					return err
				}
			}
			if len(con.Routes) > 0 {
				err := s.pushRoute(con)
				if err != nil {
					return err
				}
			}
			if len(con.Clusters) > 0 {
				err := s.pushEds(con)
				if err != nil {
					return err
				}
			}
			if con.LDSWatch {
				err := s.pushLds(con)
				if err != nil {
					return err
				}
			}
		}
	}
}

func edsClientCount() int {
	var n int
	edsClusterMutex.Lock()
	n = len(adsClients)
	edsClusterMutex.Unlock()
	return n
}

// adsPushAll implements old style invalidation, generated when any rule or endpoint changes.
// Primary code path is from v1 discoveryService.clearCache(), which is added as a handler
// to the model ConfigStorageCache and Controller.
func adsPushAll() {
	// First update all cluster load assignments. This is computed for each cluster once per config change
	// instead of once per endpoint.
	edsClusterMutex.Lock()
	// Create a temp map to avoid locking the add/remove
	cMap := make(map[string]*EdsCluster, len(edsClusters))
	for k, v := range edsClusters {
		cMap[k] = v
	}
	edsClusterMutex.Unlock()

	// UpdateCluster updates the cluster with a mutex, this code is safe ( but computing
	// the update may be duplicated if multiple goroutines compute at the same time).
	// In general this code is called from the 'event' callback that is throttled.
	for clusterName, edsCluster := range cMap {
		if err := updateCluster(clusterName, edsCluster); err != nil {
			adsLog.Errorf("updateCluster failed with clusterName %s", clusterName)
		}
	}

	// Push config changes, iterating over connected envoys. This cover ADS and EDS(0.7), both share
	// the same connection table
	adsClientsMutex.RLock()
	// Create a temp map to avoid locking the add/remove
	tmpMap := make(map[string]*XdsConnection, len(adsClients))
	for k, v := range adsClients {
		tmpMap[k] = v
	}
	adsClientsMutex.RUnlock()

	// This will trigger recomputing the config for each connected Envoy.
	// It will include sending all configs that envoy is listening for, including EDS.
	// TODO: get service, serviceinstances, configs once, to avoid repeated redundant calls.
	// TODO: indicate the specific events, to only push what changed.
	for _, c := range tmpMap {
		// Using non-blocking push has problems if 2 pushes happen too close to each other
		client := c
		// TODO: this should be in a thread group, to do multiple pushes in parallel.
		// Commented out - since we don't have throttling or rate control for push - need to experiment
		// with larger clusters.
		//go func(client *XdsConnection) {
		to := time.After(PushTimeout)
		select {
		case client.pushChannel <- &XdsEvent{}:
			client.LastPush = time.Now()
			client.LastPushFailure = timeZero
		case <-client.doneChannel: // connection was closed
		case <-to:
			pushTimeouts.Add(1)
			//default:
			// This may happen to some clients if the other side is in a bad state and can't receive.
			// The tests were catching this - one of the client was not reading.
			if client.LastPushFailure.IsZero() {
				client.LastPushFailure = time.Now()
				adsLog.Warnf("Failed to push, client busy %s", client.ConID)
				pushErrors.With(prometheus.Labels{"type": "short"}).Add(1)
			} else {
				if time.Since(client.LastPushFailure) > 10*time.Second {
					adsLog.Warnf("Repeated failure to push %s", client.ConID)
					// unfortunately grpc go doesn't allow closing (unblocking) the stream.
					pushErrors.With(prometheus.Labels{"type": "long"}).Add(1)
				}
			}
		}
		//}(client)
	}
}

func (s *DiscoveryServer) addCon(conID string, con *XdsConnection) {
	adsClientsMutex.Lock()
	defer adsClientsMutex.Unlock()
	adsClients[conID] = con
	xdsClients.Set(float64(len(adsClients)))
	if con.modelNode != nil {
		if _, ok := adsSidecarIDConnectionsMap[con.modelNode.ID]; !ok {
			adsSidecarIDConnectionsMap[con.modelNode.ID] = map[string]*XdsConnection{conID: con}
		} else {
			adsSidecarIDConnectionsMap[con.modelNode.ID][conID] = con
		}
	}
}

func (s *DiscoveryServer) removeCon(conID string, con *XdsConnection) {
	adsClientsMutex.Lock()
	defer adsClientsMutex.Unlock()

	for _, c := range con.Clusters {
		s.removeEdsCon(c, conID, con)
	}

	if adsClients[conID] == nil {
		adsLog.Errorf("ADS: Removing connection for non-existing node %v.", s)
	}
	delete(adsClients, conID)
	xdsClients.Set(float64(len(adsClients)))
	if con.modelNode != nil {
		delete(adsSidecarIDConnectionsMap[con.modelNode.ID], conID)
	}
}

// getServicesForEndpoint returns the list of services associated with a node.
// Currently using the node endpoint IP.
//func (s *DiscoveryServer) getServicesForEndpoint(node *model.Proxy) ([]*model.ServiceInstance, error) {
//	// TODO: cache the results, this is a pretty slow operation and called few times per
//	// push
//	proxyInstances, err := s.env.GetProxyServiceInstances(node)
//	return proxyInstances, err
//}

// Send with timeout
func (conn *XdsConnection) send(res *xdsapi.DiscoveryResponse) error {
	done := make(chan error, 1)
	// hardcoded for now - not sure if we need a setting
	t := time.NewTimer(SendTimeout)
	go func() {
		err := conn.stream.Send(res)
		done <- err
		conn.mu.Lock()
		if res.Nonce != "" {
			switch res.TypeUrl {
			case ClusterType:
				conn.ClusterNonceSent = res.Nonce
			case ListenerType:
				conn.ListenerNonceSent = res.Nonce
			case RouteType:
				conn.RouteNonceSent = res.Nonce
			case EndpointType:
				conn.EndpointNonceSent = res.Nonce
			}
		}
		conn.mu.Unlock()
	}()
	select {
	case <-t.C:
		// TODO: wait for ACK
		adsLog.Infof("Timeout writing %s", conn.ConID)
		writeTimeout.Add(1)
		return errors.New("timeout sending")
	case err, _ := <-done:
		_ = t.Stop()
		return err
	}
}
