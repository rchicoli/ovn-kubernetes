package node

import (
	"fmt"
	"hash/fnv"
	"net"
	"reflect"
	"strings"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/controllers/egressservice"
	nodeipt "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	kapi "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ktypes "k8s.io/apimachinery/pkg/types"
	apierrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	// defaultOpenFlowCookie identifies default open flow rules added to the host OVS bridge.
	// The hex number 0xdeff105, aka defflos, is meant to sound like default flows.
	defaultOpenFlowCookie = "0xdeff105"
	// etpSvcOpenFlowCookie identifies constant open flow rules added to the host OVS
	// bridge to move packets between host and external for etp=local traffic.
	// The hex number 0xe745ecf105, represents etp(e74)-service(5ec)-flows which makes it easier for debugging.
	etpSvcOpenFlowCookie = "0xe745ecf105"
	// ovsLocalPort is the name of the OVS bridge local port
	ovsLocalPort = "LOCAL"
	// ctMarkOVN is the conntrack mark value for OVN traffic
	ctMarkOVN = "0x1"
	// ctMarkHost is the conntrack mark value for host traffic
	ctMarkHost = "0x2"
	// ovnkubeITPMark is the fwmark used for host->ITP=local svc traffic. Note that the fwmark is not a part
	// of the packet, but just stored by kernel in its memory to track/filter packet. Hence fwmark is lost as
	// soon as packet exits the host.
	ovnkubeITPMark = "0x1745ec" // constant itp(174)-service(5ec)
	// ovnkubeSvcViaMgmPortRT is the number of the custom routing table used to steer host->service
	// traffic packets into OVN via ovn-k8s-mp0. Currently only used for ITP=local traffic.
	ovnkubeSvcViaMgmPortRT = "7"
	// ovnKubeNodeSNATMark is used to mark packets that need to be SNAT-ed to nodeIP for
	// traffic originating from egressIP and egressService controlled pods towards other nodes in the cluster.
	ovnKubeNodeSNATMark = "0x3f0"
)

var (
	HostMasqCTZone     = config.Default.ConntrackZone + 1 //64001
	OVNMasqCTZone      = HostMasqCTZone + 1               //64002
	HostNodePortCTZone = config.Default.ConntrackZone + 3 //64003
)

// nodePortWatcherIptables manages iptables rules for shared gateway
// to ensure that services using NodePorts are accessible.
type nodePortWatcherIptables struct {
}

func newNodePortWatcherIptables() *nodePortWatcherIptables {
	return &nodePortWatcherIptables{}
}

// nodePortWatcher manages OpenFlow and iptables rules
// to ensure that services using NodePorts are accessible
type nodePortWatcher struct {
	dpuMode       bool
	gatewayIPv4   string
	gatewayIPv6   string
	gatewayIPLock sync.Mutex
	ofportPhys    string
	ofportPatch   string
	gwBridge      string
	// Map of service name to programmed iptables/OF rules
	serviceInfo     map[ktypes.NamespacedName]*serviceConfig
	serviceInfoLock sync.Mutex
	ofm             *openflowManager
	nodeIPManager   *addressManager
	watchFactory    factory.NodeWatchFactory
}

type serviceConfig struct {
	// Contains the current service
	service *kapi.Service
	// hasLocalHostNetworkEp will be true for a service if it has at least one endpoint which is "hostnetworked&local-to-this-node".
	hasLocalHostNetworkEp bool
	// localEndpoints stores all the local non-host-networked endpoints for this service
	localEndpoints sets.Set[string]
}

type cidrAndFlags struct {
	ipNet *net.IPNet
	flags int
}

func (npw *nodePortWatcher) updateGatewayIPs(addressManager *addressManager) {
	// Get Physical IPs of Node, Can be IPV4 IPV6 or both
	addressManager.gatewayBridge.Lock()
	gatewayIPv4, gatewayIPv6 := getGatewayFamilyAddrs(addressManager.gatewayBridge.ips)
	addressManager.gatewayBridge.Unlock()

	npw.gatewayIPLock.Lock()
	defer npw.gatewayIPLock.Unlock()
	npw.gatewayIPv4 = gatewayIPv4
	npw.gatewayIPv6 = gatewayIPv6
}

// updateServiceFlowCache handles managing breth0 gateway flows for ingress traffic towards kubernetes services
// (nodeport, external, ingress). By default incoming traffic into the node is steered directly into OVN (case3 below).
//
// case1: If a service has externalTrafficPolicy=local, and has host-networked endpoints local to the node (hasLocalHostNetworkEp),
// traffic instead will be steered directly into the host and DNAT-ed to the targetPort on the host.
//
// case2: All other types of services in SGW mode i.e:
//
//	case2a: if externalTrafficPolicy=cluster + SGW mode, traffic will be steered into OVN via GR.
//	case2b: if externalTrafficPolicy=local + !hasLocalHostNetworkEp + SGW mode, traffic will be steered into OVN via GR.
//
// NOTE: If LGW mode, the default flow will take care of sending traffic to host irrespective of service flow type.
//
// `add` parameter indicates if the flows should exist or be removed from the cache
// `hasLocalHostNetworkEp` indicates if at least one host networked endpoint exists for this service which is local to this node.
func (npw *nodePortWatcher) updateServiceFlowCache(service *kapi.Service, add, hasLocalHostNetworkEp bool) error {
	if config.Gateway.Mode == config.GatewayModeLocal && config.Gateway.AllowNoUplink && npw.ofportPhys == "" {
		// if LGW mode and no uplink gateway bridge, ingress traffic enters host from node physical interface instead of the breth0. Skip adding these service flows to br-ex.
		return nil
	}
	npw.gatewayIPLock.Lock()
	defer npw.gatewayIPLock.Unlock()
	var cookie, key string
	var err error
	var errors []error

	isServiceTypeETPLocal := util.ServiceExternalTrafficPolicyLocal(service)

	actions := fmt.Sprintf("output:%s", npw.ofportPatch)

	// cookie is only used for debugging purpose. so it is not fatal error if cookie is failed to be generated.
	for _, svcPort := range service.Spec.Ports {
		protocol := strings.ToLower(string(svcPort.Protocol))
		if svcPort.NodePort > 0 {
			flowProtocols := []string{}
			if config.IPv4Mode {
				flowProtocols = append(flowProtocols, protocol)
			}
			if config.IPv6Mode {
				flowProtocols = append(flowProtocols, protocol+"6")
			}
			for _, flowProtocol := range flowProtocols {
				cookie, err = svcToCookie(service.Namespace, service.Name, flowProtocol, svcPort.NodePort)
				if err != nil {
					klog.Warningf("Unable to generate cookie for nodePort svc: %s, %s, %s, %d, error: %v",
						service.Namespace, service.Name, flowProtocol, svcPort.Port, err)
					cookie = "0"
				}
				key = strings.Join([]string{"NodePort", service.Namespace, service.Name, flowProtocol, fmt.Sprintf("%d", svcPort.NodePort)}, "_")
				// Delete if needed and skip to next protocol
				if !add {
					npw.ofm.deleteFlowsByKey(key)
					continue
				}
				// This allows external traffic ingress when the svc's ExternalTrafficPolicy is
				// set to Local, and the backend pod is HostNetworked. We need to add
				// Flows that will DNAT all traffic coming into nodeport to the nodeIP:Port and
				// ensure that the return traffic is UnDNATed to correct the nodeIP:Nodeport
				if isServiceTypeETPLocal && hasLocalHostNetworkEp {
					// case1 (see function description for details)
					var nodeportFlows []string
					klog.V(5).Infof("Adding flows on breth0 for Nodeport Service %s in Namespace: %s since ExternalTrafficPolicy=local", service.Name, service.Namespace)
					// table 0, This rule matches on all traffic with dst port == NodePort, DNAT's the nodePort to the svc targetPort
					// If ipv6 make sure to choose the ipv6 node address for rule
					if strings.Contains(flowProtocol, "6") {
						nodeportFlows = append(nodeportFlows,
							fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=[%s]:%s),table=6)",
								cookie, npw.ofportPhys, flowProtocol, svcPort.NodePort, HostNodePortCTZone, npw.gatewayIPv6, svcPort.TargetPort.String()))
					} else {
						nodeportFlows = append(nodeportFlows,
							fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=%s:%s),table=6)",
								cookie, npw.ofportPhys, flowProtocol, svcPort.NodePort, HostNodePortCTZone, npw.gatewayIPv4, svcPort.TargetPort.String()))
					}
					nodeportFlows = append(nodeportFlows,
						// table 6, Sends the packet to the host. Note that the constant etp svc cookie is used since this flow would be
						// same for all such services.
						fmt.Sprintf("cookie=%s, priority=110, table=6, actions=output:LOCAL",
							etpSvcOpenFlowCookie),
						// table 0, Matches on return traffic, i.e traffic coming from the host networked pod's port, and unDNATs
						fmt.Sprintf("cookie=%s, priority=110, in_port=LOCAL, %s, tp_src=%s, actions=ct(zone=%d nat,table=7)",
							cookie, flowProtocol, svcPort.TargetPort.String(), HostNodePortCTZone),
						// table 7, Sends the packet back out eth0 to the external client. Note that the constant etp svc
						// cookie is used since this would be same for all such services.
						fmt.Sprintf("cookie=%s, priority=110, table=7, "+
							"actions=output:%s", etpSvcOpenFlowCookie, npw.ofportPhys))
					npw.ofm.updateFlowCacheEntry(key, nodeportFlows)
				} else if config.Gateway.Mode == config.GatewayModeShared {
					// case2 (see function description for details)
					npw.ofm.updateFlowCacheEntry(key, []string{
						// table=0, matches on service traffic towards nodePort and sends it to OVN pipeline
						fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, tp_dst=%d, "+
							"actions=%s",
							cookie, npw.ofportPhys, flowProtocol, svcPort.NodePort, actions),
						// table=0, matches on return traffic from service nodePort and sends it out to primary node interface (br-ex)
						fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, tp_src=%d, "+
							"actions=output:%s",
							cookie, npw.ofportPatch, flowProtocol, svcPort.NodePort, npw.ofportPhys)})
				}
			}
		}

		// Flows for cloud load balancers on Azure/GCP
		// Established traffic is handled by default conntrack rules
		// NodePort/Ingress access in the OVS bridge will only ever come from outside of the host
		for _, ing := range service.Status.LoadBalancer.Ingress {
			if len(ing.IP) > 0 {
				if err = npw.createLbAndExternalSvcFlows(service, &svcPort, add, hasLocalHostNetworkEp, protocol, actions, utilnet.ParseIPSloppy(ing.IP).String(), "Ingress"); err != nil {
					errors = append(errors, err)
				}
			}
		}
		// flows for externalIPs
		for _, externalIP := range service.Spec.ExternalIPs {
			if err = npw.createLbAndExternalSvcFlows(service, &svcPort, add, hasLocalHostNetworkEp, protocol, actions, utilnet.ParseIPSloppy(externalIP).String(), "External"); err != nil {
				errors = append(errors, err)
			}
		}
	}
	return apierrors.NewAggregate(errors)

}

