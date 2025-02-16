/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package huaweicloud

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	ecsmodel "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/ecs/v2/model"
	eipmodel "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/eip/v2/model"
	elbmodel "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v2/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-huaweicloud/pkg/cloudprovider/huaweicloud/wrapper"
	"sigs.k8s.io/cloud-provider-huaweicloud/pkg/common"
	"sigs.k8s.io/cloud-provider-huaweicloud/pkg/config"
)

var (
	allowedIPTypes = map[corev1.NodeAddressType]bool{
		corev1.NodeInternalIP: true,
		corev1.NodeExternalIP: true,
	}
)

type SharedLoadBalancer struct {
	Basic
}

func (l *SharedLoadBalancer) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (*v1.LoadBalancerStatus, bool, error) {
	klog.Infof("GetLoadBalancer: called with service %s/%s", service.Namespace, service.Name)
	loadbalancer, err := l.getLoadBalancerInstance(ctx, clusterName, service)
	if err != nil {
		if common.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}

	portID := loadbalancer.VipPortId
	if portID == "" {
		return nil, false, status.Errorf(codes.Unavailable, "The ELB %s VipPortId is empty, "+
			"and the instance is unavailable", l.GetLoadBalancerName(ctx, clusterName, service))
	}
	ingressIP := loadbalancer.VipAddress

	ips, err := l.eipClient.List(&eipmodel.ListPublicipsRequest{PortId: &[]string{portID}})
	if err != nil {
		return nil, false, status.Errorf(codes.Unavailable, "error querying EIPs base on PortId (%s): %s", portID, err)
	}
	if len(ips) > 0 {
		ingressIP = *ips[0].PublicIpAddress
	}

	return &corev1.LoadBalancerStatus{
		Ingress: []corev1.LoadBalancerIngress{
			{IP: ingressIP},
		},
	}, true, nil
}

func (l *SharedLoadBalancer) getLoadBalancerInstance(ctx context.Context, clusterName string, service *v1.Service) (*elbmodel.LoadbalancerResp, error) {
	if id := getStringFromSvsAnnotation(service, ElbID, ""); id != "" {
		return l.sharedELBClient.GetInstance(id)
	}

	name := l.GetLoadBalancerName(ctx, clusterName, service)
	list, err := l.sharedELBClient.ListInstances(&elbmodel.ListLoadbalancersRequest{Name: &name})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, status.Errorf(codes.NotFound, "not found ELB instance %s", name)
	}
	if len(list) != 1 {
		return nil, status.Errorf(codes.Unavailable, "error, found %d ELBs named %s, make sure there is only one",
			len(list), name)
	}
	return &list[0], nil
}

// GetLoadBalancerName returns the name of the load balancer. Implementations must treat the
// *v1.Service parameter as read-only and not modify it.
func (l *SharedLoadBalancer) GetLoadBalancerName(_ context.Context, clusterName string, service *v1.Service) string {
	klog.Infof("GetLoadBalancerName: called with service %s/%s", service.Namespace, service.Name)
	name := fmt.Sprintf("k8s_service_%s_%s_%s", clusterName, service.Namespace, service.Name)
	return cutString(name)
}

func ensureLoadBalancerValidation(service *v1.Service, nodes []*v1.Node) error {
	if len(nodes) == 0 {
		return fmt.Errorf("there are no available nodes for LoadBalancer service %s/%s",
			service.Namespace, service.Name)
	}

	ports := service.Spec.Ports
	if len(ports) == 0 {
		return fmt.Errorf("the loadbalancer service does not configure Spec.Ports")
	}
	if len(service.Spec.Selector) == 0 {
		return fmt.Errorf("the loadbalancer service does not provide Selector, " +
			"services custom endpoints are not supported")
	}

	return nil
}

