/*
Copyright 2016 The Archon Authors.

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

package aliyun

import (
	"fmt"
	"strings"
	"time"

	"github.com/denverdino/aliyungo/common"
	"github.com/denverdino/aliyungo/ecs"
	"github.com/denverdino/aliyungo/slb"
	"github.com/denverdino/aliyungo/util"
	log "github.com/golang/glog"
	api "k8s.io/kubernetes/pkg/api/v1"
	autil "kubeup.com/kube-aliyun/pkg/util"
)

/*
https://kubernetes.io/docs/concepts/services-networking/service/

Example:
---
apiVersion: v1
kind: Service
metadata:
	annotations:
		aliyun.archon.kubeup.com/bandwidth: "1"
		aliyun.archon.kubeup.com/load-balancer-backend-protocol: "http"
		aliyun.archon.kubeup.com/load-balancer-health-check: "true"
*/
type AliyunLoadBalancerOptions struct {
	InternetChargeType        string `k8s:"internet-charge-type"`
	Bandwidth                 int    `k8s:"bandwidth"`
	HealthyThreshold          int    `k8s:"healthy-threshold"`
	UnhealthyThreshold        int    `k8s:"unhealthy-threshold"`
	HealthCheckConnectTimeout int    `k8s:"health-check-connect-timeout"`
	HealthCheckInterval       int    `k8s:"health-check-interval"`
	BackendProtocol           string `k8s:"load-balancer-backend-protocol"`
	HealthCheck			  	  bool	 `k8s:"load-balancer-http-health-check"`
	HealthCheckURI			  string `k8s:"load-balancer-http-health-check-uri"`
	HealthCheckTimeout		  int    `k8s:"load-balancer-http-health-check-timeout"`
}

// Loadbalancer helpers
func lbToStatus(lb *slb.LoadBalancerType) *api.LoadBalancerStatus {
	return &api.LoadBalancerStatus{
		[]api.LoadBalancerIngress{
			api.LoadBalancerIngress{
				IP: lb.Address,
			},
		},
	}
}

// Loadbalancer interface
func (p *AliyunProvider) GetLoadBalancerName(service *api.Service) string {
	ret := "a" + string(service.UID)
	ret = strings.Replace(ret, "-", "", -1)
	//AWS requires that the name of a load balancer is shorter than 32 bytes.
	if len(ret) > 32 {
		ret = ret[:32]
	}
	return ret
}

func (p *AliyunProvider) getLoadBalancer(name string) (*slb.LoadBalancerType, bool, error) {
	region := common.Region(p.region)
	args := &slb.DescribeLoadBalancersArgs{
		RegionId: region,
	}
	lbs, err := p.slbClient.DescribeLoadBalancers(args)
	if err != nil {
		log.Errorf("Error describing load balancer: %+v", err)
		return nil, false, err
	}

	for _, lb := range lbs {
		if lb.LoadBalancerName == name {
			_lb, err := p.slbClient.DescribeLoadBalancerAttribute(lb.LoadBalancerId)
			if err != nil {
				return nil, false, err
			}
			return _lb, true, nil
		}
	}

	//log.Warningf("oad balancer %s: not found", name)
	return nil, false, nil
}

func (p *AliyunProvider) GetLoadBalancer(clusterName string, service *api.Service) (status *api.LoadBalancerStatus, exists bool, err error) {
	name := p.GetLoadBalancerName(service)
	lb := (*slb.LoadBalancerType)(nil)
	lb, exists, err = p.getLoadBalancer(name)
	if lb != nil {
		status = lbToStatus(lb)
	}
	return
}

