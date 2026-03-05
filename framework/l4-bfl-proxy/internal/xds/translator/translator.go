package translator

import (
	"context"
	"fmt"
	"net"
	"time"

	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	accesslogfilev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/file/v3"
	proxyprotocolv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/proxy_protocol/v3"
	tlsinspectorv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/tls_inspector/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	udpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/udp_proxy/v3"
	rawtransportv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/raw_buffer/v3"
	cachetypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/beclab/l4-bfl-proxy/internal/ir"
	"github.com/beclab/l4-bfl-proxy/internal/message"
	"github.com/telepresenceio/watchable"
	"k8s.io/klog/v2"
)

type XdsTranslator struct {
	xdsIR        *message.XdsIR
	xdsResources *message.XdsResources
}

func New(xdsIR *message.XdsIR, xdsResources *message.XdsResources) *XdsTranslator {
	return &XdsTranslator{
		xdsIR:        xdsIR,
		xdsResources: xdsResources,
	}
}

func (t *XdsTranslator) Name() string { return "xds-translator" }

func (t *XdsTranslator) Start(ctx context.Context) error {
	klog.Info("xds-translator: starting")
	subscription := t.xdsIR.Subscribe(ctx)
	t.process(subscription)
	return nil
}

func (t *XdsTranslator) process(subscription <-chan watchable.Snapshot[string, *ir.Xds]) {
	for snapshot := range subscription {
		for _, update := range snapshot.Updates {
			if update.Delete {
				t.xdsResources.Delete(update.Key)
				continue
			}
			xdsIR := update.Value
			if xdsIR == nil {
				continue
			}
			listeners, clusters := t.translate(xdsIR)
			newSnapshot := &message.XdsSnapshot{
				Listeners: listeners,
				Clusters:  clusters,
			}

			if old, ok := t.xdsResources.Load(update.Key); ok && old.Equal(newSnapshot) {
				klog.V(4).Infof("xds-translator: xDS unchanged for key %s, skipping", update.Key)
				continue
			}

			t.xdsResources.Store(update.Key, newSnapshot)
			klog.Infof("xds-translator: published %d listeners, %d clusters", len(listeners), len(clusters))
		}
	}
	klog.Info("xds-translator: subscription closed")
}

func (t *XdsTranslator) translate(xdsIR *ir.Xds) ([]cachetypes.Resource, []cachetypes.Resource) {
	var listeners []cachetypes.Resource
	var clusters []cachetypes.Resource
	clusterSet := make(map[string]bool)

	for _, listenerIR := range xdsIR.Listeners {
		switch listenerIR.Protocol {
		case ir.ProtocolHTTP:
			l := buildHTTPRedirectListener(listenerIR)
			if l != nil {
				listeners = append(listeners, l)
			}
		case ir.ProtocolTLS:
			l, cls := buildTLSListener(listenerIR, clusterSet)
			if l != nil {
				listeners = append(listeners, l)
			}
			clusters = append(clusters, cls...)
		case ir.ProtocolTCP:
			l, cls := buildTCPListener(listenerIR, clusterSet)
			if l != nil {
				listeners = append(listeners, l)
			}
			clusters = append(clusters, cls...)
		case ir.ProtocolUDP:
			l, cls := buildUDPListener(listenerIR, clusterSet)
			if l != nil {
				listeners = append(listeners, l)
			}
			clusters = append(clusters, cls...)
		}
	}
	return listeners, clusters
}

func buildHTTPRedirectListener(listenerIR *ir.ListenerIR) *listenerv3.Listener {
	if listenerIR.HTTPRedirect == nil {
		return nil
	}

	routeConfig := &routev3.RouteConfiguration{
		Name: listenerIR.Name + "_routes",
		VirtualHosts: []*routev3.VirtualHost{{
			Name:    "redirect",
			Domains: []string{"*"},
			Routes: []*routev3.Route{{
				Match: &routev3.RouteMatch{
					PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"},
				},
				Action: &routev3.Route_Redirect{
					Redirect: &routev3.RedirectAction{
						SchemeRewriteSpecifier: &routev3.RedirectAction_HttpsRedirect{
							HttpsRedirect: true,
						},
						ResponseCode: routev3.RedirectAction_MOVED_PERMANENTLY,
					},
				},
			}},
		}},
	}

	hcm := &hcmv3.HttpConnectionManager{
		StatPrefix: listenerIR.Name,
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{
			RouteConfig: routeConfig,
		},
		HttpFilters: []*hcmv3.HttpFilter{{
			Name: wellknown.Router,
		}},
	}

	hcmAny, _ := anypb.New(hcm)

	return &listenerv3.Listener{
		Name: listenerIR.Name,
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Address: listenerIR.Address,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: listenerIR.Port,
					},
				},
			},
		},
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name: wellknown.HTTPConnectionManager,
				ConfigType: &listenerv3.Filter_TypedConfig{
					TypedConfig: hcmAny,
				},
			}},
		}},
	}
}