// createLbAndExternalSvcFlows handles managing breth0 gateway flows for ingress traffic towards kubernetes services
// (externalIP and LoadBalancer types). By default incoming traffic into the node is steered directly into OVN (case3 below).
//
// case1: If a service has externalTrafficPolicy=local, and has host-networked endpoints local to the node (hasLocalHostNetworkEp),
// traffic instead will be steered directly into the host and DNAT-ed to the targetPort on the host.
//
// case2: All other types of services in SGW mode i.e:
//
//	case2a: if externalTrafficPolicy=cluster + SGW mode, traffic will be steered into OVN via GR.
//	case2b: if externalTrafficPolicy=local + !hasLocalHostNetworkEp + SGW mode, traffic will be steered into OVN via GR.
//
// NOTE: If LGW mode, the default flow will take care of sending traffic to host irrespective of service flow type.
//
// `add` parameter indicates if the flows should exist or be removed from the cache
// `hasLocalHostNetworkEp` indicates if at least one host networked endpoint exists for this service which is local to this node.
// `protocol` is TCP/UDP/SCTP as set in the svc.Port
// `actions`: "send to patchport"
// `externalIPOrLBIngressIP` is either externalIP.IP or LB.status.ingress.IP
// `ipType` is either "External" or "Ingress"
func (npw *nodePortWatcher) createLbAndExternalSvcFlows(service *kapi.Service, svcPort *kapi.ServicePort, add bool, hasLocalHostNetworkEp bool, protocol string, actions string, externalIPOrLBIngressIP string, ipType string) error {
	if net.ParseIP(externalIPOrLBIngressIP) == nil {
		return fmt.Errorf("failed to parse %s IP: %q", ipType, externalIPOrLBIngressIP)
	}
	flowProtocol := protocol
	nwDst := "nw_dst"
	nwSrc := "nw_src"
	if utilnet.IsIPv6String(externalIPOrLBIngressIP) {
		flowProtocol = protocol + "6"
		nwDst = "ipv6_dst"
		nwSrc = "ipv6_src"
	}
	cookie, err := svcToCookie(service.Namespace, service.Name, externalIPOrLBIngressIP, svcPort.Port)
	if err != nil {
		klog.Warningf("Unable to generate cookie for %s svc: %s, %s, %s, %d, error: %v",
			ipType, service.Namespace, service.Name, externalIPOrLBIngressIP, svcPort.Port, err)
		cookie = "0"
	}
	key := strings.Join([]string{ipType, service.Namespace, service.Name, externalIPOrLBIngressIP, fmt.Sprintf("%d", svcPort.Port)}, "_")
	// Delete if needed and skip to next protocol
	if !add {
		npw.ofm.deleteFlowsByKey(key)
		return nil
	}
	// add the ARP bypass flow regardless of service type or gateway modes since its applicable in all scenarios.
	arpFlow := npw.generateArpBypassFlow(protocol, externalIPOrLBIngressIP, cookie)
	externalIPFlows := []string{arpFlow}
	// This allows external traffic ingress when the svc's ExternalTrafficPolicy is
	// set to Local, and the backend pod is HostNetworked. We need to add
	// Flows that will DNAT all external traffic destined for the lb/externalIP service
	// to the nodeIP / nodeIP:port of the host networked backend.
	// And then ensure that return traffic is UnDNATed correctly back
	// to the ingress / external IP
	isServiceTypeETPLocal := util.ServiceExternalTrafficPolicyLocal(service)
	if isServiceTypeETPLocal && hasLocalHostNetworkEp {
		// case1 (see function description for details)
		klog.V(5).Infof("Adding flows on breth0 for %s Service %s in Namespace: %s since ExternalTrafficPolicy=local", ipType, service.Name, service.Namespace)
		// table 0, This rule matches on all traffic with dst ip == LoadbalancerIP / externalIP, DNAT's the nodePort to the svc targetPort
		// If ipv6 make sure to choose the ipv6 node address for rule
		if strings.Contains(flowProtocol, "6") {
			externalIPFlows = append(externalIPFlows,
				fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=[%s]:%s),table=6)",
					cookie, npw.ofportPhys, flowProtocol, nwDst, externalIPOrLBIngressIP, svcPort.Port, HostNodePortCTZone, npw.gatewayIPv6, svcPort.TargetPort.String()))
		} else {
			externalIPFlows = append(externalIPFlows,
				fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=%s:%s),table=6)",
					cookie, npw.ofportPhys, flowProtocol, nwDst, externalIPOrLBIngressIP, svcPort.Port, HostNodePortCTZone, npw.gatewayIPv4, svcPort.TargetPort.String()))
		}
		externalIPFlows = append(externalIPFlows,
			// table 6, Sends the packet to Host. Note that the constant etp svc cookie is used since this flow would be
			// same for all such services.
			fmt.Sprintf("cookie=%s, priority=110, table=6, actions=output:LOCAL",
				etpSvcOpenFlowCookie),
			// table 0, Matches on return traffic, i.e traffic coming from the host networked pod's port, and unDNATs
			fmt.Sprintf("cookie=%s, priority=110, in_port=LOCAL, %s, tp_src=%s, actions=ct(commit,zone=%d nat,table=7)",
				cookie, flowProtocol, svcPort.TargetPort.String(), HostNodePortCTZone),
			// table 7, Sends the reply packet back out eth0 to the external client. Note that the constant etp svc
			// cookie is used since this would be same for all such services.
			fmt.Sprintf("cookie=%s, priority=110, table=7, actions=output:%s",
				etpSvcOpenFlowCookie, npw.ofportPhys))
	} else if config.Gateway.Mode == config.GatewayModeShared {
		// case2 (see function description for details)
		externalIPFlows = append(externalIPFlows,
			// table=0, matches on service traffic towards externalIP or LB ingress and sends it to OVN pipeline
			fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_dst=%d, "+
				"actions=%s",
				cookie, npw.ofportPhys, flowProtocol, nwDst, externalIPOrLBIngressIP, svcPort.Port, actions),
			// table=0, matches on return traffic from service externalIP or LB ingress and sends it out to primary node interface (br-ex)
			fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_src=%d, "+
				"actions=output:%s",
				cookie, npw.ofportPatch, flowProtocol, nwSrc, externalIPOrLBIngressIP, svcPort.Port, npw.ofportPhys))
	}
	npw.ofm.updateFlowCacheEntry(key, externalIPFlows)

	return nil
}

// generate ARP/NS bypass flow which will send the ARP/NS request everywhere *but* to OVN
// OpenFlow will not do hairpin switching, so we can safely add the origin port to the list of ports, too
func (npw *nodePortWatcher) generateArpBypassFlow(protocol string, ipAddr string, cookie string) string {
	addrResDst := "arp_tpa"
	addrResProto := "arp, arp_op=1"
	if utilnet.IsIPv6String(ipAddr) {
		addrResDst = "nd_target"
		addrResProto = "icmp6, icmp_type=135, icmp_code=0"
	}

	var arpFlow string
	var arpPortsFiltered []string
	arpPorts, err := util.GetOpenFlowPorts(npw.gwBridge, false)
	if err != nil {
		// in the odd case that getting all ports from the bridge should not work,
		// simply output to LOCAL (this should work well in the vast majority of cases, anyway)
		klog.Warningf("Unable to get port list from bridge. Using ovsLocalPort as output only: error: %v",
			err)
		arpFlow = fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, "+
			"actions=output:%s",
			cookie, npw.ofportPhys, addrResProto, addrResDst, ipAddr, ovsLocalPort)
	} else {
		// cover the case where breth0 has more than 3 ports, e.g. if an admin adds a 4th port
		// and the ExternalIP would be on that port
		// Use all ports except for ofPortPhys and the ofportPatch
		// Filtering ofPortPhys is for consistency / readability only, OpenFlow will not send
		// out the in_port normally (see man 7 ovs-actions)
		for _, port := range arpPorts {
			if port == npw.ofportPatch || port == npw.ofportPhys {
				continue
			}
			arpPortsFiltered = append(arpPortsFiltered, port)
		}
		arpFlow = fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, "+
			"actions=output:%s",
			cookie, npw.ofportPhys, addrResProto, addrResDst, ipAddr, strings.Join(arpPortsFiltered, ","))
	}

	return arpFlow
}

// getAndDeleteServiceInfo returns the serviceConfig for a service and if it exists and then deletes the entry
func (npw *nodePortWatcher) getAndDeleteServiceInfo(index ktypes.NamespacedName) (out *serviceConfig, exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()
	out, exists = npw.serviceInfo[index]
	delete(npw.serviceInfo, index)
	return out, exists
}

// getServiceInfo returns the serviceConfig for a service and if it exists
func (npw *nodePortWatcher) getServiceInfo(index ktypes.NamespacedName) (out *serviceConfig, exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()
	out, exists = npw.serviceInfo[index]
	return out, exists
}

// getAndSetServiceInfo creates and sets the serviceConfig, returns if it existed and whatever was there
func (npw *nodePortWatcher) getAndSetServiceInfo(index ktypes.NamespacedName, service *kapi.Service, hasLocalHostNetworkEp bool, localEndpoints sets.Set[string]) (old *serviceConfig, exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()

	old, exists = npw.serviceInfo[index]
	var ptrCopy serviceConfig
	if exists {
		ptrCopy = *old
	}
	npw.serviceInfo[index] = &serviceConfig{service: service, hasLocalHostNetworkEp: hasLocalHostNetworkEp, localEndpoints: localEndpoints}
	return &ptrCopy, exists
}

// addOrSetServiceInfo creates and sets the serviceConfig if it doesn't exist
func (npw *nodePortWatcher) addOrSetServiceInfo(index ktypes.NamespacedName, service *kapi.Service, hasLocalHostNetworkEp bool, localEndpoints sets.Set[string]) (exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()

	if _, exists := npw.serviceInfo[index]; !exists {
		// Only set this if it doesn't exist
		npw.serviceInfo[index] = &serviceConfig{service: service, hasLocalHostNetworkEp: hasLocalHostNetworkEp, localEndpoints: localEndpoints}
		return false
	}
	return true

}

// updateServiceInfo sets the serviceConfig for a service and returns the existing serviceConfig, if inputs are nil
// do not update those fields, if it does not exist return nil.
func (npw *nodePortWatcher) updateServiceInfo(index ktypes.NamespacedName, service *kapi.Service, hasLocalHostNetworkEp *bool, localEndpoints sets.Set[string]) (old *serviceConfig, exists bool) {

	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()

	if old, exists = npw.serviceInfo[index]; !exists {
		klog.V(5).Infof("No serviceConfig found for service %s in namespace %s", index.Name, index.Namespace)
		return nil, exists
	}
	ptrCopy := *old
	if service != nil {
		npw.serviceInfo[index].service = service
	}

	if hasLocalHostNetworkEp != nil {
		npw.serviceInfo[index].hasLocalHostNetworkEp = *hasLocalHostNetworkEp
	}

	if localEndpoints != nil {
		npw.serviceInfo[index].localEndpoints = localEndpoints
	}

	return &ptrCopy, exists
}

// addServiceRules ensures the correct iptables rules and OpenFlow physical
// flows are programmed for a given service and endpoint configuration
func addServiceRules(service *kapi.Service, localEndpoints []string, svcHasLocalHostNetEndPnt bool, npw *nodePortWatcher) error {
	// For dpu or Full mode
	var err error
	var errors []error
	if npw != nil {
		if err = npw.updateServiceFlowCache(service, true, svcHasLocalHostNetEndPnt); err != nil {
			errors = append(errors, err)
		}
		npw.ofm.requestFlowSync()
		if !npw.dpuMode {
			// add iptable rules only in full mode
			if err = addGatewayIptRules(service, localEndpoints, svcHasLocalHostNetEndPnt); err != nil {
				errors = append(errors, err)
			}
		}
	} else {
		// For Host Only Mode
		if err = addGatewayIptRules(service, localEndpoints, svcHasLocalHostNetEndPnt); err != nil {
			errors = append(errors, err)
		}

	}
	return apierrors.NewAggregate(errors)

}