// EnsureLoadBalancer creates a new load balancer 'name', or updates the existing one. Returns the status of the balancer
//
//nolint:gocyclo
func (l *SharedLoadBalancer) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	klog.Infof("EnsureLoadBalancer: called with service %s/%s, node: %d",
		service.Namespace, service.Name, len(nodes))

	if err := ensureLoadBalancerValidation(service, nodes); err != nil {
		return nil, err
	}

	// get exits or create a new ELB instance
	loadbalancer, err := l.getLoadBalancerInstance(ctx, clusterName, service)
	specifiedID := getStringFromSvsAnnotation(service, ElbID, "")
	if common.IsNotFound(err) && specifiedID != "" {
		return nil, err
	}
	if err != nil && common.IsNotFound(err) {
		subnetID, e := l.getSubnetID(service, nodes[0])
		if e != nil {
			return nil, e
		}
		loadbalancer, err = l.createLoadbalancer(clusterName, subnetID, service)
	}
	if err != nil {
		return nil, err
	}

	// query ELB listeners list
	listeners, err := l.sharedELBClient.ListListeners(&elbmodel.ListListenersRequest{LoadbalancerId: &loadbalancer.Id})
	if err != nil {
		return nil, err
	}

	for _, port := range service.Spec.Ports {
		listener := filterListenerByPort(listeners, port)
		// add or update listener
		if listener == nil {
			listener, err = l.createListener(loadbalancer.Id, service, port)
		} else {
			err = l.updateListener(listener, service)
		}
		if err != nil {
			return nil, err
		}

		listeners = popListener(listeners, listener.Id)

		// query pool or create pool
		pool, err := l.getPool(loadbalancer.Id, listener.Id)
		if err != nil && common.IsNotFound(err) {
			pool, err = l.createPool(listener, service)
		}
		if err != nil {
			return nil, err
		}

		// add new members and remove the obsolete members.
		if err = l.addOrRemoveMembers(loadbalancer, service, pool, port, nodes); err != nil {
			return nil, err
		}

		// add or remove health monitor
		if err = l.addOrRemoveHealthMonitor(loadbalancer.Id, pool, port, service); err != nil {
			return nil, err
		}
	}

	if specifiedID == "" {
		// All remaining listeners are obsolete, delete them
		err = l.deleteListeners(loadbalancer.Id, listeners)
		if err != nil {
			return nil, err
		}
	}

	ingressIP := loadbalancer.VipAddress
	publicIPAddr, err := l.createOrAssociateEIP(loadbalancer, service)
	if err == nil {
		if publicIPAddr != "" {
			ingressIP = publicIPAddr
		}

		return &corev1.LoadBalancerStatus{
			Ingress: []corev1.LoadBalancerIngress{{IP: ingressIP}},
		}, nil
	}

	// rollback
	klog.Errorf("rollback：failed to create the EIP, delete ELB instance created, error: %s", err)
	errs := []error{err}
	err = l.EnsureLoadBalancerDeleted(ctx, clusterName, service)
	if err != nil {
		errs = append(errs, err)
		klog.Errorf("rollback: error deleting ELB instance: %s", err)
	}
	return nil, errors.NewAggregate(errs)
}

func (l *SharedLoadBalancer) createOrAssociateEIP(loadbalancer *elbmodel.LoadbalancerResp, service *v1.Service) (string, error) {
	var err error
	eipID := getStringFromSvsAnnotation(service, ElbEipID, "")
	if eipID == "" {
		eipID, err = l.createEIP(service)
		if err != nil {
			return "", status.Errorf(codes.Internal, "rollback：failed to create EIP, delete ELB instance, error: %s", err)
		}
	}
	if eipID == "" {
		return "", nil
	}

	eip, err := l.eipClient.Get(eipID)
	if err != nil {
		return "", status.Errorf(codes.Internal, "rollback：failed to get EIP, delete ELB instance, error: %s", err)
	}

	if eip.PortId != nil && *eip.PortId == loadbalancer.VipPortId {
		return getEipAddress(eip)
	}

	err = l.eipClient.Bind(eipID, loadbalancer.VipPortId)
	if err != nil {
		return "", err
	}

	eip, err = l.eipClient.Get(eipID)
	if err != nil {
		return "", err
	}

	return getEipAddress(eip)
}

func getEipAddress(eip *eipmodel.PublicipShowResp) (string, error) {
	if eip.PublicIpAddress == nil {
		return "", status.Errorf(codes.Internal, "rollback: error EIP address is empty, delete ELB instance")
	}
	return *eip.PublicIpAddress, nil
}

func (l *SharedLoadBalancer) createLoadbalancer(clusterName, subnetID string, service *v1.Service) (*elbmodel.LoadbalancerResp, error) {
	name := l.GetLoadBalancerName(context.TODO(), clusterName, service)
	provider := elbmodel.GetCreateLoadbalancerReqProviderEnum().VLB
	desc := fmt.Sprintf("Created by the ELB service(%s/%s) of the k8s cluster(%s).",
		service.Namespace, service.Name, clusterName)
	loadbalancer, err := l.sharedELBClient.CreateInstanceCompleted(&elbmodel.CreateLoadbalancerReq{
		Name:        &name,
		VipSubnetId: subnetID,
		Provider:    &provider,
		Description: &desc,
	})
	if err != nil {
		return nil, err
	}
	return loadbalancer, nil
}