func buildTLSListener(listenerIR *ir.ListenerIR, clusterSet map[string]bool) (*listenerv3.Listener, []cachetypes.Resource) {
	var filterChains []*listenerv3.FilterChain
	var clusters []cachetypes.Resource

	for _, route := range listenerIR.Routes {
		if route.Destination == nil {
			continue
		}

		clusterName := route.Destination.Name
		if !clusterSet[clusterName] {
			clusterSet[clusterName] = true
			clusters = append(clusters, buildCluster(route.Destination, route.ProxyProtocolUpstream))
		}

		fc := buildFilterChain(route, clusterName)
		filterChains = append(filterChains, fc)
	}

	var listenerFilters []*listenerv3.ListenerFilter

	if listenerIR.ProxyProtocol {
		ppConfig := &proxyprotocolv3.ProxyProtocol{}
		ppAny, _ := anypb.New(ppConfig)
		listenerFilters = append(listenerFilters, &listenerv3.ListenerFilter{
			Name: "envoy.filters.listener.proxy_protocol",
			ConfigType: &listenerv3.ListenerFilter_TypedConfig{
				TypedConfig: ppAny,
			},
		})
	}

	if listenerIR.TLSInspector {
		tlsConfig := &tlsinspectorv3.TlsInspector{}
		tlsAny, _ := anypb.New(tlsConfig)
		listenerFilters = append(listenerFilters, &listenerv3.ListenerFilter{
			Name: "envoy.filters.listener.tls_inspector",
			ConfigType: &listenerv3.ListenerFilter_TypedConfig{
				TypedConfig: tlsAny,
			},
		})
	}

	listener := &listenerv3.Listener{
		Name: listenerIR.Name,
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Address: listenerIR.Address,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: listenerIR.Port,
					},
				},
			},
		},
		FilterChains:    filterChains,
		ListenerFilters: listenerFilters,
	}

	return listener, clusters
}

func buildFilterChain(route *ir.RouteIR, clusterName string) *listenerv3.FilterChain {
	tcpProxy := &tcpproxyv3.TcpProxy{
		StatPrefix: route.Name,
		ClusterSpecifier: &tcpproxyv3.TcpProxy_Cluster{
			Cluster: clusterName,
		},
		AccessLog: []*accesslogv3.AccessLog{buildAccessLog()},
	}
	tcpProxyAny, _ := anypb.New(tcpProxy)

	fc := &listenerv3.FilterChain{
		Name: route.Name,
		Filters: []*listenerv3.Filter{{
			Name: wellknown.TCPProxy,
			ConfigType: &listenerv3.Filter_TypedConfig{
				TypedConfig: tcpProxyAny,
			},
		}},
	}

	if len(route.SNIMatches) > 0 || len(route.SourcePrefixRanges) > 0 {
		match := &listenerv3.FilterChainMatch{}
		if len(route.SNIMatches) > 0 {
			match.ServerNames = route.SNIMatches
		}
		if len(route.SourcePrefixRanges) > 0 {
			for _, cidr := range route.SourcePrefixRanges {
				prefix, err := parseCIDR(cidr)
				if err != nil {
					klog.Warningf("xds-translator: parse CIDR %q: %v", cidr, err)
					continue
				}
				match.SourcePrefixRanges = append(match.SourcePrefixRanges, prefix)
			}
		}
		fc.FilterChainMatch = match
	}

	return fc
}

func buildTCPListener(listenerIR *ir.ListenerIR, clusterSet map[string]bool) (*listenerv3.Listener, []cachetypes.Resource) {
	var filterChains []*listenerv3.FilterChain
	var clusters []cachetypes.Resource

	for _, route := range listenerIR.Routes {
		if route.Destination == nil {
			continue
		}
		clusterName := route.Destination.Name
		if !clusterSet[clusterName] {
			clusterSet[clusterName] = true
			clusters = append(clusters, buildCluster(route.Destination, false))
		}

		tcpProxy := &tcpproxyv3.TcpProxy{
			StatPrefix: route.Name,
			ClusterSpecifier: &tcpproxyv3.TcpProxy_Cluster{
				Cluster: clusterName,
			},
		}
		tcpProxyAny, _ := anypb.New(tcpProxy)

		filterChains = append(filterChains, &listenerv3.FilterChain{
			Name: route.Name,
			Filters: []*listenerv3.Filter{{
				Name: wellknown.TCPProxy,
				ConfigType: &listenerv3.Filter_TypedConfig{
					TypedConfig: tcpProxyAny,
				},
			}},
		})
	}

	return &listenerv3.Listener{
		Name: listenerIR.Name,
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Address: listenerIR.Address,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: listenerIR.Port,
					},
				},
			},
		},
		FilterChains: filterChains,
	}, clusters
}

