/*
Copyright 2020 The Kubernetes Authors.

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

// nolint:golint // stop check lint issues as this file will be refactored
package huaweicloud

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
)

const (
	AnnotationsNATID string = "kubernetes.io/natgateway.id"
)

const (
	ClusterID string = "CLUSTER_ID"
)

type NATCloud struct {
	Basic
}

/*
 *    >>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>
 *    NAT implement of functions in cloud.go, including
 *               GetLoadBalancer()
 *               GetLoadBalancerName()
 *               EnsureLoadBalancer()
 *               UpdateLoadBalancer()
 *               EnsureLoadBalancerDeleted()
 *    >>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>
 */

func (nat *NATCloud) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	status = &v1.LoadBalancerStatus{}
	natClient, err := nat.getNATClient()
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}

		return nil, false, err
	}

	//get dnat rules binded to the dnat instance
	natGatewayId := service.ObjectMeta.Annotations[AnnotationsNATID]
	if natGatewayId == "" {
		return nil, false, fmt.Errorf("The id of natGateway should be set by %v in annotations ", AnnotationsNATID)
	}
	dnatRuleList, err := listDnatRule(natClient, natGatewayId)
	if err != nil {
		return nil, false, err
	}

	if len(dnatRuleList.DNATRules) == 0 {
		return nil, false, nil
	}

	for _, externalPort := range service.Spec.Ports {
		//check if the DNAT rule exists
		if nat.getDNATRule(dnatRuleList, &externalPort) == nil {
			return nil, false, nil
		}
	}
	status.Ingress = append(status.Ingress, v1.LoadBalancerIngress{IP: service.Spec.LoadBalancerIP})
	return status, true, nil
}

/*
 *    Not implemented
 */
func (nat *NATCloud) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	return ""
}

/*
 *    clusterName: discarded
 *    service: each service has its corresponding DNAT rule
 *    nodes: all nodes under ServiceController, i.e. all nodes under the k8s cluster
 */