func (l *SharedLoadBalancer) addOrRemoveHealthMonitor(loadbalancerID string, pool *elbmodel.PoolResp, port v1.ServicePort, service *v1.Service) error {
	healthCheckOpts := getHealthCheckOptionFromAnnotation(service, l.loadbalancerOpts)
	monitorID := pool.HealthmonitorId
	klog.Infof("add or remove health check: %s : %#v", monitorID, healthCheckOpts)

	// create health monitor
	if monitorID == "" && healthCheckOpts.Enable {
		_, err := l.createHealthMonitor(loadbalancerID, pool.Id, port.Protocol, healthCheckOpts)
		return err
	}

	// update health monitor
	if monitorID != "" && healthCheckOpts.Enable {
		return l.updateHealthMonitor(monitorID, port.Protocol, healthCheckOpts)
	}

	// delete health monitor
	if monitorID != "" && !healthCheckOpts.Enable {
		klog.Infof("Deleting health monitor %s for pool %s", monitorID, pool.Id)
		err := l.sharedELBClient.DeleteHealthMonitor(monitorID)
		if err != nil {
			return fmt.Errorf("failed to delete health monitor %s for pool %s, error: %v", monitorID, pool.Id, err)
		}
	}

	return nil
}

func (l *SharedLoadBalancer) updateHealthMonitor(id string, protocol corev1.Protocol, opts *HealthCheckOption) error {
	monitorProtocol := string(protocol)
	if protocol == corev1.ProtocolUDP {
		monitorProtocol = "UDP_CONNECT"
	}

	return l.sharedELBClient.UpdateHealthMonitor(id, &elbmodel.UpdateHealthmonitorReq{
		Type:       &monitorProtocol,
		Timeout:    &opts.Timeout,
		Delay:      &opts.Delay,
		MaxRetries: &opts.MaxRetries,
	})
}

func (l *SharedLoadBalancer) createHealthMonitor(loadbalancerID, poolID string, protocol corev1.Protocol, opts *HealthCheckOption) (*elbmodel.HealthmonitorResp, error) {
	monitorProtocol := string(protocol)
	if protocol == corev1.ProtocolUDP {
		monitorProtocol = "UDP_CONNECT"
	}

	protocolType := elbmodel.CreateHealthmonitorReqType{}
	if err := protocolType.UnmarshalJSON([]byte(monitorProtocol)); err != nil {
		return nil, err
	}

	monitor, err := l.sharedELBClient.CreateHealthMonitor(&elbmodel.CreateHealthmonitorReq{
		PoolId:     poolID,
		Type:       protocolType,
		Timeout:    opts.Timeout,
		Delay:      opts.Delay,
		MaxRetries: opts.MaxRetries,
	})
	if err != nil {
		return nil, fmt.Errorf("error creating SharedLoadBalancer pool health monitor: %v", err)
	}

	loadbalancer, err := l.sharedELBClient.WaitStatusActive(loadbalancerID)
	if err != nil {
		return nil, fmt.Errorf("timeout when waiting for loadbalancer to be ACTIVE after creating member, "+
			"current provisioning status %s", loadbalancer.ProvisioningStatus)
	}
	return monitor, nil
}