// delServiceRules deletes all possible iptables rules and OpenFlow physical
// flows for a service
func delServiceRules(service *kapi.Service, localEndpoints []string, npw *nodePortWatcher) error {
	var err error
	var errors []error
	// full mode || dpu mode
	if npw != nil {
		if err = npw.updateServiceFlowCache(service, false, false); err != nil {
			errors = append(errors, fmt.Errorf("error updating service flow cache: %v", err))
		}
		npw.ofm.requestFlowSync()
		if !npw.dpuMode {
			// Always try and delete all rules here in full mode & in host only mode. We don't touch iptables in dpu mode.
			// +--------------------------+-----------------------+-----------------------+--------------------------------+
			// | svcHasLocalHostNetEndPnt | ExternalTrafficPolicy | InternalTrafficPolicy |     Scenario for deletion      |
			// |--------------------------|-----------------------|-----------------------|--------------------------------|
			// |                          |                       |                       |      deletes the MARK          |
			// |         false            |         cluster       |          local        |      rules for itp=local       |
			// |                          |                       |                       |       called from mangle       |
			// |--------------------------|-----------------------|-----------------------|--------------------------------|
			// |                          |                       |                       |      deletes the REDIRECT      |
			// |         true             |         cluster       |          local        |      rules towards target      |
			// |                          |                       |                       |       port for itp=local       |
			// |--------------------------|-----------------------|-----------------------|--------------------------------|
			// |                          |                       |                       | deletes the DNAT rules for     |
			// |         false            |          local        |          cluster      |    non-local-host-net          |
			// |                          |                       |                       | eps towards masqueradeIP +     |
			// |                          |                       |                       | DNAT rules towards clusterIP   |
			// |--------------------------|-----------------------|-----------------------|--------------------------------|
			// |                          |                       |                       |    deletes the DNAT rules      |
			// |       false||true        |          cluster      |          cluster      |   	towards clusterIP          |
			// |                          |                       |                       |       for the default case     |
			// |--------------------------|-----------------------|-----------------------|--------------------------------|
			// |                          |                       |                       |      deletes all the rules     |
			// |       false||true        |          local        |          local        |   for etp=local + itp=local    |
			// |                          |                       |                       |   + default dnat towards CIP   |
			// +--------------------------+-----------------------+-----------------------+--------------------------------+

			if err = delGatewayIptRules(service, localEndpoints, true); err != nil {
				errors = append(errors, fmt.Errorf("error updating service flow cache: %v", err))
			}
			if err = delGatewayIptRules(service, localEndpoints, false); err != nil {
				errors = append(errors, fmt.Errorf("error updating service flow cache: %v", err))
			}
		}
	} else {

		if err = delGatewayIptRules(service, localEndpoints, true); err != nil {
			errors = append(errors, fmt.Errorf("error updating service flow cache: %v", err))
		}
		if err = delGatewayIptRules(service, localEndpoints, false); err != nil {
			errors = append(errors, fmt.Errorf("error updating service flow cache: %v", err))
		}
	}
	return apierrors.NewAggregate(errors)
}

func serviceUpdateNotNeeded(old, new *kapi.Service) bool {
	return reflect.DeepEqual(new.Spec.Ports, old.Spec.Ports) &&
		reflect.DeepEqual(new.Spec.ExternalIPs, old.Spec.ExternalIPs) &&
		reflect.DeepEqual(new.Spec.ClusterIP, old.Spec.ClusterIP) &&
		reflect.DeepEqual(new.Spec.ClusterIPs, old.Spec.ClusterIPs) &&
		reflect.DeepEqual(new.Spec.Type, old.Spec.Type) &&
		reflect.DeepEqual(new.Status.LoadBalancer.Ingress, old.Status.LoadBalancer.Ingress) &&
		reflect.DeepEqual(new.Spec.ExternalTrafficPolicy, old.Spec.ExternalTrafficPolicy) &&
		(new.Spec.InternalTrafficPolicy != nil && old.Spec.InternalTrafficPolicy != nil &&
			reflect.DeepEqual(*new.Spec.InternalTrafficPolicy, *old.Spec.InternalTrafficPolicy)) &&
		(new.Spec.AllocateLoadBalancerNodePorts != nil && old.Spec.AllocateLoadBalancerNodePorts != nil &&
			reflect.DeepEqual(*new.Spec.AllocateLoadBalancerNodePorts, *old.Spec.AllocateLoadBalancerNodePorts))
}

// AddService handles configuring shared gateway bridge flows to steer External IP, Node Port, Ingress LB traffic into OVN
func (npw *nodePortWatcher) AddService(service *kapi.Service) error {
	var localEndpoints sets.Set[string]
	var hasLocalHostNetworkEp bool
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return nil
	}

	klog.V(5).Infof("Adding service %s in namespace %s", service.Name, service.Namespace)
	name := ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	epSlices, err := npw.watchFactory.GetEndpointSlices(service.Namespace, service.Name)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return fmt.Errorf("error retrieving all endpointslices for service %s/%s during service add: %w",
				service.Namespace, service.Name, err)
		}
		klog.V(5).Infof("No endpointslice found for service %s in namespace %s during service Add",
			service.Name, service.Namespace)
		// No endpoint object exists yet so default to false
		hasLocalHostNetworkEp = false
	} else {
		nodeIPs := npw.nodeIPManager.ListAddresses()
		localEndpoints = npw.GetLocalEndpointAddresses(epSlices, service)
		hasLocalHostNetworkEp = util.HasLocalHostNetworkEndpoints(localEndpoints, nodeIPs)
	}
	// If something didn't already do it add correct Service rules
	if exists := npw.addOrSetServiceInfo(name, service, hasLocalHostNetworkEp, localEndpoints); !exists {
		klog.V(5).Infof("Service Add %s event in namespace %s came before endpoint event setting svcConfig",
			service.Name, service.Namespace)
		if err := addServiceRules(service, sets.List(localEndpoints), hasLocalHostNetworkEp, npw); err != nil {
			return fmt.Errorf("AddService failed for nodePortWatcher: %v", err)
		}
	} else {
		klog.V(5).Infof("Rules already programmed for %s in namespace %s", service.Name, service.Namespace)
	}
	return nil
}

func (npw *nodePortWatcher) UpdateService(old, new *kapi.Service) error {
	var err error
	var errors []error
	name := ktypes.NamespacedName{Namespace: old.Namespace, Name: old.Name}

	if serviceUpdateNotNeeded(old, new) {
		klog.V(5).Infof("Skipping service update for: %s as change does not apply to any of .Spec.Ports, "+
			".Spec.ExternalIP, .Spec.ClusterIP, .Spec.ClusterIPs, .Spec.Type, .Status.LoadBalancer.Ingress, "+
			".Spec.ExternalTrafficPolicy, .Spec.InternalTrafficPolicy", new.Name)
		return nil
	}
	// Update the service in svcConfig if we need to so that other handler
	// threads do the correct thing, leave hasLocalHostNetworkEp and localEndpoints alone in the cache
	svcConfig, exists := npw.updateServiceInfo(name, new, nil, nil)
	if !exists {
		klog.V(5).Infof("Service %s in namespace %s was deleted during service Update", old.Name, old.Namespace)
		return nil
	}

	if util.ServiceTypeHasClusterIP(old) && util.IsClusterIPSet(old) {
		// Delete old rules if needed, but don't delete svcConfig
		// so that we don't miss any endpoint update events here
		klog.V(5).Infof("Deleting old service rules for: %v", old)
		if err = delServiceRules(old, sets.List(svcConfig.localEndpoints), npw); err != nil {
			errors = append(errors, err)
		}
	}

	if util.ServiceTypeHasClusterIP(new) && util.IsClusterIPSet(new) {
		klog.V(5).Infof("Adding new service rules for: %v", new)
		if err = addServiceRules(new, sets.List(svcConfig.localEndpoints), svcConfig.hasLocalHostNetworkEp, npw); err != nil {
			errors = append(errors, err)
		}
	}
	if err = apierrors.NewAggregate(errors); err != nil {
		return fmt.Errorf("UpdateService failed for nodePortWatcher: %v", err)
	}
	return nil

}

// deleteConntrackForServiceVIP deletes the conntrack entries for the provided svcVIP:svcPort by comparing them to ConntrackOrigDstIP:ConntrackOrigDstPort
func deleteConntrackForServiceVIP(svcVIPs []string, svcPorts []kapi.ServicePort, ns, name string) error {
	for _, svcVIP := range svcVIPs {
		for _, svcPort := range svcPorts {
			if err := util.DeleteConntrackServicePort(svcVIP, svcPort.Port, svcPort.Protocol,
				netlink.ConntrackOrigDstIP, nil); err != nil {
				return fmt.Errorf("failed to delete conntrack entry for service %s/%s with svcVIP %s, svcPort %d, protocol %s: %v",
					ns, name, svcVIP, svcPort.Port, svcPort.Protocol, err)
			}
		}
	}
	return nil
}

// deleteConntrackForService deletes the conntrack entries corresponding to the service VIPs of the provided service
func (npw *nodePortWatcher) deleteConntrackForService(service *kapi.Service) error {
	// remove conntrack entries for LB VIPs and External IPs
	externalIPs := util.GetExternalAndLBIPs(service)
	if err := deleteConntrackForServiceVIP(externalIPs, service.Spec.Ports, service.Namespace, service.Name); err != nil {
		return err
	}
	if util.ServiceTypeHasNodePort(service) {
		// remove conntrack entries for NodePorts
		nodeIPs := npw.nodeIPManager.ListAddresses()
		for _, nodeIP := range nodeIPs {
			for _, svcPort := range service.Spec.Ports {
				if err := util.DeleteConntrackServicePort(nodeIP.String(), svcPort.NodePort, svcPort.Protocol,
					netlink.ConntrackOrigDstIP, nil); err != nil {
					return fmt.Errorf("failed to delete conntrack entry for service %s/%s with nodeIP %s, nodePort %d, protocol %s: %v",
						service.Namespace, service.Name, nodeIP, svcPort.Port, svcPort.Protocol, err)
				}
			}
		}
	}
	// remove conntrack entries for ClusterIPs
	clusterIPs := util.GetClusterIPs(service)
	if err := deleteConntrackForServiceVIP(clusterIPs, service.Spec.Ports, service.Namespace, service.Name); err != nil {
		return err
	}
	return nil
}

func (npw *nodePortWatcher) DeleteService(service *kapi.Service) error {
	var err error
	var errors []error
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return nil
	}

	klog.V(5).Infof("Deleting service %s in namespace %s", service.Name, service.Namespace)
	name := ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	if svcConfig, exists := npw.getAndDeleteServiceInfo(name); exists {
		if err = delServiceRules(svcConfig.service, sets.List(svcConfig.localEndpoints), npw); err != nil {
			errors = append(errors, err)
		}
	} else {
		klog.Warningf("Delete service: no service found in cache for endpoint %s in namespace %s", service.Name, service.Namespace)
	}
	// Remove all conntrack entries for the serviceVIPs of this service irrespective of protocol stack
	// since service deletion is considered as unplugging the network cable and hence graceful termination
	// is not guaranteed. See https://github.com/kubernetes/kubernetes/issues/108523#issuecomment-1074044415.
	if err = npw.deleteConntrackForService(service); err != nil {
		errors = append(errors, fmt.Errorf("failed to delete conntrack entry for service %v: %v", name, err))
	}

	if err = apierrors.NewAggregate(errors); err != nil {
		return fmt.Errorf("DeleteService failed for nodePortWatcher: %v", err)
	}
	return nil

}

