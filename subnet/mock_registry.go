// Copyright 2015 flannel authors
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

package subnet

import (
	"fmt"
	"strings"
	"time"

	etcd "github.com/coreos/flannel/Godeps/_workspace/src/github.com/coreos/etcd/client"
	"github.com/coreos/flannel/Godeps/_workspace/src/golang.org/x/net/context"

	"github.com/coreos/flannel/pkg/ip"
)

type netwk struct {
	config       string
	subnets      []Lease
	subnetEvents chan event
}

type event struct {
	evt   Event
	index uint64
}

type MockSubnetRegistry struct {
	networks      map[string]*netwk
	networkEvents chan event
	index         uint64
}

func NewMockRegistry(network, config string, initialSubnets []Lease) *MockSubnetRegistry {
	msr := &MockSubnetRegistry{
		networkEvents: make(chan event, 1000),
		index:         1000,
		networks:      make(map[string]*netwk),
	}

	msr.networks[network] = &netwk{
		config:       config,
		subnets:      initialSubnets,
		subnetEvents: make(chan event, 1000),
	}
	return msr
}

func (msr *MockSubnetRegistry) getNetworkConfig(ctx context.Context, network string) (string, error) {
	n, ok := msr.networks[network]
	if !ok {
		return "", fmt.Errorf("Network %s not found", network)
	}
	return n.config, nil
}

func (msr *MockSubnetRegistry) setConfig(network, config string) error {
	n, ok := msr.networks[network]
	if !ok {
		return fmt.Errorf("Network %s not found", network)
	}
	n.config = config
	return nil
}

func (msr *MockSubnetRegistry) getSubnets(ctx context.Context, network string) ([]Lease, uint64, error) {
	n, ok := msr.networks[network]
	if !ok {
		return nil, 0, fmt.Errorf("Network %s not found", network)
	}
	return n.subnets, msr.index, nil
}

func (msr *MockSubnetRegistry) createSubnet(ctx context.Context, network string, sn ip.IP4Net, attrs *LeaseAttrs, ttl time.Duration) (time.Time, error) {
	n, ok := msr.networks[network]
	if !ok {
		return time.Time{}, fmt.Errorf("Network %s not found", network)
	}

	// check for existing
	if _, _, err := n.findSubnet(sn); err == nil {
		return time.Time{}, etcd.Error{
			Code:  etcd.ErrorCodeNodeExist,
			Index: msr.index,
		}
	}

	msr.index += 1

	exp := clock.Now().Add(ttl)

	l := Lease{
		Subnet:     sn,
		Attrs:      *attrs,
		Expiration: exp,
		asof:       msr.index,
	}
	n.subnets = append(n.subnets, l)

	evt := Event{
		Type:    EventAdded,
		Lease:   l,
		Network: network,
	}
	n.subnetEvents <- event{evt, msr.index}

	return exp, nil
}

func (msr *MockSubnetRegistry) updateSubnet(ctx context.Context, network string, sn ip.IP4Net, attrs *LeaseAttrs, ttl time.Duration, asof uint64) (time.Time, error) {
	n, ok := msr.networks[network]
	if !ok {
		return time.Time{}, fmt.Errorf("Network %s not found", network)
	}

	msr.index += 1

	exp := clock.Now().Add(ttl)

	sub, _, err := n.findSubnet(sn)
	if err != nil {
		return time.Time{}, err
	}

	sub.Attrs = *attrs
	sub.Expiration = exp
	sub.asof = msr.index
	n.subnetEvents <- event{
		Event{
			Type:    EventAdded,
			Lease:   sub,
			Network: network,
		}, msr.index}

	return sub.Expiration, nil
}

func (msr *MockSubnetRegistry) deleteSubnet(ctx context.Context, network string, sn ip.IP4Net) error {
	n, ok := msr.networks[network]
	if !ok {
		return fmt.Errorf("Network %s not found", network)
	}

	msr.index += 1

	sub, i, err := n.findSubnet(sn)
	if err != nil {
		return err
	}

	n.subnets[i] = n.subnets[len(n.subnets)-1]
	n.subnets = n.subnets[:len(n.subnets)-1]
	sub.asof = msr.index
	n.subnetEvents <- event{
		Event{
			Type:    EventRemoved,
			Lease:   sub,
			Network: network,
		}, msr.index}

	return nil
}