func (l *SharedLoadBalancer) addOrRemoveMembers(loadbalancer *elbmodel.LoadbalancerResp, service *v1.Service, pool *elbmodel.PoolResp,
	port v1.ServicePort, nodes []*v1.Node) error {

	members, err := l.sharedELBClient.ListMembers(&elbmodel.ListMembersRequest{PoolId: pool.Id})
	if err != nil {
		return err
	}

	existsMember := make(map[string]bool)
	for _, m := range members {
		existsMember[fmt.Sprintf("%s:%d", m.Address, m.ProtocolPort)] = true
	}

	nodeNameMapping := make(map[string]*v1.Node)
	for _, node := range nodes {
		nodeNameMapping[node.Name] = node
	}

	podList, err := l.listPodsBySelector(context.TODO(), service.Namespace, service.Spec.Selector)
	if err != nil {
		return err
	}
	for _, pod := range podList.Items {
		if !IsPodActive(pod) {
			klog.Errorf("Pod %s/%s is not activated skipping adding to ELB", pod.Namespace, pod.Name)
			continue
		}

		if pod.Status.HostIP == "" {
			klog.Errorf("Pod %s/%s is not scheduled, skipping adding to ELB", pod.Namespace, pod.Name)
			continue
		}

		node, ok := nodeNameMapping[pod.Spec.NodeName]
		if !ok {
			return fmt.Errorf("could not find the node where the Pod resides, Pod: %s/%s",
				pod.Namespace, pod.Spec.NodeName)
		}

		address, err := getNodeAddress(node)
		if err != nil {
			if common.IsNotFound(err) {
				// Node failure, do not create member
				klog.Warningf("Failed to create SharedLoadBalancer pool member for node %s: %v", node.Name, err)
				continue
			} else {
				return fmt.Errorf("error getting address for node %s: %v", node.Name, err)
			}
		}

		key := fmt.Sprintf("%s:%d", address, port.NodePort)
		if existsMember[key] {
			klog.Infof("[addOrRemoveMembers] node already exists, skip adding, name: %s, address: %s, port: %d",
				node.Name, address, port.NodePort)
			members = popMember(members, address, port.NodePort)
			continue
		}

		klog.Infof("[addOrRemoveMembers] add node to pool, name: %s, address: %s, port: %d",
			node.Name, address, port.NodePort)
		// Add a member to the pool.
		if err = l.addMember(loadbalancer, pool, port, node); err != nil {
			return err
		}
		existsMember[key] = true
	}

	// delete the remaining elements in members
	for _, member := range members {
		klog.Infof("[addOrRemoveMembers] remove node from pool, name: %s, address: %s, port: %d",
			member.Name, member.Address, member.ProtocolPort)
		err = l.deleteMember(loadbalancer.Id, pool.Id, member)
		if err != nil {
			return err
		}
	}

	return nil
}

func (l *SharedLoadBalancer) addMember(loadbalancer *elbmodel.LoadbalancerResp, pool *elbmodel.PoolResp, port v1.ServicePort, node *v1.Node) error {
	klog.Infof("Add a member(%s) to pool %s", node.Name, pool.Id)
	address, err := getNodeAddress(node)
	if err != nil {
		return err
	}

	name := cutString(fmt.Sprintf("member_%s_%s", pool.Name, node.Name))
	_, err = l.sharedELBClient.AddMember(pool.Id, &elbmodel.CreateMemberReq{
		Name:         &name,
		ProtocolPort: port.NodePort,
		SubnetId:     loadbalancer.VipSubnetId,
		Address:      address,
	})
	if err != nil {
		return fmt.Errorf("error creating SharedLoadBalancer pool member for node: %s, %v", node.Name, err)
	}

	loadbalancer, err = l.sharedELBClient.WaitStatusActive(loadbalancer.Id)
	if err != nil {
		return fmt.Errorf("timeout when waiting for loadbalancer to be ACTIVE after adding members, "+
			"current status %s", loadbalancer.ProvisioningStatus)
	}

	return nil
}

func (l *SharedLoadBalancer) deleteMember(elbID string, poolID string, member elbmodel.MemberResp) error {
	klog.V(4).Infof("Deleting obsolete member %s for pool %s address %s", member.Id, poolID, member.Address)
	err := l.sharedELBClient.DeleteMember(poolID, member.Id)
	if err != nil && !common.IsNotFound(err) {
		return fmt.Errorf("error deleting obsolete member %s for pool %s address %s: %v",
			poolID, member.Id, member.Address, err)
	}
	loadbalancer, err := l.sharedELBClient.WaitStatusActive(elbID)
	if err != nil {
		return fmt.Errorf("timeout when waiting for loadbalancer to be ACTIVE after creating member, "+
			"current provisioning status %s", loadbalancer.ProvisioningStatus)
	}
	return nil
}

func (l *SharedLoadBalancer) getPool(elbID, listenerID string) (*elbmodel.PoolResp, error) {
	pools, err := l.sharedELBClient.ListPools(&elbmodel.ListPoolsRequest{
		LoadbalancerId: &elbID,
	})
	if err != nil {
		return nil, err
	}

	for _, p := range pools {
		for _, lis := range p.Listeners {
			if lis.Id == listenerID {
				return &p, nil
			}
		}
	}
	return nil, status.Errorf(codes.NotFound, "not found pool matched ListenerId: %s, ELB ID: %s", listenerID, elbID)
}