func (npw *nodePortWatcher) SyncServices(services []interface{}) error {
	var err error
	var errors []error
	keepIPTRules := []nodeipt.Rule{}
	for _, serviceInterface := range services {
		name := ktypes.NamespacedName{Namespace: serviceInterface.(*kapi.Service).Namespace, Name: serviceInterface.(*kapi.Service).Name}

		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}

		epSlices, err := npw.watchFactory.GetEndpointSlices(service.Namespace, service.Name)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				return fmt.Errorf("error retrieving all endpointslices for service %s/%s during SyncServices: %w",
					service.Namespace, service.Name, err)
			}
			klog.V(5).Infof("No endpointslice found for service %s in namespace %s during sync", service.Name, service.Namespace)
			continue
		}
		nodeIPs := npw.nodeIPManager.ListAddresses()
		localEndpoints := npw.GetLocalEndpointAddresses(epSlices, service)
		hasLocalHostNetworkEp := util.HasLocalHostNetworkEndpoints(localEndpoints, nodeIPs)
		npw.getAndSetServiceInfo(name, service, hasLocalHostNetworkEp, localEndpoints)

		// Delete OF rules for service if they exist
		if err = npw.updateServiceFlowCache(service, false, hasLocalHostNetworkEp); err != nil {
			errors = append(errors, err)
		}
		if err = npw.updateServiceFlowCache(service, true, hasLocalHostNetworkEp); err != nil {
			errors = append(errors, err)
		}
		// Add correct iptables rules only for Full mode
		if !npw.dpuMode {
			keepIPTRules = append(keepIPTRules, getGatewayIPTRules(service, sets.List(localEndpoints), hasLocalHostNetworkEp)...)
		}
	}

	// sync OF rules once
	npw.ofm.requestFlowSync()
	// sync IPtables rules once only for Full mode
	if !npw.dpuMode {
		// (NOTE: Order is important, add jump to iptableETPChain before jump to NP/EIP chains)
		for _, chain := range []string{iptableITPChain, egressservice.Chain, iptableNodePortChain, iptableExternalIPChain, iptableETPChain, iptableMgmPortChain} {
			if err = recreateIPTRules("nat", chain, keepIPTRules); err != nil {
				errors = append(errors, err)
			}
		}
		if err = recreateIPTRules("mangle", iptableITPChain, keepIPTRules); err != nil {
			errors = append(errors, err)
		}
	}
	return apierrors.NewAggregate(errors)
}

func (npw *nodePortWatcher) AddEndpointSlice(epSlice *discovery.EndpointSlice) error {
	var err error
	var errors []error
	var svc *kapi.Service

	svcName := epSlice.Labels[discovery.LabelServiceName]
	svc, err = npw.watchFactory.GetService(epSlice.Namespace, svcName)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return fmt.Errorf("error retrieving service %s/%s during endpointslice add: %w",
				epSlice.Namespace, svcName, err)
		}
		// This is not necessarily an error. For e.g when there are endpoints
		// without a corresponding service.
		klog.V(5).Infof("No service found for endpointslice %s in namespace %s during endpointslice add",
			epSlice.Name, epSlice.Namespace)
		return nil
	}

	if !util.ServiceTypeHasClusterIP(svc) || !util.IsClusterIPSet(svc) {
		return nil
	}

	klog.V(5).Infof("Adding endpointslice %s in namespace %s", epSlice.Name, epSlice.Namespace)
	nodeIPs := npw.nodeIPManager.ListAddresses()
	epSlices, err := npw.watchFactory.GetEndpointSlices(svc.Namespace, svc.Name)
	if err != nil {
		// No need to continue adding the new endpoint slice, if we can't retrieve all slices for this service
		return fmt.Errorf("error retrieving endpointslices for service %s/%s during endpointslice add: %w", svc.Namespace, svc.Name, err)
	}
	localEndpoints := npw.GetLocalEndpointAddresses(epSlices, svc)
	hasLocalHostNetworkEp := util.HasLocalHostNetworkEndpoints(localEndpoints, nodeIPs)

	// Here we make sure the correct rules are programmed whenever an AddEndpointSlice event is
	// received, only alter flows if we need to, i.e if cache wasn't set or if it was and
	// hasLocalHostNetworkEp or localEndpoints state (for LB svc where NPs=0) changed, to prevent flow churn
	namespacedName, err := util.ServiceNamespacedNameFromEndpointSlice(epSlice)
	if err != nil {
		return fmt.Errorf("cannot add %s/%s to nodePortWatcher: %v", epSlice.Namespace, epSlice.Name, err)
	}
	out, exists := npw.getAndSetServiceInfo(namespacedName, svc, hasLocalHostNetworkEp, localEndpoints)
	if !exists {
		klog.V(5).Infof("Endpointslice %s ADD event in namespace %s is creating rules", epSlice.Name, epSlice.Namespace)
		return addServiceRules(svc, sets.List(localEndpoints), hasLocalHostNetworkEp, npw)
	}

	if out.hasLocalHostNetworkEp != hasLocalHostNetworkEp ||
		(!util.LoadBalancerServiceHasNodePortAllocation(svc) && !reflect.DeepEqual(out.localEndpoints, localEndpoints)) {
		klog.V(5).Infof("Endpointslice %s ADD event in namespace %s is updating rules", epSlice.Name, epSlice.Namespace)
		if err = delServiceRules(svc, sets.List(out.localEndpoints), npw); err != nil {
			errors = append(errors, err)
		}
		if err = addServiceRules(svc, sets.List(localEndpoints), hasLocalHostNetworkEp, npw); err != nil {
			errors = append(errors, err)
		}
		return apierrors.NewAggregate(errors)
	}
	return nil

}

func (npw *nodePortWatcher) DeleteEndpointSlice(epSlice *discovery.EndpointSlice) error {
	var err error
	var errors []error
	var hasLocalHostNetworkEp = false

	klog.V(5).Infof("Deleting endpointslice %s in namespace %s", epSlice.Name, epSlice.Namespace)
	// remove rules for endpoints and add back normal ones
	namespacedName, err := util.ServiceNamespacedNameFromEndpointSlice(epSlice)
	if err != nil {
		return fmt.Errorf("cannot delete %s/%s from nodePortWatcher: %v", epSlice.Namespace, epSlice.Name, err)
	}
	epSlices, err := npw.watchFactory.GetEndpointSlices(epSlice.Namespace, epSlice.Labels[discovery.LabelServiceName])
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return fmt.Errorf("error retrieving all endpointslices for service %s/%s during endpointslice delete on %s: %w",
				namespacedName.Namespace, namespacedName.Name, epSlice.Name, err)
		}
		// an endpoint slice that we retry to delete will be gone from the api server, so don't return here
		klog.V(5).Infof("No endpointslices found for service %s/%s during endpointslice delete on %s (did we previously fail to delete it?)",
			namespacedName.Namespace, namespacedName.Name, epSlice.Name)
		epSlices = []*discovery.EndpointSlice{epSlice}
	}

	svc, err := npw.watchFactory.GetService(namespacedName.Namespace, namespacedName.Name)
	if err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("error retrieving service %s/%s for endpointslice %s during endpointslice delete: %v",
			namespacedName.Namespace, namespacedName.Name, epSlice.Name, err)
	}
	localEndpoints := npw.GetLocalEndpointAddresses(epSlices, svc)
	if svcConfig, exists := npw.updateServiceInfo(namespacedName, nil, &hasLocalHostNetworkEp, localEndpoints); exists {
		// Lock the cache mutex here so we don't miss a service delete during an endpoint delete
		// we have to do this because deleting and adding iptables rules is slow.
		npw.serviceInfoLock.Lock()
		defer npw.serviceInfoLock.Unlock()

		if err = delServiceRules(svcConfig.service, sets.List(svcConfig.localEndpoints), npw); err != nil {
			errors = append(errors, err)
		}
		if err = addServiceRules(svcConfig.service, sets.List(localEndpoints), hasLocalHostNetworkEp, npw); err != nil {
			errors = append(errors, err)
		}
		return apierrors.NewAggregate(errors)
	}
	return nil
}

// GetLocalEndpointAddresses returns a list of eligible endpoints that are local to the node
func (npw *nodePortWatcher) GetLocalEndpointAddresses(endpointSlices []*discovery.EndpointSlice, service *kapi.Service) sets.Set[string] {
	return util.GetLocalEndpointAddresses(endpointSlices, service, npw.nodeIPManager.nodeName)
}

func (npw *nodePortWatcher) UpdateEndpointSlice(oldEpSlice, newEpSlice *discovery.EndpointSlice) error {
	// TODO (tssurya): refactor bits in this function to ensure add and delete endpoint slices are not called repeatedly
	// Context: Both add and delete endpointslice are calling delServiceRules followed by addServiceRules which makes double
	// the number of calls than needed for an update endpoint slice
	var err error
	var errors []error

	namespacedName, err := util.ServiceNamespacedNameFromEndpointSlice(newEpSlice)
	if err != nil {
		return fmt.Errorf("cannot update %s/%s in nodePortWatcher: %v", newEpSlice.Namespace, newEpSlice.Name, err)
	}
	svc, err := npw.watchFactory.GetService(namespacedName.Namespace, namespacedName.Name)
	if err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("error retrieving service %s/%s for endpointslice %s during endpointslice update: %v",
			namespacedName.Namespace, namespacedName.Name, newEpSlice.Name, err)
	}

	oldEndpointAddresses := util.GetEndpointAddresses([]*discovery.EndpointSlice{oldEpSlice}, svc)
	newEndpointAddresses := util.GetEndpointAddresses([]*discovery.EndpointSlice{newEpSlice}, svc)
	if reflect.DeepEqual(oldEndpointAddresses, newEndpointAddresses) {
		return nil
	}

	klog.V(5).Infof("Updating endpointslice %s in namespace %s", oldEpSlice.Name, oldEpSlice.Namespace)

	var serviceInfo *serviceConfig
	var exists bool
	if serviceInfo, exists = npw.getServiceInfo(namespacedName); !exists {
		// When a service is updated from externalName to nodeport type, it won't be
		// in nodePortWatcher cache (npw): in this case, have the new nodeport IPtable rules
		// installed.
		if err = npw.AddEndpointSlice(newEpSlice); err != nil {
			errors = append(errors, err)
		}
	} else if len(newEndpointAddresses) == 0 {
		// With no endpoint addresses in new endpointslice, delete old endpoint rules
		// and add normal ones back
		if err = npw.DeleteEndpointSlice(oldEpSlice); err != nil {
			errors = append(errors, err)
		}
	}

	// Update rules and service cache if hasHostNetworkEndpoints status changed or localEndpoints changed
	nodeIPs := npw.nodeIPManager.ListAddresses()
	epSlices, err := npw.watchFactory.GetEndpointSlices(newEpSlice.Namespace, newEpSlice.Labels[discovery.LabelServiceName])
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return fmt.Errorf("error retrieving all endpointslices for service %s/%s during endpointslice update on %s: %w",
				namespacedName.Namespace, namespacedName.Name, newEpSlice.Name, err)
		}
		klog.V(5).Infof("No endpointslices found for service %s/%s during endpointslice update on %s: %v",
			namespacedName.Namespace, namespacedName.Name, newEpSlice.Name, err)
	}

	// Delete old endpoint slice and add new one when local endpoints have changed or the presence of local host-network
	// endpoints has changed. For this second comparison, check first between the old endpoint slice and all current
	// endpointslices for this service. This is a partial comparison, in case serviceInfo is not set. When it is set, compare
	// between /all/ old endpoint slices and all new ones.
	oldLocalEndpoints := npw.GetLocalEndpointAddresses([]*discovery.EndpointSlice{oldEpSlice}, svc)
	newLocalEndpoints := npw.GetLocalEndpointAddresses(epSlices, svc)
	hasLocalHostNetworkEpOld := util.HasLocalHostNetworkEndpoints(oldLocalEndpoints, nodeIPs)
	hasLocalHostNetworkEpNew := util.HasLocalHostNetworkEndpoints(newLocalEndpoints, nodeIPs)

	localEndpointsHaveChanged := serviceInfo != nil && !reflect.DeepEqual(serviceInfo.localEndpoints, newLocalEndpoints)
	localHostNetworkEndpointsPresenceHasChanged := hasLocalHostNetworkEpOld != hasLocalHostNetworkEpNew ||
		serviceInfo != nil && serviceInfo.hasLocalHostNetworkEp != hasLocalHostNetworkEpNew

	if localEndpointsHaveChanged || localHostNetworkEndpointsPresenceHasChanged {
		if err = npw.DeleteEndpointSlice(oldEpSlice); err != nil {
			errors = append(errors, err)
		}
		if err = npw.AddEndpointSlice(newEpSlice); err != nil {
			errors = append(errors, err)
		}
		return apierrors.NewAggregate(errors)
	}

	return apierrors.NewAggregate(errors)
}