func (p *AliyunProvider) EnsureLoadBalancer(clusterName string, service *api.Service, nodes []*api.Node) (*api.LoadBalancerStatus, error) {
	spec := service.Spec
	name := p.GetLoadBalancerName(service)

	// Check services
	if spec.SessionAffinity != api.ServiceAffinityNone {
		return nil, fmt.Errorf("unsupported load balancer affinity: %v", spec.SessionAffinity)
	}

	for _, port := range spec.Ports {
		switch port.Protocol {
		case api.ProtocolTCP, api.ProtocolUDP:
			continue
		default:
			return nil, fmt.Errorf("Unsupported server port protocol for Aliyun load balancers: %v", port.Protocol)
		}
	}

	if spec.LoadBalancerIP != "" {
		return nil, fmt.Errorf("LoadBalancerIP can't be set for Aliyun load balancers")
	}

	instances, err := p.getInstancesByNodes(nodes)
	if err != nil {
		return nil, err
	}

	log.Infof("Ensuring loadbalancer with backends %+v", nodes)
	// if len(instances) != len(nodes) {
	// log.Errorf("Unable to find some corresponding hosts in aliyun instances: %+v", instances)
	// }
	// TODO: separate security groups to handle sourceRanges
	/* sourceRanges, err := service.GetLoadBalancerSourceRanges(service.Annotations)
	if err != nil {
		return err
	}
	*/

	lbOptions := &AliyunLoadBalancerOptions{}
	if service.Annotations != nil {
		err = autil.MapToStruct(service.Annotations, lbOptions, AliyunAnnotationPrefix)
		if err != nil {
			log.Warningf("Unable to extract loadbalancer options from service annotations")
		}
	}

	lb, _, err := p.getLoadBalancer(name)
	if err != nil {
		log.Errorf("Error ensuring load balancer: %v", err)
		return nil, err
	}

	if lb == nil {
		// Chargetype actually doesn't conform to common.InternetChargeType. They have to be all lower case.
		args := &slb.CreateLoadBalancerArgs{
			RegionId:         common.Region(p.region),
			LoadBalancerName: name,
			// Ignore this to create a LB in the classic network
			//VSwitchId:          p.vswitch,
			AddressType:        slb.InternetAddressType,
			InternetChargeType: slb.InternetChargeType(lbOptions.InternetChargeType),
			Bandwidth:          lbOptions.Bandwidth,
			ClientToken:        util.CreateRandomString(),
		}
		_, err := p.slbClient.CreateLoadBalancer(args)
		if err != nil {
			log.Errorf("Error creating load balancer %s: %+v", name, err)
			return nil, err
		}

		retry := 3
		for retry > 0 {
			time.Sleep(time.Duration(5) * time.Second)
			lb, _, err = p.getLoadBalancer(name)
			if lb != nil {
				log.Infof("Created lb %+v with args %+v", lb, args)
				break
			}
			if err != nil {
				log.Warningf("Error checking if creating load balancer has succeeded: %v. Will retry", err)
			}

			retry -= 1
		}
		if lb == nil && retry <= 0 {
			if err == nil {
				log.Errorf("LB just doesn't exist.")
			}
			return nil, err
		}
	}

	// Sync lb
	err = p.ensureLBListeners(lb, spec.Ports, lbOptions)
	if err != nil {
		return nil, err
	}

	err = p.ensureLBBackends(lb, instances)
	if err != nil {
		return nil, err
	}

	return lbToStatus(lb), nil
}

func (p *AliyunProvider) getLBListenerAttributes(lb *slb.LoadBalancerType, pp *slb.ListenerPortAndProtocolType) (backendPort int32, status slb.ListenerStatus, err error) {
	switch strings.ToLower(pp.ListenerProtocol) {
	case "tcp":
		resp, err2 := p.slbClient.DescribeLoadBalancerTCPListenerAttribute(lb.LoadBalancerId, pp.ListenerPort)
		if err2 != nil {
			err = err2
		}
		backendPort = int32(resp.BackendServerPort)
		status = resp.Status
	case "udp":
		resp, err2 := p.slbClient.DescribeLoadBalancerUDPListenerAttribute(lb.LoadBalancerId, pp.ListenerPort)
		if err2 != nil {
			err = err2
		}
		backendPort = int32(resp.BackendServerPort)
		status = resp.Status
	default:
		err = fmt.Errorf("Error getting listener attributes. Unsupported listener protocol: %s", pp.ListenerProtocol)
	}

	return
}