func (l *SharedLoadBalancer) getSessionAffinity(service *v1.Service) *elbmodel.SessionPersistence {
	globalOpts := l.loadbalancerOpts
	sessionMode := getStringFromSvsAnnotation(service, ElbSessionAffinityMode, globalOpts.SessionAffinityMode)
	if sessionMode == "" {
		return nil
	}
	sessionType := elbmodel.SessionPersistenceType{}
	err := sessionType.UnmarshalJSON([]byte(sessionMode))
	if err != nil {
		klog.Warningf("\"kubernetes.io/elb.session-affinity-mode\" is invalid, does not enable session affinity")
		return nil
	}

	persistence := elbmodel.SessionPersistence{
		Type: sessionType,
	}
	opts := getStringFromSvsAnnotation(service, ElbSessionAffinityOption, globalOpts.SessionAffinityOption)

	err = json.Unmarshal([]byte(opts), &persistence)
	if err != nil {
		klog.Warningf("error parsing \"kubernetes.io/elb.session-affinity-option\": %s, ignore options: %s",
			err, opts)
	}
	printSessionAffinity(service, persistence)
	return &persistence
}

func printSessionAffinity(service *v1.Service, per elbmodel.SessionPersistence) {
	cookieName := ""
	if per.CookieName != nil {
		cookieName = *per.CookieName
	}
	timeout := int32(0)
	if per.PersistenceTimeout != nil {
		timeout = *per.PersistenceTimeout
	}

	klog.V(4).Infof("[DEBUG] service name: %s/%s, SessionAffinity: { mode: %s, CookieName: %s, "+
		"PersistenceTimeout: %d min }", service.Namespace, service.Name, per.Type.Value(), cookieName, timeout)
}

func (l *SharedLoadBalancer) createPool(listener *elbmodel.ListenerResp, service *v1.Service) (*elbmodel.PoolResp, error) {
	lbAlgorithm := getStringFromSvsAnnotation(service, ElbAlgorithm, l.loadbalancerOpts.LBAlgorithm)

	persistence := l.getSessionAffinity(service)

	protocol := elbmodel.CreatePoolReqProtocol{}
	if err := protocol.UnmarshalJSON([]byte(listener.Protocol.Value())); err != nil {
		return nil, err
	}

	name := fmt.Sprintf("pl_%s", listener.Name)
	return l.sharedELBClient.CreatePool(&elbmodel.CreatePoolReq{
		Name:               &name,
		Protocol:           protocol,
		LbAlgorithm:        lbAlgorithm,
		ListenerId:         &listener.Id,
		SessionPersistence: persistence,
	})
}

func popMember(members []elbmodel.MemberResp, addr string, port int32) []elbmodel.MemberResp {
	for i, m := range members {
		if m.Address == addr && m.ProtocolPort == port {
			members[i] = members[len(members)-1]
			members = members[:len(members)-1]
		}
	}
	return members
}

func popListener(arr []elbmodel.ListenerResp, id string) []elbmodel.ListenerResp {
	for i, lis := range arr {
		if lis.Id == id {
			arr[i] = arr[len(arr)-1]
			arr = arr[:len(arr)-1]
			break
		}
	}
	return arr
}

func (l *SharedLoadBalancer) deleteListeners(elbID string, listeners []elbmodel.ListenerResp) error {
	errs := make([]error, 0)
	for _, lis := range listeners {
		pool, err := l.getPool(elbID, lis.Id)
		if err != nil && !common.IsNotFound(err) {
			errs = append(errs, err)
			continue
		}
		if err == nil {
			delErrs := l.deletePool(pool)
			if len(delErrs) > 0 {
				errs = append(errs, delErrs...)
			}
		}
		// delete ELB listener
		if err = l.sharedELBClient.DeleteListener(elbID, lis.Id); err != nil && !common.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to delete ELB listener %s : %s ", lis.Id, err))
		}
	}

	if len(errs) != 0 {
		return fmt.Errorf("failed to delete listeners: %s", errors.NewAggregate(errs))
	}

	return nil
}