func (nat *NATCloud) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, hosts []*v1.Node) (*v1.LoadBalancerStatus, error) {
	status := &v1.LoadBalancerStatus{}

	// step 0: ensure the nat gateway is exist
	natProvider, err := nat.getNATClient()
	if err != nil {
		return nil, err
	}

	natGatewayId := service.ObjectMeta.Annotations[AnnotationsNATID]
	if natGatewayId == "" {
		return nil, fmt.Errorf("The id of natGateway should be set by %v in annotations ", AnnotationsNATID)
	}

	natGateway, err := natProvider.GetNATGateway(natGatewayId)
	if err != nil {
		return nil, err
	}

	if natGateway.RouterId != nat.cloudConfig.VpcOpts.ID {
		return nil, fmt.Errorf("The natGateway is not in the same VPC with cluster. ")
	}

	//step 1:get floatingip id by floatingip address and check the floatingIp can be used
	dnatRuleList, err := listDnatRule(natProvider, natGatewayId)
	if err != nil {
		return nil, err
	}

	floatingIp, err := nat.getFloatingIpInfoByIp(natProvider, service.Spec.LoadBalancerIP)
	if err != nil {
		return nil, err
	}

	allDnatRuleInFloatIP, err := listAllDnatRuleByFloatIP(natProvider, service.Spec.LoadBalancerIP)
	if err != nil {
		return nil, err
	}

	if !nat.checkFloatingIp(allDnatRuleInFloatIP, floatingIp, natGatewayId) {
		return nil, fmt.Errorf("The floating ip %v is binding to port,and its not DNAT rule in natGateway %s", floatingIp.FloatingIpAddress, natGateway.Name)
	}

	//step 2: get podList (with labels/selectors of this service),then get the backend to create DNAT rule
	podList, err := nat.getPods(service.Name, service.Namespace)
	if err != nil {
		return nil, err
	}

	var runningPod v1.Pod
	for _, pod := range podList.Items {
		if podutil.IsPodReady(&pod) {
			runningPod = pod
			break
		}
	}
	if len(runningPod.Status.HostIP) == 0 {
		return nil, fmt.Errorf("There is no availabel endpoint for the service %s", service.Name)
	}

	subnetId := nat.getSubnetIdForPod(runningPod, hosts)
	netPort, err := nat.getPortByFixedIp(natProvider, subnetId, runningPod.Status.HostIP)
	if err != nil {
		return nil, err
	}
	var errs []error
	// step1: create dnat rule
	for _, port := range service.Spec.Ports {
		//check if the DNAT rule has been created by the service,if exists continue
		if nat.getDNATRule(dnatRuleList, &port) != nil {
			klog.V(4).Infoln("DNAT rule already exists, no need to create")
			continue
		}

		klog.V(4).Infof("port:%v dnat rule not exist,start create dnat rule", port)

		err := nat.ensureCreateDNATRule(natProvider, &port, netPort, floatingIp, natGatewayId)
		if err != nil {
			errs = append(errs, fmt.Errorf("EnsureCreateDNATRule Failed: %v", err))
			continue
		}
	}

	// get service with loadbalancer type and loadbalancer ip
	lbServers, _ := nat.kubeClient.Services("").List(context.TODO(), metav1.ListOptions{})
	var lbPorts []v1.ServicePort
	for _, svc := range lbServers.Items {
		lbType := svc.Annotations[ElbClass]
		if lbType != "dnat" || svc.Spec.LoadBalancerIP != service.Spec.LoadBalancerIP {
			continue
		}
		klog.V(4).Infof("exist dnat svc:%v", svc)
		lbPorts = append(lbPorts, svc.Spec.Ports...)
	}

	for _, dnatRule := range dnatRuleList.DNATRules {
		if dnatRule.FloatingIpAddress != service.Spec.LoadBalancerIP {
			continue
		}

		if nat.getServicePort(&dnatRule, lbPorts) != nil {
			klog.V(4).Infoln("port exist,no need to delete")
			continue
		}

		klog.V(4).Infof("rule:%v port not exist,start delete dnat rule", dnatRule)

		err := nat.ensureDeleteDNATRule(natProvider, &dnatRule, natGatewayId)
		if err != nil {
			errs = append(errs, fmt.Errorf("EnsureDeleteDNATRule Failed: %v", err))
			continue
		}
	}

	if len(errs) != 0 {
		return nil, utilerrors.NewAggregate(errs)
	}
	status.Ingress = append(status.Ingress, v1.LoadBalancerIngress{IP: service.Spec.LoadBalancerIP})
	return status, nil
}

func (nat *NATCloud) getServicePort(dnatRule *DNATRule, ports []v1.ServicePort) *v1.ServicePort {
	for _, port := range ports {
		if dnatRule.ExternalServicePort == port.Port &&
			dnatRule.InternalServicePort == port.NodePort &&
			strings.EqualFold(string(dnatRule.Protocol), string(port.Protocol)) {
			return &port
		}
	}
	return nil
}

func listDnatRule(natProvider *NATClient, natGatewayId string) (*DNATRuleList, error) {
	params := map[string]string{"nat_gateway_id": natGatewayId}
	dnatRuleList, err := natProvider.ListDNATRules(params)
	if err != nil {
		return nil, err
	}
	clusterID := os.Getenv(ClusterID)
	var distList DNATRuleList
	for _, rule := range dnatRuleList.DNATRules {
		if rule.Description != "" {
			desc := getDNATRuleDescription(rule.Description)
			if desc != nil {
				if desc.ClusterID == clusterID {
					distList.DNATRules = append(distList.DNATRules, rule)
				}
			}
		}
	}
	return &distList, nil
}

func listAllDnatRuleByFloatIP(natProvider *NATClient, floatIP string) (*DNATRuleList, error) {
	params := map[string]string{"floating_ip_address": floatIP}
	dnatRuleList, err := natProvider.ListDNATRules(params)
	if err != nil {
		return nil, err
	}
	return dnatRuleList, nil
}