func (npwipt *nodePortWatcherIptables) AddService(service *kapi.Service) error {
	// don't process headless service or services that doesn't have NodePorts or ExternalIPs
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return nil
	}
	if err := addServiceRules(service, nil, false, nil); err != nil {
		return fmt.Errorf("AddService failed for nodePortWatcherIptables: %v", err)
	}
	return nil
}

func (npwipt *nodePortWatcherIptables) UpdateService(old, new *kapi.Service) error {
	var err error
	var errors []error
	if serviceUpdateNotNeeded(old, new) {
		klog.V(5).Infof("Skipping service update for: %s as change does not apply to "+
			"any of .Spec.Ports, .Spec.ExternalIP, .Spec.ClusterIP, .Spec.ClusterIPs,"+
			" .Spec.Type, .Status.LoadBalancer.Ingress", new.Name)
		return nil
	}

	if util.ServiceTypeHasClusterIP(old) && util.IsClusterIPSet(old) {
		if err = delServiceRules(old, nil, nil); err != nil {
			errors = append(errors, err)
		}
	}

	if util.ServiceTypeHasClusterIP(new) && util.IsClusterIPSet(new) {
		if err = addServiceRules(new, nil, false, nil); err != nil {
			errors = append(errors, err)
		}
	}
	if err = apierrors.NewAggregate(errors); err != nil {
		return fmt.Errorf("UpdateService failed for nodePortWatcherIptables: %v", err)
	}
	return nil

}

func (npwipt *nodePortWatcherIptables) DeleteService(service *kapi.Service) error {
	// don't process headless service
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return nil
	}
	if err := delServiceRules(service, nil, nil); err != nil {
		return fmt.Errorf("DeleteService failed for nodePortWatcherIptables: %v", err)
	}
	return nil
}

func (npwipt *nodePortWatcherIptables) SyncServices(services []interface{}) error {
	var err error
	var errors []error
	keepIPTRules := []nodeipt.Rule{}
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}
		// Add correct iptables rules.
		// TODO: ETP and ITP is not implemented for smart NIC mode.
		keepIPTRules = append(keepIPTRules, getGatewayIPTRules(service, nil, false)...)
	}

	// sync IPtables rules once
	for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain} {
		if err = recreateIPTRules("nat", chain, keepIPTRules); err != nil {
			errors = append(errors, err)
		}
	}
	return apierrors.NewAggregate(errors)
}

// since we share the host's k8s node IP, add OpenFlow flows
// -- to steer the NodePort traffic arriving on the host to the OVN logical topology and
// -- to also connection track the outbound north-south traffic through l3 gateway so that
//
//	the return traffic can be steered back to OVN logical topology
//
// -- to handle host -> service access, via masquerading from the host to OVN GR
// -- to handle external -> service(ExternalTrafficPolicy: Local) -> host access without SNAT
func newGatewayOpenFlowManager(gwBridge, exGWBridge *bridgeConfiguration, subnets []*net.IPNet, extraIPs []net.IP) (*openflowManager, error) {
	// add health check function to check default OpenFlow flows are on the shared gateway bridge
	ofm := &openflowManager{
		defaultBridge:         gwBridge,
		externalGatewayBridge: exGWBridge,
		flowCache:             make(map[string][]string),
		flowMutex:             sync.Mutex{},
		exGWFlowCache:         make(map[string][]string),
		exGWFlowMutex:         sync.Mutex{},
		flowChan:              make(chan struct{}, 1),
	}

	if err := ofm.updateBridgeFlowCache(subnets, extraIPs); err != nil {
		return nil, err
	}

	// defer flowSync until syncService() to prevent the existing service OpenFlows being deleted
	return ofm, nil
}

// updateBridgeFlowCache generates the "static" per-bridge flows
// note: this is shared between shared and local gateway modes
func (ofm *openflowManager) updateBridgeFlowCache(subnets []*net.IPNet, extraIPs []net.IP) error {
	// protect defaultBridge config from being updated by gw.nodeIPManager
	ofm.defaultBridge.Lock()
	defer ofm.defaultBridge.Unlock()

	dftFlows, err := flowsForDefaultBridge(ofm.defaultBridge, extraIPs)
	if err != nil {
		return err
	}
	dftCommonFlows, err := commonFlows(subnets, ofm.defaultBridge)
	if err != nil {
		return err
	}
	dftFlows = append(dftFlows, dftCommonFlows...)

	ofm.updateFlowCacheEntry("NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
	ofm.updateFlowCacheEntry("DEFAULT", dftFlows)

	// we consume ex gw bridge flows only if that is enabled
	if ofm.externalGatewayBridge != nil {
		ofm.updateExBridgeFlowCacheEntry("NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
		exGWBridgeDftFlows, err := commonFlows(subnets, ofm.externalGatewayBridge)
		if err != nil {
			return err
		}
		ofm.updateExBridgeFlowCacheEntry("DEFAULT", exGWBridgeDftFlows)
	}
	return nil
}

func flowsForDefaultBridge(bridge *bridgeConfiguration, extraIPs []net.IP) ([]string, error) {
	ofPortPhys := bridge.ofPortPhys
	bridgeMacAddress := bridge.macAddress.String()
	ofPortPatch := bridge.ofPortPatch
	ofPortHost := bridge.ofPortHost
	bridgeIPs := bridge.ips

	var dftFlows []string

	if config.IPv4Mode {
		// table0, Geneve packets coming from external. Skip conntrack and go directly to host
		// if dest mac is the shared mac send directly to host.
		if ofPortPhys != "" {
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=205, in_port=%s, dl_dst=%s, udp, udp_dst=%d, "+
					"actions=output:%s", defaultOpenFlowCookie, ofPortPhys, bridgeMacAddress, config.Default.EncapPort,
					ofPortHost))
			// perform NORMAL action otherwise.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp, udp_dst=%d, "+
					"actions=NORMAL", defaultOpenFlowCookie, ofPortPhys, config.Default.EncapPort))

			// table0, Geneve packets coming from LOCAL. Skip conntrack and go directly to external
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp, udp_dst=%d, "+
					"actions=output:%s", defaultOpenFlowCookie, ovsLocalPort, config.Default.EncapPort, ofPortPhys))
		}
		physicalIP, err := util.MatchFirstIPNetFamily(false, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv4 physical IP of host: %v", err)
		}
		// table 0, SVC Hairpin from OVN destined to local host, DNAT and go to table 4
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ip, ip_dst=%s, ip_src=%s,"+
				"actions=ct(commit,zone=%d,nat(dst=%s),table=4)",
				defaultOpenFlowCookie, ofPortPatch, types.V4HostMasqueradeIP, physicalIP.IP,
				HostMasqCTZone, physicalIP.IP))

		// table 0, hairpin from OVN destined to local host (but an additional node IP), send to table 4
		for _, ip := range extraIPs {
			if ip.To4() == nil {
				continue
			}
			// not needed for the physical IP
			if ip.Equal(physicalIP.IP) {
				continue
			}

			// not needed for special masquerade IP
			if ip.Equal(net.ParseIP(types.V4HostMasqueradeIP)) {
				continue
			}

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ip, ip_dst=%s, ip_src=%s,"+
					"actions=ct(commit,zone=%d,table=4)",
					defaultOpenFlowCookie, ofPortPatch, ip.String(), physicalIP.IP,
					HostMasqCTZone))
		}

		// table 0, Reply SVC traffic from Host -> OVN, unSNAT and goto table 5
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ip, ip_dst=%s,"+
				"actions=ct(zone=%d,nat,table=5)",
				defaultOpenFlowCookie, ofPortHost, types.V4OVNMasqueradeIP, OVNMasqCTZone))
	}
	if config.IPv6Mode {
		if ofPortPhys != "" {
			// table0, Geneve packets coming from external. Skip conntrack and go directly to host
			// if dest mac is the shared mac send directly to host.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=205, in_port=%s, dl_dst=%s, udp6, udp_dst=%d, "+
					"actions=output:%s", defaultOpenFlowCookie, ofPortPhys, bridgeMacAddress, config.Default.EncapPort,
					ofPortHost))
			// perform NORMAL action otherwise.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp6, udp_dst=%d, "+
					"actions=NORMAL", defaultOpenFlowCookie, ofPortPhys, config.Default.EncapPort))

			// table0, Geneve packets coming from LOCAL. Skip conntrack and send to external
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp6, udp_dst=%d, "+
					"actions=output:%s", defaultOpenFlowCookie, ovsLocalPort, config.Default.EncapPort, ofPortPhys))
		}

		physicalIP, err := util.MatchFirstIPNetFamily(true, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv6 physical IP of host: %v", err)
		}
		// table 0, SVC Hairpin from OVN destined to local host, DNAT to host, send to table 4
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ipv6, ipv6_dst=%s, ipv6_src=%s,"+
				"actions=ct(commit,zone=%d,nat(dst=%s),table=4)",
				defaultOpenFlowCookie, ofPortPatch, types.V6HostMasqueradeIP, physicalIP.IP,
				HostMasqCTZone, physicalIP.IP))

		// table 0, hairpin from OVN destined to local host (but an additional node IP), send to table 4
		for _, ip := range extraIPs {
			if ip.To4() != nil {
				continue
			}
			// not needed for the physical IP
			if ip.Equal(physicalIP.IP) {
				continue
			}

			// not needed for special masquerade IP
			if ip.Equal(net.ParseIP(types.V6HostMasqueradeIP)) {
				continue
			}

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ipv6, ipv6_dst=%s, ipv6_src=%s,"+
					"actions=ct(commit,zone=%d,table=4)",
					defaultOpenFlowCookie, ofPortPatch, ip.String(), physicalIP.IP,
					HostMasqCTZone))
		}

		// table 0, Reply SVC traffic from Host -> OVN, unSNAT and goto table 5
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ipv6, ipv6_dst=%s,"+
				"actions=ct(zone=%d,nat,table=5)",
				defaultOpenFlowCookie, ofPortHost, types.V6OVNMasqueradeIP, OVNMasqCTZone))
	}

	var protoPrefix string
	var masqIP string

	// table 0, packets coming from Host -> Service
	for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
		if utilnet.IsIPv4CIDR(svcCIDR) {
			protoPrefix = "ip"
			masqIP = types.V4HostMasqueradeIP
		} else {
			protoPrefix = "ipv6"
			masqIP = types.V6HostMasqueradeIP
		}

		// table 0, Host -> OVN towards SVC, SNAT to special IP
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_dst=%s,"+
				"actions=ct(commit,zone=%d,nat(src=%s),table=2)",
				defaultOpenFlowCookie, ofPortHost, protoPrefix, protoPrefix, svcCIDR, HostMasqCTZone, masqIP))

		// table 0, Reply hairpin traffic to host, coming from OVN, unSNAT
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_src=%s, %s_dst=%s,"+
				"actions=ct(zone=%d,nat,table=3)",
				defaultOpenFlowCookie, ofPortPatch, protoPrefix, protoPrefix, svcCIDR,
				protoPrefix, masqIP, HostMasqCTZone))

		// table 0, Reply traffic coming from OVN to outside, drop it if the DNAT wasn't done either
		// at the GR load balancer or switch load balancer. It means the correct port wasn't provided.
		// nodeCIDR->serviceCIDR traffic flow is internal and it shouldn't be carried to outside the cluster
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=105, in_port=%s, %s, %s_dst=%s,"+
				"actions=drop", defaultOpenFlowCookie, ofPortPatch, protoPrefix, protoPrefix, svcCIDR))
	}

	actions := fmt.Sprintf("output:%s", ofPortPatch)

	if ofPortPhys != "" {
		if config.IPv4Mode {
			// table 1, established and related connections in zone 64000 with ct_mark ctMarkOVN go to OVN
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+est, ct_mark=%s, "+
					"actions=%s",
					defaultOpenFlowCookie, ctMarkOVN, actions))

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+rel, ct_mark=%s, "+
					"actions=%s",
					defaultOpenFlowCookie, ctMarkOVN, actions))

			// table 1, established and related connections in zone 64000 with ct_mark ctMarkHost go to host
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+est, ct_mark=%s, "+
					"actions=output:%s",
					defaultOpenFlowCookie, ctMarkHost, ofPortHost))

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+rel, ct_mark=%s, "+
					"actions=output:%s",
					defaultOpenFlowCookie, ctMarkHost, ofPortHost))
		}

		if config.IPv6Mode {
			// table 1, established and related connections in zone 64000 with ct_mark ctMarkOVN go to OVN
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, ipv6, ct_state=+trk+est, ct_mark=%s, "+
					"actions=%s",
					defaultOpenFlowCookie, ctMarkOVN, actions))

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, ipv6, ct_state=+trk+rel, ct_mark=%s, "+
					"actions=%s",
					defaultOpenFlowCookie, ctMarkOVN, actions))

			// table 1, established and related connections in zone 64000 with ct_mark ctMarkHost go to host
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, ip6, ct_state=+trk+est, ct_mark=%s, "+
					"actions=output:%s",
					defaultOpenFlowCookie, ctMarkHost, ofPortHost))

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, table=1, ip6, ct_state=+trk+rel, ct_mark=%s, "+
					"actions=output:%s",
					defaultOpenFlowCookie, ctMarkHost, ofPortHost))
		}

		// table 1, we check to see if this dest mac is the shared mac, if so send to host
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=10, table=1, dl_dst=%s, actions=output:%s",
				defaultOpenFlowCookie, bridgeMacAddress, ofPortHost))
	}

	// table 2, dispatch from Host -> OVN
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, table=2, "+
			"actions=mod_dl_dst=%s,output:%s", defaultOpenFlowCookie, bridgeMacAddress, ofPortPatch))

	// table 3, dispatch from OVN -> Host
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, table=3, "+
			"actions=move:NXM_OF_ETH_DST[]->NXM_OF_ETH_SRC[],mod_dl_dst=%s,output:%s",
			defaultOpenFlowCookie, bridgeMacAddress, ofPortHost))

	// table 4, hairpinned pkts that need to go from OVN -> Host
	// We need to SNAT and masquerade OVN GR IP, send to table 3 for dispatch to Host
	if config.IPv4Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=4,ip,"+
				"actions=ct(commit,zone=%d,nat(src=%s),table=3)",
				defaultOpenFlowCookie, OVNMasqCTZone, types.V4OVNMasqueradeIP))
	}
	if config.IPv6Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=4,ipv6, "+
				"actions=ct(commit,zone=%d,nat(src=%s),table=3)",
				defaultOpenFlowCookie, OVNMasqCTZone, types.V6OVNMasqueradeIP))
	}
	// table 5, Host Reply traffic to hairpinned svc, need to unDNAT, send to table 2
	if config.IPv4Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=5, ip, "+
				"actions=ct(commit,zone=%d,nat,table=2)",
				defaultOpenFlowCookie, HostMasqCTZone))
	}
	if config.IPv6Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=5, ipv6, "+
				"actions=ct(commit,zone=%d,nat,table=2)",
				defaultOpenFlowCookie, HostMasqCTZone))
	}
	return dftFlows, nil
}