func (l *SharedLoadBalancer) deletePool(pool *elbmodel.PoolResp) []error {
	errs := make([]error, 0)
	// delete all members of pool
	if err := l.sharedELBClient.DeleteAllPoolMembers(pool.Id); err != nil {
		errs = append(errs, err)
	}
	// delete the pool monitor if exists
	if err := l.sharedELBClient.DeleteHealthMonitor(pool.HealthmonitorId); err != nil && !common.IsNotFound(err) {
		errs = append(errs, err)
	}
	// delete ELB listener pool
	if err := l.sharedELBClient.DeletePool(pool.Id); err != nil && !common.IsNotFound(err) {
		errs = append(errs, err)
	}
	return errs
}

func (l *SharedLoadBalancer) createListener(loadbalancerID string, service *v1.Service, port v1.ServicePort) (*elbmodel.ListenerResp, error) {
	protocol := elbmodel.CreateListenerReqProtocol{}
	if err := protocol.UnmarshalJSON([]byte(port.Protocol)); err != nil {
		return nil, err
	}

	xForwardFor := getBoolFromSvsAnnotation(service, ElbXForwardedHost, false)
	connectLimit := getConnectionLimitFromAnnotation(service)
	name := cutString(fmt.Sprintf("%s_%s_%v", service.Name, port.Protocol, port.Port))
	listener, err := l.sharedELBClient.CreateListener(&elbmodel.CreateListenerReq{
		LoadbalancerId:  loadbalancerID,
		Protocol:        protocol,
		ProtocolPort:    port.Port,
		Name:            &name,
		InsertHeaders:   &elbmodel.InsertHeader{XForwardedHost: &xForwardFor},
		ConnectionLimit: pointer.Int32Ptr(int32(connectLimit)),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to create listener for loadbalancer %s: %v",
			loadbalancerID, err)
	}

	return listener, nil
}

func (l *SharedLoadBalancer) updateListener(listener *elbmodel.ListenerResp, service *v1.Service) error {
	connectLimit := getConnectionLimitFromAnnotation(service)
	if int32(connectLimit) == listener.ConnectionLimit {
		return nil
	}

	err := l.sharedELBClient.UpdateListener(listener.Id, &elbmodel.UpdateListenerReq{
		ConnectionLimit: pointer.Int32Ptr(int32(connectLimit)),
	})
	if err != nil {
		return err
	}

	klog.Infof("Listener updated, id: %s, name: %s", listener.Id, listener.Name)
	return nil
}

func filterListenerByPort(listenerArr []elbmodel.ListenerResp, port v1.ServicePort) *elbmodel.ListenerResp {
	for _, l := range listenerArr {
		if l.Protocol.Value() == string(port.Protocol) && l.ProtocolPort == port.Port {
			return &l
		}
	}

	return nil
}

func getConnectionLimitFromAnnotation(service *v1.Service) int {
	connLimitStr := getStringFromSvsAnnotation(service, ElbConnectionLimit, "-1")
	connectLimit, err := strconv.Atoi(connLimitStr)
	if err != nil {
		klog.Warningf("Could not parse int value from '%s' error: %s, failing back to default", connLimitStr, err)
		connectLimit = -1
	}
	return connectLimit
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
func (l *SharedLoadBalancer) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	klog.Infof("UpdateLoadBalancer: called with service %s/%s, node: %d", service.Namespace, service.Name, len(nodes))
	// get exits or create a new ELB instance
	loadbalancer, err := l.getLoadBalancerInstance(ctx, clusterName, service)
	if err != nil {
		return err
	}

	// query ELB listeners list
	listeners, err := l.sharedELBClient.ListListeners(&elbmodel.ListListenersRequest{LoadbalancerId: &loadbalancer.Id})
	if err != nil {
		return err
	}

	for _, port := range service.Spec.Ports {
		listener := filterListenerByPort(listeners, port)
		if listener == nil {
			return status.Errorf(codes.Unavailable, "error, can not find a listener matching %s:%v",
				port.Protocol, port.Port)
		}

		// query pool or create pool
		pool, err := l.getPool(loadbalancer.Id, listener.Id)
		if err != nil && common.IsNotFound(err) {
			pool, err = l.createPool(listener, service)
		}
		if err != nil {
			return err
		}

		// add new members and remove the obsolete members.
		if err = l.addOrRemoveMembers(loadbalancer, service, pool, port, nodes); err != nil {
			return err
		}

		// add or remove health monitor
		if err = l.addOrRemoveHealthMonitor(loadbalancer.Id, pool, port, service); err != nil {
			return err
		}
	}
	return nil
}