// update members in the service
//
//	(1) find the previous DNATRule
//	(2) check whether the node whose port set in the rule is health
//	(3) if not health delete the previous and create a new one
func (nat *NATCloud) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	natProvider, err := nat.getNATClient()
	if err != nil {
		return err
	}

	natGatewayId := service.ObjectMeta.Annotations[AnnotationsNATID]
	if natGatewayId == "" {
		return fmt.Errorf("The id of natGateway should be set by %v in annotations ", AnnotationsNATID)
	}

	natGateway, err := natProvider.GetNATGateway(natGatewayId)
	if err != nil {
		return err
	}

	if natGateway.RouterId != nat.cloudConfig.VpcOpts.ID {
		return fmt.Errorf("The natGateway is not in the same VPC with cluster. ")
	}

	//get floatingip id by floatingip address and check if it can be used
	dnatRuleList, err := listDnatRule(natProvider, natGatewayId)
	if err != nil {
		return err
	}

	floatingIp, err := nat.getFloatingIpInfoByIp(natProvider, service.Spec.LoadBalancerIP)
	if err != nil {
		return err
	}

	allDnatRuleInFloatIP, err := listAllDnatRuleByFloatIP(natProvider, service.Spec.LoadBalancerIP)
	if err != nil {
		return err
	}

	if !nat.checkFloatingIp(allDnatRuleInFloatIP, floatingIp, natGatewayId) {
		return fmt.Errorf("The floating ip %v is binding to port,and its not DNAT rule in natGateway %s ", floatingIp.FloatingIpAddress, natGateway.Name)
	}

	podList, err := nat.getPods(service.Name, service.Namespace)
	if err != nil {
		return err
	}
	var runningPod v1.Pod
	for _, pod := range podList.Items {
		if podutil.IsPodReady(&pod) {
			runningPod = pod
			break
		}
	}
	var errs []error
	if len(runningPod.Status.HostIP) == 0 {
		klog.V(4).Infof("Delete all DNAT Rule if there is no available endpoint for service %s", service.Name)
		for _, servicePort := range service.Spec.Ports {
			dnatRule := nat.getDNATRule(dnatRuleList, &servicePort)
			if dnatRule != nil {
				if err = nat.ensureDeleteDNATRule(natProvider, dnatRule, natGatewayId); err != nil {
					errs = append(errs, fmt.Errorf("UpdateDNATRule Failed: %v", err))
					continue
				}
			}
		}
		if len(errs) != 0 {
			return utilerrors.NewAggregate(errs)
		}
		return nil
	}

	subnetId := nat.getSubnetIdForPod(runningPod, nodes)
	netPort, err := nat.getPortByFixedIp(natProvider, subnetId, runningPod.Status.HostIP)
	if err != nil {
		return err
	}
	for _, servicePort := range service.Spec.Ports {
		dnatRule := nat.getDNATRule(dnatRuleList, &servicePort)
		if dnatRule != nil {
			networkPort, err := natProvider.GetPort(dnatRule.PortId)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if len(networkPort.FixedIps) == 0 {
				errs = append(errs, fmt.Errorf("The port has no ipAddress binded "))
				continue
			}
			node, err := nat.kubeClient.Nodes().Get(context.TODO(), networkPort.FixedIps[0].IpAddress, metav1.GetOptions{})
			if err != nil {
				klog.Errorf("Get node(%s) error: %v", networkPort.FixedIps[0].IpAddress, err)
				continue
			}
			status, err := CheckNodeHealth(node)
			if !status || err != nil {
				klog.Warningf("The node %v is not ready. %v", node.Name, err)
				if err = nat.ensureDeleteDNATRule(natProvider, dnatRule, natGatewayId); err != nil {
					errs = append(errs, fmt.Errorf("UpdateDNATRule Failed: %v", err))
					continue
				}
			}
			if status {
				klog.V(4).Infof("The status of node %s is normal,no need to update DnatRule", node.Name)
				continue
			}
		}

		if err = nat.ensureCreateDNATRule(natProvider, &servicePort, netPort, floatingIp, natGateway.Id); err != nil {
			errs = append(errs, fmt.Errorf("UpdateDNATRule Failed: %v", err))
			continue
		}

	}

	if len(errs) != 0 {
		return utilerrors.NewAggregate(errs)
	}
	return nil
}