func (p *AliyunProvider) ensureLBListeners(lb *slb.LoadBalancerType, ports []api.ServicePort, options *AliyunLoadBalancerOptions) error {
	keyFmt := "%d|%s"
	expected := make(map[string]api.ServicePort)
	actual := lb.ListenerPortsAndProtocol.ListenerPortAndProtocol[:]
	for _, p := range ports {
		if p.NodePort == 0 {
			log.Infof("Ignored a service port with no NodePort syncing listeners: %+v", p)
			continue
		}
		expected[fmt.Sprintf(keyFmt, p.Port, strings.ToLower(string(p.Protocol)))] = p
	}

	// Diff of port, protocol and nodeport
	removals := []int{}
	stopped := []int{}
	for _, listener := range actual {
		key := fmt.Sprintf(keyFmt, listener.ListenerPort, strings.ToLower(listener.ListenerProtocol))
		log.Infof("Existing listener: %+v", key)
		sp, ok := expected[key]
		if ok {
			// See if backendPort matches
			backendPort, status, err := p.getLBListenerAttributes(lb, &listener)
			if err != nil {
				log.Errorf("Error getting backend server port while syncing load balancer listeners: %+v", err)
				return err
			}

			if backendPort == sp.NodePort {
				if status == slb.Stopped {
					stopped = append(stopped, int(sp.Port))
				}
				delete(expected, key)
				continue
			}
		}

		removals = append(removals, listener.ListenerPort)
	}

	log.Infof("Existing: %+v, Removing %v, starting %v, creating %+v", actual, removals, stopped, expected)
	if len(stopped) > 0 {
		for _, port := range stopped {
			err := p.slbClient.StartLoadBalancerListener(lb.LoadBalancerId, port)
			if err != nil {
				log.Errorf("Error starting load balancer listener: %+v", err)
				return err
			}
		}
	}

	if len(removals) > 0 {
		for _, port := range removals {
			err := p.slbClient.DeleteLoadBalancerListener(lb.LoadBalancerId, port)
			if err != nil {
				log.Errorf("Error deleting load balancer listener: %+v", err)
				return err
			}
		}
	}

	if len(expected) > 0 {
		for _, sp := range expected {
			var err error
			switch sp.Protocol {
			case api.ProtocolTCP:
				if "HTTP" != strings.ToUpper(options.BackendProtocol) {
					args := &slb.CreateLoadBalancerTCPListenerArgs{
						LoadBalancerId:            lb.LoadBalancerId,
						ListenerPort:              int(sp.Port),
						BackendServerPort:         int(sp.NodePort),
						Bandwidth:                 options.Bandwidth,
						HealthCheckType:           slb.TCPHealthCheckType,
						HealthCheckConnectPort:    int(sp.NodePort),
						HealthyThreshold:          options.HealthyThreshold,
						UnhealthyThreshold:        options.UnhealthyThreshold,
						HealthCheckConnectTimeout: options.HealthCheckConnectTimeout,
						HealthCheckInterval:       options.HealthCheckInterval,
					}
					err = p.slbClient.CreateLoadBalancerTCPListener(args)
				} else {
					// https://help.aliyun.com/document_detail/27592.html?spm=5176.doc27637.6.646.7Me9In
					args := &slb.CreateLoadBalancerHTTPListenerArgs{
						LoadBalancerId:    lb.LoadBalancerId,
						ListenerPort:      int(sp.Port),
						BackendServerPort: int(sp.NodePort),
						Bandwidth:         options.Bandwidth,
						StickySession:	   slb.OffFlag,
					}

					// Check value of "aliyun.archon.kubeup.com/load-balancer-http-health-check"
					if true == options.HealthCheck {
						args.HealthCheck = slb.OnFlag
						args.HealthCheckConnectPort = int(sp.NodePort)
						args.HealthCheckHttpCode = "http_2xx,http_3xx,http_4xx"

						// Check existence of mandatory values and setup default values
						if 0 == len(options.HealthCheckURI)       { options.HealthCheckURI = "/"    }
						if 0 == options.HealthCheckConnectTimeout { options.HealthCheckTimeout = 3  }
						if 0 == options.HealthCheckInterval       { options.HealthCheckInterval = 5 }
						if 0 == options.HealthyThreshold          { options.HealthyThreshold = 4    }
						if 0 == options.UnhealthyThreshold        { options.UnhealthyThreshold = 4  }

						args.HealthCheckURI = options.HealthCheckURI
						args.HealthCheckTimeout = options.HealthCheckTimeout
						args.HealthCheckInterval = options.HealthCheckInterval
						args.HealthyThreshold = options.HealthyThreshold
						args.UnhealthyThreshold = options.UnhealthyThreshold
					} else {
						args.HealthCheck = slb.OffFlag
					}
					err = p.slbClient.CreateLoadBalancerHTTPListener(args)
				}
			case api.ProtocolUDP:
				args := &slb.CreateLoadBalancerUDPListenerArgs{
					LoadBalancerId:            lb.LoadBalancerId,
					ListenerPort:              int(sp.Port),
					BackendServerPort:         int(sp.NodePort),
					Bandwidth:                 options.Bandwidth,
					HealthCheckConnectPort:    int(sp.NodePort),
					HealthyThreshold:          options.HealthyThreshold,
					UnhealthyThreshold:        options.UnhealthyThreshold,
					HealthCheckConnectTimeout: options.HealthCheckConnectTimeout,
					HealthCheckInterval:       options.HealthCheckInterval,
				}
				err = p.slbClient.CreateLoadBalancerUDPListener(args)

			default:
				err = fmt.Errorf("Error creating service listener. Unsupported listener protocol: %s", string(sp.Protocol))
			}

			if err != nil {
				log.Errorf("Error creating load balancer listener for service port %+v: %+v", sp, err)
				return err
			}

			err = p.slbClient.StartLoadBalancerListener(lb.LoadBalancerId, int(sp.Port))
			if err != nil {
				log.Errorf("Error starting load balancer listener for service port %+v: %+v", sp, err)
				return err
			}
		}
	}

	return nil
}