// EnsureLoadBalancerDeleted deletes the specified load balancer
func (l *SharedLoadBalancer) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	klog.Infof("EnsureLoadBalancerDeleted: called with service %s/%s", service.Namespace, service.Name)
	serviceName := fmt.Sprintf("%s/%s", service.Namespace, service.Name)
	klog.Infof("EnsureLoadBalancerDeleted(%s, %s)", clusterName, serviceName)

	loadBalancer, err := l.getLoadBalancerInstance(ctx, clusterName, service)
	if err != nil {
		if common.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}

	specifiedID := getStringFromSvsAnnotation(service, ElbID, "")
	if specifiedID != "" {
		err = l.deleteListener(loadBalancer, service)
	} else {
		err = l.deleteELBInstance(loadBalancer, service)
	}

	if err != nil {
		return err
	}
	return nil
}

func (l *SharedLoadBalancer) deleteListener(loadBalancer *elbmodel.LoadbalancerResp, service *v1.Service) error {
	// query ELB listeners list
	listenerArr, err := l.sharedELBClient.ListListeners(&elbmodel.ListListenersRequest{
		LoadbalancerId: &loadBalancer.Id,
	})
	if err != nil {
		return err
	}

	listenersMatched := make([]elbmodel.ListenerResp, 0)
	for _, port := range service.Spec.Ports {
		listener := filterListenerByPort(listenerArr, port)
		if listener != nil {
			listenersMatched = append(listenersMatched, *listener)
		}
	}

	if err = l.deleteListeners(loadBalancer.Id, listenersMatched); err != nil {
		return err
	}
	return nil
}

func (l *SharedLoadBalancer) deleteELBInstance(loadBalancer *elbmodel.LoadbalancerResp, service *v1.Service) error {
	// query ELB listeners list
	listenerArr, err := l.sharedELBClient.ListListeners(&elbmodel.ListListenersRequest{
		LoadbalancerId: &loadBalancer.Id,
	})
	if err != nil {
		return err
	}

	if err = l.deleteListeners(loadBalancer.Id, listenerArr); err != nil {
		return err
	}

	eipID := getStringFromSvsAnnotation(service, ElbEipID, "")
	keepEip := getBoolFromSvsAnnotation(service, ELBKeepEip, l.loadbalancerOpts.KeepEIP)
	if err = unbindEIP(l.eipClient, loadBalancer, eipID, keepEip); err != nil {
		return err
	}
	if err = l.sharedELBClient.DeleteInstance(loadBalancer.Id); err != nil {
		return err
	}
	return nil
}

func unbindEIP(eipClient *wrapper.EIpClient, loadBalancer *elbmodel.LoadbalancerResp, eipID string, keepEIP bool) error {
	if eipID == "" {
		ips, err := eipClient.List(&eipmodel.ListPublicipsRequest{
			PortId: &[]string{loadBalancer.VipPortId},
		})

		if err != nil {
			return err
		}
		if len(ips) == 0 {
			return nil
		}
		eipID = *ips[0].Id
	}

	if err := eipClient.Unbind(eipID); err != nil {
		return err
	}
	if keepEIP {
		return nil
	}
	if err := eipClient.Delete(eipID); err != nil {
		return err
	}
	return nil
}

// cutString makes sure the string length doesn't exceed 255, which is usually the maximum string length in HuaweiCloud.
func cutString(original string) string {
	ret := original
	if len(original) > 255 {
		ret = original[:255]
	}
	return ret
}

func getStringFromSvsAnnotation(service *corev1.Service, annotationKey string, defaultSetting string) string {
	if annotationValue, ok := service.Annotations[annotationKey]; ok {
		klog.V(4).Infof("Found a Service Annotation: %v = %v", annotationKey, annotationValue)
		return annotationValue
	}
	klog.V(4).Infof("Could not find a Service Annotation; falling back on cloud-authConfig setting: %v = %v",
		annotationKey, defaultSetting)
	return defaultSetting
}

func getBoolFromSvsAnnotation(service *corev1.Service, annotationKey string, defaultVal bool) bool {
	valueStr, ok := service.Annotations[annotationKey]
	if !ok {
		return defaultVal
	}

	rstValue := false
	switch valueStr {
	case "true":
		rstValue = true
	case "false":
		rstValue = false
	default:
		klog.Warningf("unknown %s annotation: %v, specify \"true\" or \"false\" ", annotationKey, valueStr)
		rstValue = defaultVal
	}
	return rstValue
}