// delete all DNATRules under a service
//
//	(1) find the DNAT rules of the service
//	(2) delete the DNAT rule
func (nat *NATCloud) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	natProvider, err := nat.getNATClient()
	if err != nil {
		return err
	}
	natGatewayId := service.ObjectMeta.Annotations[AnnotationsNATID]
	if natGatewayId == "" {
		return fmt.Errorf("The id of natGateway should be set by %v in annotations ", AnnotationsNATID)
	}
	dnatRuleList, err := listDnatRule(natProvider, natGatewayId)
	if err != nil {
		return err
	}
	var errs []error
	for _, servicePort := range service.Spec.Ports {
		dnatRule := nat.getDNATRule(dnatRuleList, &servicePort)
		if dnatRule != nil {
			err := nat.ensureDeleteDNATRule(natProvider, dnatRule, natGatewayId)
			if err != nil {
				errs = append(errs, err)
				continue
			}
		}
	}
	if len(errs) != 0 {
		return utilerrors.NewAggregate(errs)
	}
	return nil
}

/*
 *    >>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>
 *               Util function
 *    >>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>
 */
func (nat *NATCloud) getNATClient() (*NATClient, error) {
	authOpts := nat.cloudConfig.AuthOpts
	return NewNATClient(authOpts.Cloud, authOpts.Region, authOpts.ProjectID, authOpts.AccessKey, authOpts.SecretKey), nil
}

