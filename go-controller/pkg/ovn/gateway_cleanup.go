package ovn

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// gatewayCleanup removes all the NB DB objects created for a node's gateway
func gatewayCleanup(nodeName string) error {
	gatewayRouter := types.GWRouterPrefix + nodeName

	// Get the gateway router port's IP address (connected to join switch)
	var nextHops []net.IP

	gwIPAddrs, err := util.GetLRPAddrs(types.GWRouterToJoinSwitchPrefix + gatewayRouter)
	if err != nil {
		return err
	}

	for _, gwIPAddr := range gwIPAddrs {
		nextHops = append(nextHops, gwIPAddr.IP)
	}
	staticRouteCleanup(nextHops)

	// Remove the patch port that connects join switch to gateway router
	_, stderr, err := util.RunOVNNbctl("--if-exist", "lsp-del", types.JoinSwitchToGWRouterPrefix+gatewayRouter)
	if err != nil {
		return fmt.Errorf("failed to delete logical switch port %s%s: "+
			"stderr: %q, error: %v", types.JoinSwitchToGWRouterPrefix, gatewayRouter, stderr, err)
	}

	// Remove the gateway router associated with nodeName
	_, stderr, err = util.RunOVNNbctl("--if-exist", "lr-del",
		gatewayRouter)
	if err != nil {
		return fmt.Errorf("failed to delete gateway router %s, stderr: %q, "+
			"error: %v", gatewayRouter, stderr, err)
	}

	// Remove external switch
	externalSwitch := types.ExternalSwitchPrefix + nodeName
	_, stderr, err = util.RunOVNNbctl("--if-exist", "ls-del",
		externalSwitch)
	if err != nil {
		return fmt.Errorf("failed to delete external switch %s, stderr: %q, "+
			"error: %v", externalSwitch, stderr, err)
	}

	// If exists, remove the TCP, UDP load-balancers created for north-south traffic for gateway router.
	k8sNSLbTCP, k8sNSLbUDP, k8sNSLbSCTP, err := getGatewayLoadBalancers(gatewayRouter)
	if err != nil {
		return err
	}
	protoLBMap := map[kapi.Protocol]string{
		kapi.ProtocolTCP:  k8sNSLbTCP,
		kapi.ProtocolUDP:  k8sNSLbUDP,
		kapi.ProtocolSCTP: k8sNSLbSCTP,
	}
	for proto, uuid := range protoLBMap {
		if uuid != "" {
			_, stderr, err = util.RunOVNNbctl("lb-del", uuid)
			if err != nil {
				return fmt.Errorf("failed to delete Gateway router %s's %s load balancer %s, stderr: %q, "+
					"error: %v", gatewayRouter, proto, uuid, stderr, err)
			}
		}
	}

	// We don't know the gateway mode as this is running in the master, try to delete the additional local
	// gateway for the shared gateway mode. it will be no op if this is done for other gateway modes.
	delPbrAndNatRules(nodeName)
	return nil
}