func (msr *MockSubnetRegistry) watchSubnets(ctx context.Context, network string, since uint64) (Event, uint64, error) {
	n, ok := msr.networks[network]
	if !ok {
		return Event{}, msr.index, fmt.Errorf("Network %s not found", network)
	}

	for {
		if since < msr.index {
			return Event{}, msr.index, etcd.Error{
				Code:    etcd.ErrorCodeEventIndexCleared,
				Cause:   "out of date",
				Message: "cursor is out of date",
				Index:   msr.index,
			}
		}

		select {
		case <-ctx.Done():
			return Event{}, msr.index, ctx.Err()

		case e := <-n.subnetEvents:
			if e.index <= since {
				continue
			}
			return e.evt, msr.index, nil
		}
	}
}

func (msr *MockSubnetRegistry) expireSubnet(network string, sn ip.IP4Net) {
	n, ok := msr.networks[network]
	if !ok {
		return
	}

	if sub, i, err := n.findSubnet(sn); err == nil {
		msr.index += 1
		n.subnets[i] = n.subnets[len(n.subnets)-1]
		n.subnets = n.subnets[:len(n.subnets)-1]
		sub.asof = msr.index
		n.subnetEvents <- event{
			Event{
				Type:  EventRemoved,
				Lease: sub,
			}, msr.index,
		}
	}
}

func configKeyToNetworkKey(configKey string) string {
	if !strings.HasSuffix(configKey, "/config") {
		return ""
	}
	return strings.TrimSuffix(configKey, "/config")
}

func (msr *MockSubnetRegistry) getNetworks(ctx context.Context) ([]string, uint64, error) {
	ns := []string{}

	for n, _ := range msr.networks {
		ns = append(ns, n)
	}

	return ns, msr.index, nil
}

func (msr *MockSubnetRegistry) watchNetworks(ctx context.Context, since uint64) (Event, uint64, error) {
	for {
		if since < msr.index {
			return Event{}, msr.index, etcd.Error{
				Code:    etcd.ErrorCodeEventIndexCleared,
				Cause:   "out of date",
				Message: "cursor is out of date",
				Index:   msr.index,
			}
		}

		select {
		case <-ctx.Done():
			return Event{}, msr.index, ctx.Err()

		case e := <-msr.networkEvents:
			if e.index <= since {
				continue
			}
			return e.evt, msr.index, nil
		}
	}
}

func (msr *MockSubnetRegistry) getNetwork(ctx context.Context, network string) (*netwk, error) {
	n, ok := msr.networks[network]
	if !ok {
		return nil, fmt.Errorf("Network %s not found", network)
	}

	return n, nil
}

func (msr *MockSubnetRegistry) CreateNetwork(ctx context.Context, network, config string) error {
	_, ok := msr.networks[network]
	if ok {
		return fmt.Errorf("Network %s already exists", network)
	}

	msr.index += 1

	n := &netwk{
		config:       network,
		subnetEvents: make(chan event, 1000),
	}

	msr.networks[network] = n
	msr.networkEvents <- event{
		Event{
			Type:    EventAdded,
			Network: network,
		}, msr.index,
	}

	return nil
}

func (msr *MockSubnetRegistry) DeleteNetwork(ctx context.Context, network string) error {
	_, ok := msr.networks[network]
	if !ok {
		return fmt.Errorf("Network %s not found", network)
	}
	delete(msr.networks, network)

	msr.index += 1

	msr.networkEvents <- event{
		Event{
			Type:    EventRemoved,
			Network: network,
		}, msr.index,
	}

	return nil
}

func (n *netwk) findSubnet(sn ip.IP4Net) (Lease, int, error) {
	for i, sub := range n.subnets {
		if sub.Subnet.Equal(sn) {
			return sub, i, nil
		}
	}
	return Lease{}, 0, fmt.Errorf("subnet not found")
}