func (nat *NATCloud) getPods(name, namespace string) (*v1.PodList, error) {
	service, err := nat.kubeClient.Services(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	if len(service.Spec.Selector) == 0 {
		return nil, fmt.Errorf("the service %s has no selector to associate the pods", name)
	}

	set := labels.Set(service.Spec.Selector)
	labelSelector := labels.SelectorFromSet(set)

	opts := metav1.ListOptions{LabelSelector: labelSelector.String()}
	return nat.kubeClient.Pods(namespace).List(context.TODO(), opts)
}

func genDNATRuleDescription() string {
	desc := &DNATRuleDescription{
		ClusterID:   os.Getenv(ClusterID),
		Description: Attention,
	}
	tmp, _ := json.Marshal(desc)
	return string(tmp)
}

func getDNATRuleDescription(desc string) *DNATRuleDescription {
	var description DNATRuleDescription
	err := json.Unmarshal([]byte(desc), &description)
	if err != nil {
		return nil
	}
	return &description
}

func (nat *NATCloud) ensureCreateDNATRule(natProvider *NATClient, port *v1.ServicePort, netPort *Port, floatingIp *FloatingIp, natGatewayId string) error {
	dnatRuleConf := &DNATRule{
		NATGatewayId:        natGatewayId,
		PortId:              netPort.Id,
		InternalServicePort: port.NodePort,
		FloatingIpId:        floatingIp.Id,
		ExternalServicePort: port.Port,
		Protocol:            NATProtocol(port.Protocol),
		Description:         genDNATRuleDescription(),
	}

	_, err := natProvider.CreateDNATRule(dnatRuleConf)
	if err != nil {
		return err
	}
	return nil
}

// 1.delete the old dnatRule
// 2.get the new port id
// 3.create a new dnatRule
func (nat *NATCloud) ensureDeleteDNATRule(natProvider *NATClient, dnatRule *DNATRule, natGatewayId string) error {
	klog.V(4).Infoln("Delete the DNAT Rule when the node is not ready", dnatRule.FloatingIpAddress+":"+fmt.Sprint(dnatRule.ExternalServicePort))
	err := natProvider.DeleteDNATRule(dnatRule.Id, natGatewayId)
	if err != nil {
		return err
	}

	return wait.Poll(100*time.Millisecond, 5*time.Second, func() (bool, error) {
		return !nat.checkDNATRuleById(natProvider, dnatRule.Id), nil
	})
}

func (nat *NATCloud) checkFloatingIp(dnatRuleList *DNATRuleList, floatingIp *FloatingIp, natGatewayId string) (available bool) {
	if floatingIp.PortId == "" {
		return true
	}

	if len(dnatRuleList.DNATRules) != 0 && dnatRuleList.DNATRules[0].NATGatewayId == natGatewayId {
		return true
	}
	return false
}

func (nat *NATCloud) getDNATRule(dnatRuleList *DNATRuleList, port *v1.ServicePort) *DNATRule {
	for _, dnatRule := range dnatRuleList.DNATRules {
		if dnatRule.ExternalServicePort == port.Port &&
			dnatRule.InternalServicePort == port.NodePort &&
			strings.EqualFold(string(dnatRule.Protocol), string(port.Protocol)) {
			return &dnatRule
		}
	}
	return nil
}

func (nat *NATCloud) checkDNATRuleById(natProvider *NATClient, dnatRuleId string) (exist bool) {
	_, err := natProvider.GetDNATRule(dnatRuleId)
	if err != nil && strings.Contains(err.Error(), "No DNAT rule exist") {
		return false
	}
	return true
}

func (nat *NATCloud) getFloatingIpInfoByIp(natProvider *NATClient, ip string) (*FloatingIp, error) {
	listparams := make(map[string]string)
	listparams["floating_ip_address"] = ip
	floatingIpList, err := natProvider.ListFloatings(listparams)
	if err != nil {
		return nil, err
	}
	if len(floatingIpList.FloatingIps) == 0 {
		return nil, fmt.Errorf("The floating ip %v is not exist", ip)
	}
	return &floatingIpList.FloatingIps[0], nil
}

func (nat *NATCloud) getPortByFixedIp(natProvider *NATClient, subnetId string, fixedIp string) (*Port, error) {
	listparams := make(map[string]string)
	listparams["network_id"] = subnetId
	listparams["fixed_ips=ip_address"] = fixedIp
	netPortList, err := natProvider.ListPorts(listparams)
	if err != nil {
		return nil, err
	}
	if len(netPortList.Ports) == 0 {
		return nil, fmt.Errorf("The port with fixed ip %s is not exist ", fixedIp)
	}
	return &netPortList.Ports[0], nil
}

func (nat *NATCloud) getSubnetIdForPod(pod v1.Pod, nodes []*v1.Node) string {
	var (
		nodeRunningPod *v1.Node
		subnetId       string
	)

	for _, node := range nodes {
		for _, address := range node.Status.Addresses {
			if address.Type == v1.NodeInternalIP && address.Address == pod.Status.HostIP {
				nodeRunningPod = node
			}
		}
		if nodeRunningPod != nil {
			break
		}
	}

	subnetId = nat.cloudConfig.VpcOpts.SubnetID
	if nodeRunningPod != nil {
		nodeSubnetId, ok := nodeRunningPod.Labels[NodeSubnetIDLabelKey]
		if ok {
			subnetId = nodeSubnetId
		}
	}

	return subnetId
}

// if the node not health, it will not be added to ELB
func CheckNodeHealth(node *v1.Node) (bool, error) {
	conditionMap := make(map[v1.NodeConditionType]*v1.NodeCondition)
	for i := range node.Status.Conditions {
		cond := node.Status.Conditions[i]
		conditionMap[cond.Type] = &cond
	}

	status := false
	if condition, ok := conditionMap[v1.NodeReady]; ok {
		if condition.Status == v1.ConditionTrue {
			status = true
		} else {
			status = false
		}
	}

	if node.Spec.Unschedulable {
		status = false
	}

	return status, nil
}

func GetHealthCheckPort(service *v1.Service) *v1.ServicePort {
	for _, port := range service.Spec.Ports {
		if port.Name == HealthzCCE {
			return &port
		}
	}
	return nil
}

func GetSessionAffinityType(service *v1.Service) string {
	return service.Annotations[ElbSessionAffinityMode]
}

func GetSessionAffinityOptions(service *v1.Service) string {
	return service.Annotations[ElbHealthCheckOptions]
}