func (l *SharedLoadBalancer) getNodeSubnetID(node *corev1.Node) (string, error) {
	ipAddress, err := getNodeAddress(node)
	if err != nil {
		return "", err
	}

	instance, err := l.ecsClient.GetByName(node.Name)
	if err != nil {
		return "", err
	}

	interfaces, err := l.ecsClient.ListInterfaces(&ecsmodel.ListServerInterfacesRequest{ServerId: instance.Id})
	if err != nil {
		return "", err
	}

	for _, intfs := range interfaces {
		for _, fixedIP := range *intfs.FixedIps {
			if *fixedIP.IpAddress == ipAddress {
				return *fixedIP.SubnetId, nil
			}
		}
	}

	return "", fmt.Errorf("failed to get node subnet ID")
}

func getNodeAddress(node *corev1.Node) (string, error) {
	addresses := node.Status.Addresses
	if len(addresses) == 0 {
		return "", status.Errorf(codes.NotFound, "error, current node do not have addresses, nodeName: %s",
			node.Name)
	}

	for _, addr := range addresses {
		if _, ok := allowedIPTypes[addr.Type]; ok {
			return addr.Address, nil
		}
	}
	return "", status.Errorf(codes.NotFound, "error, current node do not have any valid addresses, nodeName: %s",
		node.Name)
}

type HealthCheckOption struct {
	Enable     bool   `json:"enable"`
	Delay      int32  `json:"delay"`
	Timeout    int32  `json:"timeout"`
	MaxRetries int32  `json:"max_retries"`
	Protocol   string `json:"protocol"`
	Path       string `json:"path"`
}

func getHealthCheckOptionFromAnnotation(service *v1.Service, opts *config.LoadBalancerOptions) *HealthCheckOption {
	checkOpts := HealthCheckOption{
		Enable:     true,
		Timeout:    3,
		MaxRetries: 3,
		Delay:      5,
	}

	healthCheck := getStringFromSvsAnnotation(service, ElbHealthCheck, opts.HealthCheck)
	if healthCheck == "off" {
		checkOpts.Enable = false
	}

	str := getStringFromSvsAnnotation(service, ElbHealthCheckOptions, opts.HealthCheckOption)
	if err := json.Unmarshal([]byte(str), &checkOpts); err != nil {
		klog.Errorf("error parsing health check options: %s, using default", err)
	}
	return &checkOpts
}

func (l *SharedLoadBalancer) getSubnetID(service *v1.Service, node *v1.Node) (string, error) {
	subnetID := getStringFromSvsAnnotation(service, ElbSubnetID, l.cloudConfig.VpcOpts.SubnetID)
	if subnetID != "" {
		return subnetID, nil
	}

	subnetID, err := l.getNodeSubnetID(node)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "missing subnet-id, "+
			"can not to read subnet-id from the node also, error: %s", err)
	}
	return subnetID, nil
}

type CreateEIPOptions struct {
	BandwidthSize int32  `json:"bandwidth_size"`
	ShareType     string `json:"share_type"`
	ShareID       string `json:"share_id"`

	IPType string `json:"ip_type"`
}

func parseEIPAutoCreateOptions(service *v1.Service) (*CreateEIPOptions, error) {
	str := getStringFromSvsAnnotation(service, AutoCreateEipOptions, "")
	if str == "" {
		return nil, nil
	}

	opts := &CreateEIPOptions{}
	err := json.Unmarshal([]byte(str), opts)
	return opts, err
}

func (l *SharedLoadBalancer) createEIP(service *v1.Service) (string, error) {
	opts, err := parseEIPAutoCreateOptions(service)
	if err != nil || opts == nil {
		return "", err
	}

	shareType := eipmodel.CreatePublicipBandwidthOptionShareType{}
	err = shareType.UnmarshalJSON([]byte(opts.ShareType))
	if err != nil {
		return "", err
	}

	eip, err := l.eipClient.Create(&eipmodel.CreatePublicipRequestBody{
		Bandwidth: &eipmodel.CreatePublicipBandwidthOption{
			Name:      &service.Name,
			Id:        &opts.ShareID,
			Size:      &opts.BandwidthSize,
			ShareType: shareType,
		},
		Publicip: &eipmodel.CreatePublicipOption{Type: opts.IPType},
	})
	if err != nil {
		return "", err
	}

	return *eip.Id, nil
}