func delPbrAndNatRules(nodeName string) {
	// delete the dnat_and_snat entry that we added for the management port IP
	// Note: we don't need to delete any MAC bindings that are dynamically learned from OVN SB DB
	// because there will be none since this NAT is only for outbound traffic and not for inbound
	mgmtPortName := types.K8sPrefix + nodeName
	externalIP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=external_ip", "find", "nat", fmt.Sprintf("logical_port=%s", mgmtPortName))
	if err != nil {
		klog.Errorf("Failed to fetch the dnat_and_snat entry for the management port %s "+
			"stderr: %s, error: %v", mgmtPortName, stderr, err)
		externalIP = ""
	}
	if externalIP != "" {
		_, stderr, err = util.RunOVNNbctl("--if-exists", "lr-nat-del", types.OVNClusterRouter, "dnat_and_snat", externalIP)
		if err != nil {
			klog.Errorf("Failed to delete the dnat_and_snat ip %s associated with the management "+
				"port %s: stderr: %s, error: %v", externalIP, mgmtPortName, stderr, err)
		}
	}

	// find the pbr rules associated with the node and delete them
	matches, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=match",
		"find", "logical_router_policy")
	if err != nil {
		klog.Errorf("Failed to fetch the policy route for the node subnet "+
			"stdout: %s, stderr: %s, error: %v", matches, stderr, err)
		matches = ""
	}
	// matches inport for a policy, must use ending quote to substring match to avoid matching other nodes accidentally
	// i.e. rtos-ovn-worker would match rtos-ovn-worker2
	nodeSubnetMatchSubStr := fmt.Sprintf("%s%s\"", types.RouterToSwitchPrefix, nodeName)
	// for inter node, match the comment in the policy, and include extra space to avoid accidental submatch
	interNodeMatchSubStr := fmt.Sprintf("%s%s ", types.InterPrefix, nodeName)
	// mgmt port policy matches node name in comment, use extra spaces to avoid accidental match of other nodes
	mgmtPortPolicyMatchSubStr := fmt.Sprintf(" %s ", nodeName)
	for _, match := range strings.Split(matches, "\n\n") {
		var priority string
		if strings.Contains(match, nodeSubnetMatchSubStr) {
			priority = types.NodeSubnetPolicyPriority
		} else if strings.Contains(match, interNodeMatchSubStr) {
			priority = types.InterNodePolicyPriority
		} else if strings.Contains(match, mgmtPortPolicyMatchSubStr) {
			priority = types.MGMTPortPolicyPriority
		} else {
			continue
		}
		_, stderr, err = util.RunOVNNbctl("lr-policy-del", types.OVNClusterRouter, priority, match)
		if err != nil {
			klog.Errorf("Failed to remove the policy routes (%s, %s) associated with the node %s: stderr: %s, error: %v",
				priority, match, nodeName, stderr, err)
		}
	}
}

func staticRouteCleanup(nextHops []net.IP) {
	for _, nextHop := range nextHops {
		// Get a list of all the routes in cluster router with the next hop IP.
		var uuids string
		uuids, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
			"--columns=_uuid", "find", "logical_router_static_route",
			"nexthop=\""+nextHop.String()+"\"")
		if err != nil {
			klog.Errorf("Failed to fetch all routes with "+
				"IP %s as nexthop, stderr: %q, "+
				"error: %v", nextHop.String(), stderr, err)
			continue
		}

		// Remove all the routes in cluster router with this IP as the nexthop.
		routes := strings.Fields(uuids)
		for _, route := range routes {
			_, stderr, err = util.RunOVNNbctl("--if-exists", "remove",
				"logical_router", types.OVNClusterRouter, "static_routes", route)
			if err != nil {
				klog.Errorf("Failed to delete static route %s"+
					", stderr: %q, err = %v", route, stderr, err)
			}
		}
	}
}