func commonFlows(subnets []*net.IPNet, bridge *bridgeConfiguration) ([]string, error) {
	ofPortPhys := bridge.ofPortPhys
	bridgeMacAddress := bridge.macAddress.String()
	ofPortPatch := bridge.ofPortPatch
	ofPortHost := bridge.ofPortHost
	bridgeIPs := bridge.ips

	var dftFlows []string

	if ofPortPhys != "" {
		// table 0, we check to see if this dest mac is the shared mac, if so flood to both ports
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=10, table=0, in_port=%s, dl_dst=%s, actions=output:%s,output:%s",
				defaultOpenFlowCookie, ofPortPhys, bridgeMacAddress, ofPortPatch, ofPortHost))
	}

	if config.IPv4Mode {
		physicalIP, err := util.MatchFirstIPNetFamily(false, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv4 physical IP of host: %v", err)
		}
		if ofPortPhys != "" {
			// table0, packets coming from egressIP pods that have mark 1008 on them
			// will be DNAT-ed a final time into nodeIP to maintain consistency in traffic even if the GR
			// DNATs these into egressIP prior to reaching external bridge.
			// egressService pods will also undergo this SNAT to nodeIP since these features are tied
			// together at the OVN policy level on the distributed router.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=105, in_port=%s, ip, pkt_mark=%s "+
					"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)),output:%s",
					defaultOpenFlowCookie, ofPortPatch, ovnKubeNodeSNATMark, config.Default.ConntrackZone, physicalIP.IP, ctMarkOVN, ofPortPhys))

			// table 0, packets coming from pods headed externally. Commit connections with ct_mark ctMarkOVN
			// so that reverse direction goes back to the pods.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ip, "+
					"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
					defaultOpenFlowCookie, ofPortPatch, config.Default.ConntrackZone, ctMarkOVN, ofPortPhys))

			// table 0, packets coming from host Commit connections with ct_mark ctMarkHost
			// so that reverse direction goes back to the host.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ip, "+
					"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
					defaultOpenFlowCookie, ofPortHost, config.Default.ConntrackZone, ctMarkHost, ofPortPhys))
		}
		if config.Gateway.Mode == config.GatewayModeLocal {
			// table 0, any packet coming from OVN send to host in LGW mode, host will take care of sending it outside if needed.
			// exceptions are traffic for egressIP and egressGW features and ICMP related traffic which will hit the priority 100 flow instead of this.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=175, in_port=%s, tcp, nw_src=%s, "+
					"actions=ct(table=4,zone=%d)",
					defaultOpenFlowCookie, ofPortPatch, physicalIP.IP, HostMasqCTZone))
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=175, in_port=%s, udp, nw_src=%s, "+
					"actions=ct(table=4,zone=%d)",
					defaultOpenFlowCookie, ofPortPatch, physicalIP.IP, HostMasqCTZone))
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=175, in_port=%s, sctp, nw_src=%s, "+
					"actions=ct(table=4,zone=%d)",
					defaultOpenFlowCookie, ofPortPatch, physicalIP.IP, HostMasqCTZone))
			// We send BFD traffic coming from OVN to outside directly using a higher priority flow
			if ofPortPhys != "" {
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=650, table=0, in_port=%s, udp, tp_dst=3784, actions=output:%s",
						defaultOpenFlowCookie, ofPortPatch, ofPortPhys))
			}
		}

		if ofPortPhys != "" {
			// table 0, packets coming from external. Send it through conntrack and
			// resubmit to table 1 to know the state and mark of the connection.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=50, in_port=%s, ip, "+
					"actions=ct(zone=%d, nat, table=1)", defaultOpenFlowCookie, ofPortPhys, config.Default.ConntrackZone))
		}
	}
	if config.IPv6Mode {
		physicalIP, err := util.MatchFirstIPNetFamily(true, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv6 physical IP of host: %v", err)
		}
		if ofPortPhys != "" {
			// table0, packets coming from egressIP pods that have mark 1008 on them
			// will be DNAT-ed a final time into nodeIP to maintain consistency in traffic even if the GR
			// DNATs these into egressIP prior to reaching external bridge.
			// egressService pods will also undergo this SNAT to nodeIP since these features are tied
			// together at the OVN policy level on the distributed router.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=105, in_port=%s, ipv6, pkt_mark=%s "+
					"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)),output:%s",
					defaultOpenFlowCookie, ofPortPatch, ovnKubeNodeSNATMark, config.Default.ConntrackZone, physicalIP.IP, ctMarkOVN, ofPortPhys))

			// table 0, packets coming from pods headed externally. Commit connections with ct_mark ctMarkOVN
			// so that reverse direction goes back to the pods.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ipv6, "+
					"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
					defaultOpenFlowCookie, ofPortPatch, config.Default.ConntrackZone, ctMarkOVN, ofPortPhys))

			// table 0, packets coming from host. Commit connections with ct_mark ctMarkHost
			// so that reverse direction goes back to the host.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ipv6, "+
					"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
					defaultOpenFlowCookie, ofPortHost, config.Default.ConntrackZone, ctMarkHost, ofPortPhys))
		}
		if config.Gateway.Mode == config.GatewayModeLocal {
			// table 0, any packet coming from OVN send to host in LGW mode, host will take care of sending it outside if needed.
			// exceptions are traffic for egressIP and egressGW features and ICMP related traffic which will hit the priority 100 flow instead of this.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=175, in_port=%s, tcp6, ipv6_src=%s, "+
					"actions=ct(table=4,zone=%d)",
					defaultOpenFlowCookie, ofPortPatch, physicalIP.IP, HostMasqCTZone))
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=175, in_port=%s, udp6, ipv6_src=%s, "+
					"actions=ct(table=4,zone=%d)",
					defaultOpenFlowCookie, ofPortPatch, physicalIP.IP, HostMasqCTZone))
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=175, in_port=%s, sctp6, ipv6_src=%s, "+
					"actions=ct(table=4,zone=%d)",
					defaultOpenFlowCookie, ofPortPatch, physicalIP.IP, HostMasqCTZone))
			if ofPortPhys != "" {
				// We send BFD traffic coming from OVN to outside directly using a higher priority flow
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=650, table=0, in_port=%s, udp6, tp_dst=3784, actions=output:%s",
						defaultOpenFlowCookie, ofPortPatch, ofPortPhys))
			}
		}
		if ofPortPhys != "" {
			// table 0, packets coming from external. Send it through conntrack and
			// resubmit to table 1 to know the state and mark of the connection.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=50, in_port=%s, ipv6, "+
					"actions=ct(zone=%d, nat, table=1)", defaultOpenFlowCookie, ofPortPhys, config.Default.ConntrackZone))
		}
	}
	// Egress IP is often configured on a node different from the one hosting the affected pod.
	// Due to the fact that ovn-controllers on different nodes apply the changes independently,
	// there is a chance that the pod traffic will reach the egress node before it configures the SNAT flows.
	// Drop pod traffic that is not SNATed, excluding local pods(required for ICNIv2)
	if config.OVNKubernetesFeature.EnableEgressIP {
		for _, clusterEntry := range config.Default.ClusterSubnets {
			cidr := clusterEntry.CIDR
			ipPrefix := "ip"
			if utilnet.IsIPv6CIDR(cidr) {
				ipPrefix = "ipv6"
			}
			// table 0, drop packets coming from pods headed externally that were not SNATed.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=104, in_port=%s, %s, %s_src=%s, actions=drop",
					defaultOpenFlowCookie, ofPortPatch, ipPrefix, ipPrefix, cidr))
		}
		for _, subnet := range subnets {
			ipPrefix := "ip"
			if utilnet.IsIPv6CIDR(subnet) {
				ipPrefix = "ipv6"
			}
			if ofPortPhys != "" {
				// table 0, commit connections from local pods.
				// ICNIv2 requires that local pod traffic can leave the node without SNAT.
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=109, in_port=%s, %s, %s_src=%s"+
						"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
						defaultOpenFlowCookie, ofPortPatch, ipPrefix, ipPrefix, subnet, config.Default.ConntrackZone, ctMarkOVN, ofPortPhys))
			}
		}
	}

	if ofPortPhys != "" {
		actions := fmt.Sprintf("output:%s", ofPortPatch)

		if config.Gateway.DisableSNATMultipleGWs {
			// table 1, traffic to pod subnet go directly to OVN
			for _, clusterEntry := range config.Default.ClusterSubnets {
				cidr := clusterEntry.CIDR
				var ipPrefix string
				if utilnet.IsIPv6CIDR(cidr) {
					ipPrefix = "ipv6"
				} else {
					ipPrefix = "ip"
				}
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=15, table=1, %s, %s_dst=%s, "+
						"actions=%s",
						defaultOpenFlowCookie, ipPrefix, ipPrefix, cidr, actions))
			}
		}

		// table 1, we check to see if this dest mac is the shared mac, if so send to host
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=10, table=1, dl_dst=%s, actions=output:%s",
				defaultOpenFlowCookie, bridgeMacAddress, ofPortHost))

		if config.IPv6Mode {
			// REMOVEME(trozet) when https://bugzilla.kernel.org/show_bug.cgi?id=11797 is resolved
			// must flood icmpv6 Route Advertisement and Neighbor Advertisement traffic as it fails to create a CT entry
			for _, icmpType := range []int{types.RouteAdvertisementICMPType, types.NeighborAdvertisementICMPType} {
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=14, table=1,icmp6,icmpv6_type=%d actions=FLOOD",
						defaultOpenFlowCookie, icmpType))
			}
			if ofPortPhys != "" {
				// We send BFD traffic both on the host and in ovn
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=13, table=1, in_port=%s, udp6, tp_dst=3784, actions=output:%s,output:%s",
						defaultOpenFlowCookie, ofPortPhys, ofPortPatch, ofPortHost))
			}
		}

		if config.IPv4Mode {
			if ofPortPhys != "" {
				// We send BFD traffic both on the host and in ovn
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=13, table=1, in_port=%s, udp, tp_dst=3784, actions=output:%s,output:%s",
						defaultOpenFlowCookie, ofPortPhys, ofPortPatch, ofPortHost))
			}
		}
		// table 1, all other connections do normal processing
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=0, table=1, actions=output:NORMAL", defaultOpenFlowCookie))
	}

	return dftFlows, nil
}

