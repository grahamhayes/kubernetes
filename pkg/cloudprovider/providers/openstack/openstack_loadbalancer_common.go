/*
Copyright 2016 The Kubernetes Authors.

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

package openstack

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	"github.com/rackspace/gophercloud"
	"github.com/rackspace/gophercloud/openstack/networking/v2/extensions"
	"github.com/rackspace/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/rackspace/gophercloud/openstack/networking/v2/extensions/lbaas/members"
	"github.com/rackspace/gophercloud/openstack/networking/v2/extensions/lbaas/monitors"
	"github.com/rackspace/gophercloud/openstack/networking/v2/extensions/lbaas/pools"
	"github.com/rackspace/gophercloud/openstack/networking/v2/extensions/lbaas/vips"
	"github.com/rackspace/gophercloud/openstack/networking/v2/extensions/lbaas_v2/listeners"
	"github.com/rackspace/gophercloud/openstack/networking/v2/extensions/lbaas_v2/loadbalancers"
	v2_monitors "github.com/rackspace/gophercloud/openstack/networking/v2/extensions/lbaas_v2/monitors"
	v2_pools "github.com/rackspace/gophercloud/openstack/networking/v2/extensions/lbaas_v2/pools"
	neutron_ports "github.com/rackspace/gophercloud/openstack/networking/v2/ports"
	"github.com/rackspace/gophercloud/pagination"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/api/v1/service"
	"k8s.io/kubernetes/pkg/cloudprovider"
)

// Note: when creating a new Loadbalancer (VM), it can take some time before it is ready for use,
// this timeout is used for waiting until the Loadbalancer provisioning status goes to ACTIVE state.
const loadbalancerActiveTimeoutSeconds = 120
const loadbalancerDeleteTimeoutSeconds = 30

// LoadBalancer implementation for LBaaS v1
type LbaasV1 struct {
	LoadBalancer
}

type empty struct{}

func networkExtensions(client *gophercloud.ServiceClient) (map[string]bool, error) {
	seen := make(map[string]bool)

	pager := extensions.List(client)
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		exts, err := extensions.ExtractExtensions(page)
		if err != nil {
			return false, err
		}
		for _, ext := range exts {
			seen[ext.Alias] = true
		}
		return true, nil
	})

	return seen, err
}

func getPortByIP(client *gophercloud.ServiceClient, ipAddress string) (neutron_ports.Port, error) {
	var targetPort neutron_ports.Port
	var portFound = false

	err := neutron_ports.List(client, neutron_ports.ListOpts{}).EachPage(func(page pagination.Page) (bool, error) {
		portList, err := neutron_ports.ExtractPorts(page)
		if err != nil {
			return false, err
		}

		for _, port := range portList {
			for _, ip := range port.FixedIPs {
				if ip.IPAddress == ipAddress {
					targetPort = port
					portFound = true
					return false, nil
				}
			}
		}

		return true, nil
	})
	if err == nil && !portFound {
		err = ErrNotFound
	}
	return targetPort, err
}

func getPortIDByIP(client *gophercloud.ServiceClient, ipAddress string) (string, error) {
	targetPort, err := getPortByIP(client, ipAddress)
	if err != nil {
		return targetPort.ID, err
	}
	return targetPort.ID, nil
}

func getFloatingIPByPortID(client *gophercloud.ServiceClient, portID string) (*floatingips.FloatingIP, error) {
	opts := floatingips.ListOpts{
		PortID: portID,
	}
	pager := floatingips.List(client, opts)

	floatingIPList := make([]floatingips.FloatingIP, 0, 1)

	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		f, err := floatingips.ExtractFloatingIPs(page)
		if err != nil {
			return false, err
		}
		floatingIPList = append(floatingIPList, f...)
		if len(floatingIPList) > 1 {
			return false, ErrMultipleResults
		}
		return true, nil
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if len(floatingIPList) == 0 {
		return nil, ErrNotFound
	} else if len(floatingIPList) > 1 {
		return nil, ErrMultipleResults
	}

	return &floatingIPList[0], nil
}

func getPoolByName(client *gophercloud.ServiceClient, name string) (*pools.Pool, error) {
	opts := pools.ListOpts{
		Name: name,
	}
	pager := pools.List(client, opts)

	poolList := make([]pools.Pool, 0, 1)

	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		p, err := pools.ExtractPools(page)
		if err != nil {
			return false, err
		}
		poolList = append(poolList, p...)
		if len(poolList) > 1 {
			return false, ErrMultipleResults
		}
		return true, nil
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if len(poolList) == 0 {
		return nil, ErrNotFound
	} else if len(poolList) > 1 {
		return nil, ErrMultipleResults
	}

	return &poolList[0], nil
}

func getVipByName(client *gophercloud.ServiceClient, name string) (*vips.VirtualIP, error) {
	opts := vips.ListOpts{
		Name: name,
	}
	pager := vips.List(client, opts)

	vipList := make([]vips.VirtualIP, 0, 1)

	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		v, err := vips.ExtractVIPs(page)
		if err != nil {
			return false, err
		}
		vipList = append(vipList, v...)
		if len(vipList) > 1 {
			return false, ErrMultipleResults
		}
		return true, nil
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if len(vipList) == 0 {
		return nil, ErrNotFound
	} else if len(vipList) > 1 {
		return nil, ErrMultipleResults
	}

	return &vipList[0], nil
}

func getLoadbalancerByName(client *gophercloud.ServiceClient, name string) (*loadbalancers.LoadBalancer, error) {
	opts := loadbalancers.ListOpts{
		Name: name,
	}
	pager := loadbalancers.List(client, opts)

	loadbalancerList := make([]loadbalancers.LoadBalancer, 0, 1)

	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		v, err := loadbalancers.ExtractLoadbalancers(page)
		if err != nil {
			return false, err
		}
		loadbalancerList = append(loadbalancerList, v...)
		if len(loadbalancerList) > 1 {
			return false, ErrMultipleResults
		}
		return true, nil
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if len(loadbalancerList) == 0 {
		return nil, ErrNotFound
	} else if len(loadbalancerList) > 1 {
		return nil, ErrMultipleResults
	}

	return &loadbalancerList[0], nil
}

func getListenersByLoadBalancerID(client *gophercloud.ServiceClient, id string) ([]listeners.Listener, error) {
	var existingListeners []listeners.Listener
	err := listeners.List(client, listeners.ListOpts{LoadbalancerID: id}).EachPage(func(page pagination.Page) (bool, error) {
		listenerList, err := listeners.ExtractListeners(page)
		if err != nil {
			return false, err
		}
		for _, l := range listenerList {
			for _, lb := range l.Loadbalancers {
				if lb.ID == id {
					existingListeners = append(existingListeners, l)
					break
				}
			}
		}

		return true, nil
	})
	if err != nil {
		return nil, err
	}

	return existingListeners, nil
}

// get listener for a port or nil if does not exist
func getListenerForPort(existingListeners []listeners.Listener, port v1.ServicePort) *listeners.Listener {
	for _, l := range existingListeners {
		if l.Protocol == string(port.Protocol) && l.ProtocolPort == int(port.Port) {
			return &l
		}
	}

	return nil
}

// Get pool for a listener. A listener always has exactly one pool.
func getPoolByListenerID(client *gophercloud.ServiceClient, loadbalancerID string, listenerID string) (*v2_pools.Pool, error) {
	listenerPools := make([]v2_pools.Pool, 0, 1)
	err := v2_pools.List(client, v2_pools.ListOpts{LoadbalancerID: loadbalancerID}).EachPage(func(page pagination.Page) (bool, error) {
		poolsList, err := v2_pools.ExtractPools(page)
		if err != nil {
			return false, err
		}
		for _, p := range poolsList {
			for _, l := range p.Listeners {
				if l.ID == listenerID {
					listenerPools = append(listenerPools, p)
				}
			}
		}
		if len(listenerPools) > 1 {
			return false, ErrMultipleResults
		}
		return true, nil
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if len(listenerPools) == 0 {
		return nil, ErrNotFound
	} else if len(listenerPools) > 1 {
		return nil, ErrMultipleResults
	}

	return &listenerPools[0], nil
}

func getMembersByPoolID(client *gophercloud.ServiceClient, id string) ([]v2_pools.Member, error) {
	var members []v2_pools.Member
	err := v2_pools.ListAssociateMembers(client, id, v2_pools.MemberListOpts{}).EachPage(func(page pagination.Page) (bool, error) {
		membersList, err := v2_pools.ExtractMembers(page)
		if err != nil {
			return false, err
		}
		members = append(members, membersList...)

		return true, nil
	})
	if err != nil {
		return nil, err
	}

	return members, nil
}

// Each pool has exactly one or zero monitors. ListOpts does not seem to filter anything.
func getMonitorByPoolID(client *gophercloud.ServiceClient, id string) (*v2_monitors.Monitor, error) {
	var monitorList []v2_monitors.Monitor
	err := v2_monitors.List(client, v2_monitors.ListOpts{PoolID: id}).EachPage(func(page pagination.Page) (bool, error) {
		monitorsList, err := v2_monitors.ExtractMonitors(page)
		if err != nil {
			return false, err
		}

		for _, monitor := range monitorsList {
			// bugfix, filter by poolid
			for _, pool := range monitor.Pools {
				if pool.ID == id {
					monitorList = append(monitorList, monitor)
				}
			}
		}
		if len(monitorList) > 1 {
			return false, ErrMultipleResults
		}
		return true, nil
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if len(monitorList) == 0 {
		return nil, ErrNotFound
	} else if len(monitorList) > 1 {
		return nil, ErrMultipleResults
	}

	return &monitorList[0], nil
}

// Check if a member exists for node
func memberExists(members []v2_pools.Member, addr string, port int) bool {
	for _, member := range members {
		if member.Address == addr && member.ProtocolPort == port {
			return true
		}
	}

	return false
}

func popListener(existingListeners []listeners.Listener, id string) []listeners.Listener {
	for i, existingListener := range existingListeners {
		if existingListener.ID == id {
			existingListeners[i] = existingListeners[len(existingListeners)-1]
			existingListeners = existingListeners[:len(existingListeners)-1]
			break
		}
	}

	return existingListeners
}

func popMember(members []v2_pools.Member, addr string, port int) []v2_pools.Member {
	for i, member := range members {
		if member.Address == addr && member.ProtocolPort == port {
			members[i] = members[len(members)-1]
			members = members[:len(members)-1]
		}
	}

	return members
}

func waitLoadbalancerActiveProvisioningStatus(client *gophercloud.ServiceClient, loadbalancerID string) (string, error) {
	start := time.Now().Second()
	for {
		loadbalancer, err := loadbalancers.Get(client, loadbalancerID).Extract()
		if err != nil {
			return "", err
		}
		if loadbalancer.ProvisioningStatus == "ACTIVE" {
			return "ACTIVE", nil
		} else if loadbalancer.ProvisioningStatus == "ERROR" {
			return "ERROR", fmt.Errorf("Loadbalancer has gone into ERROR state")
		}

		time.Sleep(1 * time.Second)

		if time.Now().Second()-start >= loadbalancerActiveTimeoutSeconds {
			return loadbalancer.ProvisioningStatus, fmt.Errorf("Loadbalancer failed to go into ACTIVE provisioning status within alloted time")
		}
	}
}

func waitLoadbalancerDeleted(client *gophercloud.ServiceClient, loadbalancerID string) error {
	start := time.Now().Second()
	for {
		_, err := loadbalancers.Get(client, loadbalancerID).Extract()
		if err != nil {
			if err == ErrNotFound {
				return nil
			} else {
				return err
			}
		}

		time.Sleep(1 * time.Second)

		if time.Now().Second()-start >= loadbalancerDeleteTimeoutSeconds {
			return fmt.Errorf("Loadbalancer failed to delete within the alloted time")
		}

	}
}