// multiJoinSwitchGatewayCleanup removes the OVN NB gateway logical entities that for
// the obsoleted multiple join switch OVN topology.
//
// if "upgradeOnly" is true, this function only deletes the gateway logical entities that
// are unique for the multiple join switch OVN topology version; this is used for
// upgrading its OVN logical topology to the single join switch version.
//
// if "upgradeOnly" is false, this function deletes all the gateway logical entities of
// the specific node, even some of them are common in both the multiple join switch and
// the single join switch versions; this is to cleanup the logical entities for the
// specified node if the node was deleted when the ovnkube-master pod was brought down
// to do the version upgrade.
func multiJoinSwitchGatewayCleanup(nodeName string, upgradeOnly bool) error {
	gatewayRouter := types.GWRouterPrefix + nodeName

	// Get the gateway router port's IP address (connected to join switch)
	var nextHops []net.IP

	gwIPAddrs, err := util.GetLRPAddrs(types.GWRouterToJoinSwitchPrefix + gatewayRouter)
	if err != nil {
		return err
	}

	for _, gwIPAddr := range gwIPAddrs {
		// Delete logical router policy whose nexthop is the old rtoj- gateway port address
		stdout, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid",
			"find", "logical_router_policy", fmt.Sprintf("nexthop=%s", gwIPAddr.IP))
		if err != nil {
			klog.Errorf("Unable to find LR policy of nexthop: %s for node %s, stderr: %s, err: %v",
				gwIPAddr.IP, nodeName, stderr, err)
			continue
		}
		if stdout != "" {
			policyIDs := strings.Fields(stdout)
			for _, policyID := range policyIDs {
				_, stderr, err = util.RunOVNNbctl("remove", "logical_router", types.OVNClusterRouter, "policies", policyID)
				if err != nil {
					klog.Errorf("Unable to remove LR policy: %s, stderr: %s, err: %v", policyID, stderr, err)
				}
			}
		}
		nextHops = append(nextHops, gwIPAddr.IP)
	}
	staticRouteCleanup(nextHops)

	// Remove the join switch that connects ovn_cluster_router to gateway router
	_, stderr, err := util.RunOVNNbctl("--if-exist", "ls-del", "join_"+nodeName)
	if err != nil {
		return fmt.Errorf("failed to delete the join logical switch %s, "+
			"stderr: %q, error: %v", "join_"+nodeName, stderr, err)
	}

	// Remove the logical router port on the distributed router that connects to the join switch
	_, stderr, err = util.RunOVNNbctl("--if-exist", "lrp-del", "dtoj-"+nodeName)
	if err != nil {
		return fmt.Errorf("failed to delete the patch port dtoj-%s on distributed router "+
			"stderr: %q, error: %v", nodeName, stderr, err)
	}

	// Remove the logical router port on the gateway router that connects to the join switch
	_, stderr, err = util.RunOVNNbctl("--if-exist", "lrp-del", types.GWRouterToJoinSwitchPrefix+gatewayRouter)
	if err != nil {
		return fmt.Errorf("failed to delete the port %s%s on gateway router "+
			"stderr: %q, error: %v", types.GWRouterToJoinSwitchPrefix, gatewayRouter, stderr, err)
	}

	if upgradeOnly {
		return nil
	}

	// Remove the gateway router associated with nodeName
	_, stderr, err = util.RunOVNNbctl("--if-exist", "lr-del",
		gatewayRouter)
	if err != nil {
		return fmt.Errorf("failed to delete gateway router %s, stderr: %q, "+
			"error: %v", gatewayRouter, stderr, err)
	}

	// Remove external switch
	externalSwitch := types.ExternalSwitchPrefix + nodeName
	_, stderr, err = util.RunOVNNbctl("--if-exist", "ls-del",
		externalSwitch)
	if err != nil {
		return fmt.Errorf("failed to delete external switch %s, stderr: %q, "+
			"error: %v", externalSwitch, stderr, err)
	}

	// If exists, remove the TCP, UDP load-balancers created for north-south traffic for gateway router.
	k8sNSLbTCP, k8sNSLbUDP, k8sNSLbSCTP, err := getGatewayLoadBalancers(gatewayRouter)
	if err != nil {
		return err
	}
	protoLBMap := map[kapi.Protocol]string{
		kapi.ProtocolTCP:  k8sNSLbTCP,
		kapi.ProtocolUDP:  k8sNSLbUDP,
		kapi.ProtocolSCTP: k8sNSLbSCTP,
	}
	for proto, uuid := range protoLBMap {
		if uuid != "" {
			_, stderr, err = util.RunOVNNbctl("lb-del", uuid)
			if err != nil {
				return fmt.Errorf("failed to delete Gateway router %s's %s load balancer %s, stderr: %q, "+
					"error: %v", gatewayRouter, proto, uuid, stderr, err)
			}
		}
	}

	// We don't know the gateway mode as this is running in the master, try to delete the additional local
	// gateway for the shared gateway mode. it will be no op if this is done for other gateway modes.
	delPbrAndNatRules(nodeName)
	return nil
}