func setBridgeOfPorts(bridge *bridgeConfiguration) error {
	// Get ofport of patchPort
	ofportPatch, stderr, err := util.GetOVSOfPort("get", "Interface", bridge.patchPort, "ofport")
	if err != nil {
		return fmt.Errorf("failed while waiting on patch port %q to be created by ovn-controller and "+
			"while getting ofport. stderr: %q, error: %v", bridge.patchPort, stderr, err)
	}
	bridge.ofPortPatch = ofportPatch

	if bridge.uplinkName != "" {
		// Get ofport of physical interface
		ofportPhys, stderr, err := util.GetOVSOfPort("get", "interface", bridge.uplinkName, "ofport")
		if err != nil {
			return fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
				bridge.uplinkName, stderr, err)
		}
		bridge.ofPortPhys = ofportPhys
	}

	// Get ofport represeting the host. That is, host representor port in case of DPUs, ovsLocalPort otherwise.
	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		var stderr string
		hostRep, err := util.GetDPUHostInterface(bridge.bridgeName)
		if err != nil {
			return err
		}

		bridge.ofPortHost, stderr, err = util.RunOVSVsctl("get", "interface", hostRep, "ofport")
		if err != nil {
			return fmt.Errorf("failed to get ofport of host interface %s, stderr: %q, error: %v",
				hostRep, stderr, err)
		}
	} else {
		bridge.ofPortHost = ovsLocalPort
	}

	return nil
}

// initSvcViaMgmPortRoutingRules creates the svc2managementport routing table, routes and rules
// that let's us forward service traffic to ovn-k8s-mp0 as opposed to the default route towards breth0
func initSvcViaMgmPortRoutingRules(hostSubnets []*net.IPNet) error {
	// create ovnkubeSvcViaMgmPortRT and service route towards ovn-k8s-mp0
	for _, hostSubnet := range hostSubnets {
		isIPv6 := utilnet.IsIPv6CIDR(hostSubnet)
		gatewayIP := util.GetNodeGatewayIfAddr(hostSubnet).IP.String()
		for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
			if isIPv6 == utilnet.IsIPv6CIDR(svcCIDR) {
				if stdout, stderr, err := util.RunIP("route", "replace", "table", ovnkubeSvcViaMgmPortRT, svcCIDR.String(), "via", gatewayIP, "dev", types.K8sMgmtIntfName); err != nil {
					return fmt.Errorf("error adding routing table entry into custom routing table: %s: stdout: %s, stderr: %s, err: %v", ovnkubeSvcViaMgmPortRT, stdout, stderr, err)
				}
				klog.V(5).Infof("Successfully added route into custom routing table: %s", ovnkubeSvcViaMgmPortRT)
			}
		}
	}

	createRule := func(family string) error {
		stdout, stderr, err := util.RunIP(family, "rule")
		if err != nil {
			return fmt.Errorf("error listing routing rules, stdout: %s, stderr: %s, err: %v", stdout, stderr, err)
		}
		if !strings.Contains(stdout, fmt.Sprintf("from all fwmark %s lookup %s", ovnkubeITPMark, ovnkubeSvcViaMgmPortRT)) {
			if stdout, stderr, err := util.RunIP(family, "rule", "add", "fwmark", ovnkubeITPMark, "lookup", ovnkubeSvcViaMgmPortRT, "prio", "30"); err != nil {
				return fmt.Errorf("error adding routing rule for service via management table (%s): stdout: %s, stderr: %s, err: %v", ovnkubeSvcViaMgmPortRT, stdout, stderr, err)
			}
		}
		return nil
	}

	// create ip rule that will forward ovnkubeITPMark marked packets to ovnkubeITPRoutingTable
	if config.IPv4Mode {
		if err := createRule("-4"); err != nil {
			return fmt.Errorf("could not add IPv4 rule: %v", err)
		}
	}
	if config.IPv6Mode {
		if err := createRule("-6"); err != nil {
			return fmt.Errorf("could not add IPv6 rule: %v", err)
		}
	}

	// lastly update the reverse path filtering options for ovn-k8s-mp0 interface to avoid dropping return packets
	// NOTE: v6 doesn't have rp_filter strict mode block
	rpFilterLooseMode := "2"
	// TODO: Convert testing framework to mock golang module utilities. Example:
	// result, err := sysctl.Sysctl(fmt.Sprintf("net/ipv4/conf/%s/rp_filter", types.K8sMgmtIntfName), rpFilterLooseMode)
	stdout, stderr, err := util.RunSysctl("-w", fmt.Sprintf("net.ipv4.conf.%s.rp_filter=%s", types.K8sMgmtIntfName, rpFilterLooseMode))
	if err != nil || stdout != fmt.Sprintf("net.ipv4.conf.%s.rp_filter = %s", types.K8sMgmtIntfName, rpFilterLooseMode) {
		return fmt.Errorf("could not set the correct rp_filter value for interface %s: stdout: %v, stderr: %v, err: %v",
			types.K8sMgmtIntfName, stdout, stderr, err)
	}

	return nil
}

func newSharedGateway(nodeName string, subnets []*net.IPNet, gwNextHops []net.IP, gwIntf, egressGWIntf string,
	gwIPs []*net.IPNet, nodeAnnotator kube.Annotator, kube kube.Interface, cfg *managementPortConfig,
	watchFactory factory.NodeWatchFactory, routeManager *routeManager) (*gateway, error) {
	klog.Info("Creating new shared gateway")
	gw := &gateway{}

	gwBridge, exGwBridge, err := gatewayInitInternal(
		nodeName, gwIntf, egressGWIntf, gwNextHops, gwIPs, nodeAnnotator)
	if err != nil {
		return nil, err
	}

	if exGwBridge != nil {
		gw.readyFunc = func() (bool, error) {
			ready, err := gatewayReady(gwBridge.patchPort)
			if err != nil {
				return false, err
			}
			exGWReady, err := gatewayReady(exGwBridge.patchPort)
			if err != nil {
				return false, err
			}
			return ready && exGWReady, nil
		}
	} else {
		gw.readyFunc = func() (bool, error) {
			return gatewayReady(gwBridge.patchPort)
		}
	}

	gw.initFunc = func() error {
		// Program cluster.GatewayIntf to let non-pod traffic to go to host
		// stack
		klog.Info("Creating Shared Gateway Openflow Manager")
		err := setBridgeOfPorts(gwBridge)
		if err != nil {
			return err
		}
		if exGwBridge != nil {
			err = setBridgeOfPorts(exGwBridge)
			if err != nil {
				return err
			}
			if config.Gateway.DisableForwarding {
				if err := initExternalBridgeDropForwardingRules(exGwBridge.bridgeName); err != nil {
					return fmt.Errorf("failed to add forwarding block rules for bridge %s: err %v", exGwBridge.bridgeName, err)
				}
			}
		}
		gw.nodeIPManager = newAddressManager(nodeName, kube, cfg, watchFactory, gwBridge)
		nodeIPs := gw.nodeIPManager.ListAddresses()

		if config.OvnKubeNode.Mode == types.NodeModeFull {
			if err := setNodeMasqueradeIPOnExtBridge(gwBridge.bridgeName); err != nil {
				return fmt.Errorf("failed to set the node masquerade IP on the ext bridge %s: %v", gwBridge.bridgeName, err)
			}

			if err := addMasqueradeRoute(routeManager, gwBridge.bridgeName, nodeName, gwIPs, watchFactory); err != nil {
				return fmt.Errorf("failed to set the node masquerade route to OVN: %v", err)
			}
		}

		gw.openflowManager, err = newGatewayOpenFlowManager(gwBridge, exGwBridge, subnets, nodeIPs)
		if err != nil {
			return err
		}

		// resync flows on IP change
		gw.nodeIPManager.OnChanged = func() {
			klog.V(5).Info("Node addresses changed, re-syncing bridge flows")
			if err := gw.openflowManager.updateBridgeFlowCache(subnets, gw.nodeIPManager.ListAddresses()); err != nil {
				// very unlikely - somehow node has lost its IP address
				klog.Errorf("Failed to re-generate gateway flows after address change: %v", err)
			}
			npw, _ := gw.nodePortWatcher.(*nodePortWatcher)
			npw.updateGatewayIPs(gw.nodeIPManager)
			gw.openflowManager.requestFlowSync()
		}

		if config.Gateway.NodeportEnable {
			if config.OvnKubeNode.Mode == types.NodeModeFull {
				// (TODO): Internal Traffic Policy is not supported in DPU mode
				if err := initSvcViaMgmPortRoutingRules(subnets); err != nil {
					return err
				}
			}
			klog.Info("Creating Shared Gateway Node Port Watcher")
			gw.nodePortWatcher, err = newNodePortWatcher(gwBridge, gw.openflowManager, gw.nodeIPManager, watchFactory)
			if err != nil {
				return err
			}
		} else {
			// no service OpenFlows, request to sync flows now.
			gw.openflowManager.requestFlowSync()
		}

		if err := addHostMACBindings(gwBridge.bridgeName); err != nil {
			return fmt.Errorf("failed to add MAC bindings for service routing")
		}

		return nil
	}
	gw.watchFactory = watchFactory.(*factory.WatchFactory)
	klog.Info("Shared Gateway Creation Complete")
	return gw, nil
}