func (p *AliyunProvider) ensureLBBackends(lb *slb.LoadBalancerType, instances []ecs.InstanceAttributesType) error {
	actual := lb.BackendServers.BackendServer[:]
	expected := make(map[string]bool)
	for _, ins := range instances {
		expected[ins.InstanceId] = true
	}

	removals := []string{}
	for _, s := range actual {
		if _, ok := expected[s.ServerId]; ok {
			delete(expected, s.ServerId)
			continue
		}

		removals = append(removals, s.ServerId)
	}

	additions := []slb.BackendServerType{}
	for ins := range expected {
		additions = append(additions, slb.BackendServerType{
			ServerId: ins,
			Weight:   100,
		})
	}

	if len(removals) > 0 {
		_, err := p.slbClient.RemoveBackendServers(lb.LoadBalancerId, removals)
		if err != nil {
			log.Errorf("Error removing backend servers from load balancer %s: %+v", lb.LoadBalancerId, err)
			return err
		}
	}

	if len(additions) > 0 {
		_, err := p.slbClient.AddBackendServers(lb.LoadBalancerId, additions)
		if err != nil {
			log.Errorf("Error adding backend servers from load balancer %s: %+v", lb.LoadBalancerId, err)
			return err
		}
	}

	return nil
}

func (p *AliyunProvider) UpdateLoadBalancer(clusterName string, service *api.Service, nodes []*api.Node) error {
	lb, _, err := p.getLoadBalancer(p.GetLoadBalancerName(service))
	if err != nil {
		return err
	}

	if lb == nil {
		return fmt.Errorf("Load balancer is not found")
	}

	lbOptions := &AliyunLoadBalancerOptions{}
	if service.Annotations != nil {
		err = autil.MapToStruct(service.Annotations, lbOptions, AliyunAnnotationPrefix)
		if err != nil {
			log.Warningf("Unable to extract loadbalancer options from service annotations")
		}
	}

	// Sync lb
	log.Infof("Updating LB: %+v", lb)
	err = p.ensureLBListeners(lb, service.Spec.Ports, lbOptions)
	if err != nil {
		return err
	}

	instances, err := p.getInstancesByNodes(nodes)
	if err != nil {
		return err
	}

	err = p.ensureLBBackends(lb, instances)
	if err != nil {
		return err
	}

	return nil
}

func (p *AliyunProvider) EnsureLoadBalancerDeleted(clusterName string, service *api.Service) error {
	name := p.GetLoadBalancerName(service)
	log.Infof("Deleting service lb: %s", name)
	lb, _, err := p.getLoadBalancer(name)
	if err != nil {
		return err
	}
	if lb == nil {
		log.Infof("Probably already gone. Ignoring")
		return nil
	}
	err = p.slbClient.DeleteLoadBalancer(lb.LoadBalancerId)
	return err
}