func buildUDPListener(listenerIR *ir.ListenerIR, clusterSet map[string]bool) (*listenerv3.Listener, []cachetypes.Resource) {
	var clusters []cachetypes.Resource

	if len(listenerIR.Routes) == 0 || listenerIR.Routes[0].Destination == nil {
		return nil, nil
	}
	route := listenerIR.Routes[0]
	clusterName := route.Destination.Name

	if !clusterSet[clusterName] {
		clusterSet[clusterName] = true
		clusters = append(clusters, buildCluster(route.Destination, false))
	}

	udpProxy := &udpproxyv3.UdpProxyConfig{
		StatPrefix: route.Name,
		RouteSpecifier: &udpproxyv3.UdpProxyConfig_Cluster{
			Cluster: clusterName,
		},
	}
	udpProxyAny, _ := anypb.New(udpProxy)

	return &listenerv3.Listener{
		Name: listenerIR.Name,
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Address:  listenerIR.Address,
					Protocol: corev3.SocketAddress_UDP,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: listenerIR.Port,
					},
				},
			},
		},
		FilterChains: []*listenerv3.FilterChain{{
			Name: route.Name,
			Filters: []*listenerv3.Filter{{
				Name: "envoy.filters.udp_listener.udp_proxy",
				ConfigType: &listenerv3.Filter_TypedConfig{
					TypedConfig: udpProxyAny,
				},
			}},
		}},
		UdpListenerConfig: &listenerv3.UdpListenerConfig{},
	}, clusters
}

func buildCluster(dest *ir.DestinationIR, proxyProtocolUpstream bool) *clusterv3.Cluster {
	cluster := &clusterv3.Cluster{
		Name: dest.Name,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_STRICT_DNS,
		},
		ConnectTimeout: durationpb.New(5 * time.Second),
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: dest.Name,
			Endpoints: []*endpointv3.LocalityLbEndpoints{{
				LbEndpoints: []*endpointv3.LbEndpoint{{
					HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
						Endpoint: &endpointv3.Endpoint{
							Address: &corev3.Address{
								Address: &corev3.Address_SocketAddress{
									SocketAddress: &corev3.SocketAddress{
										Address: dest.Host,
										PortSpecifier: &corev3.SocketAddress_PortValue{
											PortValue: dest.Port,
										},
									},
								},
							},
						},
					},
				}},
			}},
		},
	}

	if proxyProtocolUpstream {
		rawBuf := &rawtransportv3.RawBuffer{}
		rawBufAny, _ := anypb.New(rawBuf)

		cluster.TransportSocket = &corev3.TransportSocket{
			Name: "envoy.transport_sockets.raw_buffer",
			ConfigType: &corev3.TransportSocket_TypedConfig{
				TypedConfig: rawBufAny,
			},
		}
	}

	return cluster
}

func buildAccessLog() *accesslogv3.AccessLog {
	fileLog := &accesslogfilev3.FileAccessLog{
		Path: "/dev/stdout",
	}
	fileLogAny, _ := anypb.New(fileLog)
	return &accesslogv3.AccessLog{
		Name: "envoy.access_loggers.file",
		ConfigType: &accesslogv3.AccessLog_TypedConfig{
			TypedConfig: fileLogAny,
		},
	}
}

func parseCIDR(cidr string) (*corev3.CidrRange, error) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		ip = net.ParseIP(cidr)
		if ip == nil {
			return nil, fmt.Errorf("invalid CIDR %q", cidr)
		}
		return &corev3.CidrRange{
			AddressPrefix: ip.String(),
			PrefixLen:     wrapperspb.UInt32(32),
		}, nil
	}
	ones, _ := ipNet.Mask.Size()
	return &corev3.CidrRange{
		AddressPrefix: ip.String(),
		PrefixLen:     wrapperspb.UInt32(uint32(ones)),
	}, nil
}