func newNodePortWatcher(gwBridge *bridgeConfiguration, ofm *openflowManager,
	nodeIPManager *addressManager, watchFactory factory.NodeWatchFactory) (*nodePortWatcher, error) {
	// Get ofport of patchPort
	ofportPatch, stderr, err := util.GetOVSOfPort("--if-exists", "get",
		"interface", gwBridge.patchPort, "ofport")
	if err != nil {
		return nil, fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			gwBridge.patchPort, stderr, err)
	}

	// Get ofport of physical interface
	ofportPhys, stderr, err := util.GetOVSOfPort("--if-exists", "get",
		"interface", gwBridge.uplinkName, "ofport")
	if err != nil {
		return nil, fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			gwBridge.uplinkName, stderr, err)
	}

	// In the shared gateway mode, the NodePort service is handled by the OpenFlow flows configured
	// on the OVS bridge in the host. These flows act only on the packets coming in from outside
	// of the node. If someone on the node is trying to access the NodePort service, those packets
	// will not be processed by the OpenFlow flows, so we need to add iptable rules that DNATs the
	// NodePortIP:NodePort to ClusterServiceIP:Port. We don't need to do this while
	// running on DPU or on DPU-Host.
	if config.OvnKubeNode.Mode == types.NodeModeFull {
		if config.Gateway.Mode == config.GatewayModeLocal {
			if err := initLocalGatewayIPTables(); err != nil {
				return nil, err
			}
		} else if config.Gateway.Mode == config.GatewayModeShared {
			if err := initSharedGatewayIPTables(); err != nil {
				return nil, err
			}
		}
	}

	if config.Gateway.DisableForwarding {
		for _, subnet := range config.Kubernetes.ServiceCIDRs {
			if err := initExternalBridgeServiceForwardingRules(subnet); err != nil {
				return nil, fmt.Errorf("failed to add forwarding rules for bridge %s: err %v", gwBridge.bridgeName, err)
			}
		}
		if err := initExternalBridgeDropForwardingRules(gwBridge.bridgeName); err != nil {
			return nil, fmt.Errorf("failed to add forwarding rules for bridge %s: err %v", gwBridge.bridgeName, err)
		}
	}

	// used to tell addServiceRules which rules to add
	dpuMode := false
	if config.OvnKubeNode.Mode != types.NodeModeFull {
		dpuMode = true
	}

	// Get Physical IPs of Node, Can be IPV4 IPV6 or both
	gatewayIPv4, gatewayIPv6 := getGatewayFamilyAddrs(gwBridge.ips)

	npw := &nodePortWatcher{
		dpuMode:       dpuMode,
		gatewayIPv4:   gatewayIPv4,
		gatewayIPv6:   gatewayIPv6,
		ofportPhys:    ofportPhys,
		ofportPatch:   ofportPatch,
		gwBridge:      gwBridge.bridgeName,
		serviceInfo:   make(map[ktypes.NamespacedName]*serviceConfig),
		nodeIPManager: nodeIPManager,
		ofm:           ofm,
		watchFactory:  watchFactory,
	}
	return npw, nil
}

func cleanupSharedGateway() error {
	// NicToBridge() may be created before-hand, only delete the patch port here
	stdout, stderr, err := util.RunOVSVsctl("--columns=name", "--no-heading", "find", "port",
		"external_ids:ovn-localnet-port!=_")
	if err != nil {
		return fmt.Errorf("failed to get ovn-localnet-port port stderr:%s (%v)", stderr, err)
	}
	ports := strings.Fields(strings.Trim(stdout, "\""))
	for _, port := range ports {
		_, stderr, err := util.RunOVSVsctl("--if-exists", "del-port", strings.Trim(port, "\""))
		if err != nil {
			return fmt.Errorf("failed to delete port %s stderr:%s (%v)", port, stderr, err)
		}
	}

	// Get the OVS bridge name from ovn-bridge-mappings
	stdout, stderr, err = util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".",
		"external_ids:ovn-bridge-mappings")
	if err != nil {
		return fmt.Errorf("failed to get ovn-bridge-mappings stderr:%s (%v)", stderr, err)
	}

	// skip the existing mapping setting for the specified physicalNetworkName
	bridgeName := ""
	bridgeMappings := strings.Split(stdout, ",")
	for _, bridgeMapping := range bridgeMappings {
		m := strings.Split(bridgeMapping, ":")
		if network := m[0]; network == types.PhysicalNetworkName {
			bridgeName = m[1]
			break
		}
	}
	if len(bridgeName) == 0 {
		return nil
	}

	_, stderr, err = util.AddOFFlowWithSpecificAction(bridgeName, util.NormalAction)
	if err != nil {
		return fmt.Errorf("failed to replace-flows on bridge %q stderr:%s (%v)", bridgeName, stderr, err)
	}

	cleanupSharedGatewayIPTChains()
	return nil
}

func svcToCookie(namespace string, name string, token string, port int32) (string, error) {
	id := fmt.Sprintf("%s%s%s%d", namespace, name, token, port)
	h := fnv.New64a()
	_, err := h.Write([]byte(id))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("0x%x", h.Sum64()), nil
}

func addMasqueradeRoute(routeManager *routeManager, netIfaceName, nodeName string, ifAddrs []*net.IPNet, watchFactory factory.NodeWatchFactory) error {
	var ipv4, ipv6 net.IP
	findIPs := func(ips []net.IP) error {
		var err error
		if config.IPv4Mode && ipv4 == nil {
			ipv4, err = util.MatchFirstIPFamily(false, ips)
			if err != nil {
				return fmt.Errorf("missing IP among %+v: %v", ips, err)
			}
		}
		if config.IPv6Mode && ipv6 == nil {
			ipv6, err = util.MatchFirstIPFamily(true, ips)
			if err != nil {
				return fmt.Errorf("missing IP among %+v: %v", ips, err)
			}
		}
		return nil
	}

	// Try first with the node status IPs and fallback to the interface IPs. The
	// fallback is a workaround for instances where the node status might not
	// have the minimum set of IPs we need (for example, when ovnkube is
	// restarted after enabling an IP family without actually restarting kubelet
	// with a new configuration including an IP address for that family). Node
	// status IPs are preferred though because a user might add arbitrary IP
	// addresses to the interface that we don't really want to use and might
	// cause problems.

	var nodeIPs []net.IP
	node, err := watchFactory.GetNode(nodeName)
	if err != nil {
		return err
	}
	for _, nodeAddr := range node.Status.Addresses {
		if nodeAddr.Type != kapi.NodeInternalIP {
			continue
		}
		nodeIP := utilnet.ParseIPSloppy(nodeAddr.Address)
		nodeIPs = append(nodeIPs, nodeIP)
	}

	err = findIPs(nodeIPs)
	if err != nil {
		klog.Warningf("Unable to add OVN masquerade route to host using source node status IPs: %v", err)
		// fallback to the interface IPs
		var ifIPs []net.IP
		for _, ifAddr := range ifAddrs {
			ifIPs = append(ifIPs, ifAddr.IP)
		}
		err := findIPs(ifIPs)
		if err != nil {
			return fmt.Errorf("unable to add OVN masquerade route to host using interface IPs: %v", err)
		}
	}

	netIfaceLink, err := util.LinkSetUp(netIfaceName)
	if err != nil {
		return fmt.Errorf("unable to find shared gw bridge interface: %s", netIfaceName)
	}
	mtu := 0
	var routes []route
	if ipv4 != nil {
		_, masqIPNet, _ := net.ParseCIDR(fmt.Sprintf("%s/32", types.V4OVNMasqueradeIP))
		klog.Infof("Setting OVN Masquerade route with source: %s", ipv4)

		routes = append(routes, route{
			gwIP:   nil,
			subnet: masqIPNet,
			mtu:    mtu,
			srcIP:  ipv4,
		})
	}

	if ipv6 != nil {
		_, masqIPNet, _ := net.ParseCIDR(fmt.Sprintf("%s/128", types.V6OVNMasqueradeIP))
		klog.Infof("Setting OVN Masquerade route with source: %s", ipv6)

		routes = append(routes, route{
			gwIP:   nil,
			subnet: masqIPNet,
			mtu:    mtu,
			srcIP:  ipv6,
		})
	}
	if len(routes) > 0 {
		routeManager.add(routesPerLink{netIfaceLink, routes})
	}

	return nil
}

func setNodeMasqueradeIPOnExtBridge(extBridgeName string) error {
	extBridge, err := util.LinkSetUp(extBridgeName)
	if err != nil {
		return err
	}

	var bridgeCIDRs []cidrAndFlags
	if config.IPv4Mode {
		_, masqIPNet, _ := net.ParseCIDR(types.V4MasqueradeSubnet)
		masqIPNet.IP = net.ParseIP(types.V4HostMasqueradeIP)
		bridgeCIDRs = append(bridgeCIDRs, cidrAndFlags{ipNet: masqIPNet, flags: 0})
	}

	if config.IPv6Mode {
		_, masqIPNet, _ := net.ParseCIDR(types.V6MasqueradeSubnet)
		masqIPNet.IP = net.ParseIP(types.V6HostMasqueradeIP)
		bridgeCIDRs = append(bridgeCIDRs, cidrAndFlags{ipNet: masqIPNet, flags: unix.IFA_F_NODAD})
	}

	for _, bridgeCIDR := range bridgeCIDRs {
		if exists, err := util.LinkAddrExist(extBridge, bridgeCIDR.ipNet); err == nil && !exists {
			if err := util.LinkAddrAdd(extBridge, bridgeCIDR.ipNet, bridgeCIDR.flags); err != nil {
				return err
			}
		} else if err != nil {
			return fmt.Errorf(
				"failed to check existence of addr %s in bridge %s: %v", bridgeCIDR.ipNet, extBridgeName, err)
		}
	}

	return nil
}

func addHostMACBindings(bridgeName string) error {
	// Add a neighbour entry on the K8s node to map dummy next-hop masquerade
	// addresses with MACs. This is required because these addresses do not
	// exist on the network and will not respond to an ARP/ND, so to route them
	// we need an entry.
	// Additionally, the OVN Masquerade IP is not assigned to its interface, so
	// we also need a fake entry for that.
	link, err := util.LinkSetUp(bridgeName)
	if err != nil {
		return fmt.Errorf("unable to get link for %s, error: %v", bridgeName, err)
	}

	var neighborIPs []string
	if config.IPv4Mode {
		neighborIPs = append(neighborIPs, types.V4OVNMasqueradeIP, types.V4DummyNextHopMasqueradeIP)
	}
	if config.IPv6Mode {
		neighborIPs = append(neighborIPs, types.V6OVNMasqueradeIP, types.V6DummyNextHopMasqueradeIP)
	}
	for _, ip := range neighborIPs {
		klog.Infof("Ensuring IP Neighbor entry for: %s", ip)
		dummyNextHopMAC := util.IPAddrToHWAddr(net.ParseIP(ip))
		if exists, err := util.LinkNeighExists(link, net.ParseIP(ip), dummyNextHopMAC); err == nil && !exists {
			// LinkNeighExists checks if the mac also matches, but it is possible there is a stale entry
			// still in the neighbor cache which would prevent add. Therefore execute a delete first.
			if err = util.LinkNeighDel(link, net.ParseIP(ip)); err != nil {
				klog.Warningf("Failed to remove IP neighbor entry for ip %s, on iface %s: %v",
					ip, bridgeName, err)
			}
			if err = util.LinkNeighAdd(link, net.ParseIP(ip), dummyNextHopMAC); err != nil {
				return fmt.Errorf("failed to configure neighbor: %s, on iface %s: %v",
					ip, bridgeName, err)
			}
		} else if err != nil {
			return fmt.Errorf("failed to configure neighbor:%s, on iface %s: %v", ip, bridgeName, err)
		}
	}
	return nil
}
