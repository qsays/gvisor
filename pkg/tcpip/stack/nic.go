// Copyright 2018 The gVisor Authors.
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

package stack

import (
	"log"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// NIC represents a "network interface card" to which the networking stack is
// attached.
type NIC struct {
	stack   *Stack
	id      tcpip.NICID
	name    string
	linkEP  LinkEndpoint
	context NICContext

	stats NICStats

	mu struct {
		sync.RWMutex
		enabled       bool
		spoofing      bool
		promiscuous   bool
		primary       map[tcpip.NetworkProtocolNumber][]*referencedNetworkEndpoint
		endpoints     map[NetworkEndpointID]*referencedNetworkEndpoint
		addressRanges []tcpip.Subnet
		mcastJoins    map[NetworkEndpointID]int32
		// packetEPs is protected by mu, but the contained PacketEndpoint
		// values are not.
		packetEPs map[tcpip.NetworkProtocolNumber][]PacketEndpoint
		ndp       ndpState
	}
}

// NICStats includes transmitted and received stats.
type NICStats struct {
	Tx DirectionStats
	Rx DirectionStats

	DisabledRx DirectionStats
}

func makeNICStats() NICStats {
	var s NICStats
	tcpip.InitStatCounters(reflect.ValueOf(&s).Elem())
	return s
}

// DirectionStats includes packet and byte counts.
type DirectionStats struct {
	Packets *tcpip.StatCounter
	Bytes   *tcpip.StatCounter
}

// PrimaryEndpointBehavior is an enumeration of an endpoint's primacy behavior.
type PrimaryEndpointBehavior int

const (
	// CanBePrimaryEndpoint indicates the endpoint can be used as a primary
	// endpoint for new connections with no local address. This is the
	// default when calling NIC.AddAddress.
	CanBePrimaryEndpoint PrimaryEndpointBehavior = iota

	// FirstPrimaryEndpoint indicates the endpoint should be the first
	// primary endpoint considered. If there are multiple endpoints with
	// this behavior, the most recently-added one will be first.
	FirstPrimaryEndpoint

	// NeverPrimaryEndpoint indicates the endpoint should never be a
	// primary endpoint.
	NeverPrimaryEndpoint
)

// newNIC returns a new NIC using the default NDP configurations from stack.
func newNIC(stack *Stack, id tcpip.NICID, name string, ep LinkEndpoint, ctx NICContext) *NIC {
	// TODO(b/141011931): Validate a LinkEndpoint (ep) is valid. For
	// example, make sure that the link address it provides is a valid
	// unicast ethernet address.

	// TODO(b/143357959): RFC 8200 section 5 requires that IPv6 endpoints
	// observe an MTU of at least 1280 bytes. Ensure that this requirement
	// of IPv6 is supported on this endpoint's LinkEndpoint.

	nic := &NIC{
		stack:   stack,
		id:      id,
		name:    name,
		linkEP:  ep,
		context: ctx,
		stats:   makeNICStats(),
	}
	nic.mu.primary = make(map[tcpip.NetworkProtocolNumber][]*referencedNetworkEndpoint)
	nic.mu.endpoints = make(map[NetworkEndpointID]*referencedNetworkEndpoint)
	nic.mu.mcastJoins = make(map[NetworkEndpointID]int32)
	nic.mu.packetEPs = make(map[tcpip.NetworkProtocolNumber][]PacketEndpoint)
	nic.mu.ndp = ndpState{
		nic:              nic,
		configs:          stack.ndpConfigs,
		dad:              make(map[tcpip.Address]dadState),
		defaultRouters:   make(map[tcpip.Address]defaultRouterState),
		onLinkPrefixes:   make(map[tcpip.Subnet]onLinkPrefixState),
		autoGenAddresses: make(map[tcpip.Address]autoGenAddressState),
	}

	// Register supported packet endpoint protocols.
	for _, netProto := range header.Ethertypes {
		nic.mu.packetEPs[netProto] = []PacketEndpoint{}
	}
	for _, netProto := range stack.networkProtocols {
		nic.mu.packetEPs[netProto.Number()] = []PacketEndpoint{}
	}

	return nic
}

// enable enables the NIC. enable will attach the link to its LinkEndpoint and
// join the IPv6 All-Nodes Multicast address (ff02::1).
func (n *NIC) enable() *tcpip.Error {
	n.mu.RLock()
	enabled := n.mu.enabled
	n.mu.RUnlock()
	if enabled {
		return nil
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if n.mu.enabled {
		return nil
	}

	n.mu.enabled = true

	n.attachLinkEndpoint()

	// Create an endpoint to receive broadcast packets on this interface.
	if _, ok := n.stack.networkProtocols[header.IPv4ProtocolNumber]; ok {
		if _, err := n.addAddressLocked(tcpip.ProtocolAddress{
			Protocol:          header.IPv4ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{header.IPv4Broadcast, 8 * header.IPv4AddressSize},
		}, NeverPrimaryEndpoint, permanent, static, false /* deprecated */); err != nil {
			return err
		}
	}

	// Join the IPv6 All-Nodes Multicast group if the stack is configured to
	// use IPv6. This is required to ensure that this node properly receives
	// and responds to the various NDP messages that are destined to the
	// all-nodes multicast address. An example is the Neighbor Advertisement
	// when we perform Duplicate Address Detection, or Router Advertisement
	// when we do Router Discovery. See RFC 4862, section 5.4.2 and RFC 4861
	// section 4.2 for more information.
	//
	// Also auto-generate an IPv6 link-local address based on the NIC's
	// link address if it is configured to do so. Note, each interface is
	// required to have IPv6 link-local unicast address, as per RFC 4291
	// section 2.1.
	_, ok := n.stack.networkProtocols[header.IPv6ProtocolNumber]
	if !ok {
		return nil
	}

	// Perform DAD on the all the unicast IPv6 endpoints that are in the permanent
	// state.
	//
	// Addresses may have aleady completed DAD but in the time since the NIC was
	// last enabled, other devices may have acquired the same addresses.
	for _, r := range n.mu.endpoints {
		addr := r.ep.ID().LocalAddress
		if k := r.getKind(); (k != permanent && k != permanentTentative) || !header.IsV6UnicastAddress(addr) {
			continue
		}

		r.setKind(permanentTentative)
		if err := n.mu.ndp.startDuplicateAddressDetection(addr, r); err != nil {
			return err
		}
	}

	if err := n.joinGroupLocked(header.IPv6ProtocolNumber, header.IPv6AllNodesMulticastAddress); err != nil {
		return err
	}

	// Do not auto-generate an IPv6 link-local address for loopback devices.
	if n.stack.autoGenIPv6LinkLocal && !n.isLoopback() {
		// The valid and preferred lifetime is infinite for the auto-generated
		// link-local address.
		n.mu.ndp.doSLAAC(header.IPv6LinkLocalPrefix.Subnet(), header.NDPInfiniteLifetime, header.NDPInfiniteLifetime)
	}

	// If we are operating as a router, then do not solicit routers since we
	// won't process the RAs anyways.
	//
	// Routers do not process Router Advertisements (RA) the same way a host
	// does. That is, routers do not learn from RAs (e.g. on-link prefixes
	// and default routers). Therefore, soliciting RAs from other routers on
	// a link is unnecessary for routers.
	if !n.stack.forwarding {
		n.mu.ndp.startSolicitingRouters()
	}

	return nil
}

// becomeIPv6Router transitions n into an IPv6 router.
//
// When transitioning into an IPv6 router, host-only state (NDP discovered
// routers, discovered on-link prefixes, and auto-generated addresses) will
// be cleaned up/invalidated and NDP router solicitations will be stopped.
func (n *NIC) becomeIPv6Router() {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.mu.ndp.cleanupHostOnlyState()
	n.mu.ndp.stopSolicitingRouters()
}

// becomeIPv6Host transitions n into an IPv6 host.
//
// When transitioning into an IPv6 host, NDP router solicitations will be
// started.
func (n *NIC) becomeIPv6Host() {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.mu.ndp.startSolicitingRouters()
}

// attachLinkEndpoint attaches the NIC to the endpoint, which will enable it
// to start delivering packets.
func (n *NIC) attachLinkEndpoint() {
	n.linkEP.Attach(n)
}

// setPromiscuousMode enables or disables promiscuous mode.
func (n *NIC) setPromiscuousMode(enable bool) {
	n.mu.Lock()
	n.mu.promiscuous = enable
	n.mu.Unlock()
}

func (n *NIC) isPromiscuousMode() bool {
	n.mu.RLock()
	rv := n.mu.promiscuous
	n.mu.RUnlock()
	return rv
}

func (n *NIC) isLoopback() bool {
	return n.linkEP.Capabilities()&CapabilityLoopback != 0
}

// setSpoofing enables or disables address spoofing.
func (n *NIC) setSpoofing(enable bool) {
	n.mu.Lock()
	n.mu.spoofing = enable
	n.mu.Unlock()
}

// primaryEndpoint will return the first non-deprecated endpoint if such an
// endpoint exists for the given protocol and remoteAddr. If no non-deprecated
// endpoint exists, the first deprecated endpoint will be returned.
//
// If an IPv6 primary endpoint is requested, Source Address Selection (as
// defined by RFC 6724 section 5) will be performed.
func (n *NIC) primaryEndpoint(protocol tcpip.NetworkProtocolNumber, remoteAddr tcpip.Address) *referencedNetworkEndpoint {
	if protocol == header.IPv6ProtocolNumber && remoteAddr != "" {
		return n.primaryIPv6Endpoint(remoteAddr)
	}

	n.mu.RLock()
	defer n.mu.RUnlock()

	var deprecatedEndpoint *referencedNetworkEndpoint
	for _, r := range n.mu.primary[protocol] {
		if !r.isValidForOutgoingRLocked() {
			continue
		}

		if !r.deprecated {
			if r.tryIncRef() {
				// r is not deprecated, so return it immediately.
				//
				// If we kept track of a deprecated endpoint, decrement its reference
				// count since it was incremented when we decided to keep track of it.
				if deprecatedEndpoint != nil {
					deprecatedEndpoint.decRefLocked()
					deprecatedEndpoint = nil
				}

				return r
			}
		} else if deprecatedEndpoint == nil && r.tryIncRef() {
			// We prefer an endpoint that is not deprecated, but we keep track of r in
			// case n doesn't have any non-deprecated endpoints.
			//
			// If we end up finding a more preferred endpoint, r's reference count
			// will be decremented when such an endpoint is found.
			deprecatedEndpoint = r
		}
	}

	// n doesn't have any valid non-deprecated endpoints, so return
	// deprecatedEndpoint (which may be nil if n doesn't have any valid deprecated
	// endpoints either).
	return deprecatedEndpoint
}

// ipv6AddrCandidate is an IPv6 candidate for Source Address Selection (RFC
// 6724 section 5).
type ipv6AddrCandidate struct {
	ref   *referencedNetworkEndpoint
	scope header.IPv6AddressScope
}

// primaryIPv6Endpoint returns an IPv6 endpoint following Source Address
// Selection (RFC 6724 section 5).
//
// Note, only rules 1-3 are followed.
//
// remoteAddr must be a valid IPv6 address.
func (n *NIC) primaryIPv6Endpoint(remoteAddr tcpip.Address) *referencedNetworkEndpoint {
	n.mu.RLock()
	defer n.mu.RUnlock()

	primaryAddrs := n.mu.primary[header.IPv6ProtocolNumber]

	if len(primaryAddrs) == 0 {
		return nil
	}

	// Create a candidate set of available addresses we can potentially use as a
	// source address.
	cs := make([]ipv6AddrCandidate, 0, len(primaryAddrs))
	for _, r := range primaryAddrs {
		// If r is not valid for outgoing connections, it is not a valid endpoint.
		if !r.isValidForOutgoing() {
			continue
		}

		addr := r.ep.ID().LocalAddress
		scope, err := header.ScopeForIPv6Address(addr)
		if err != nil {
			// Should never happen as we got r from the primary IPv6 endpoint list and
			// ScopeForIPv6Address only returns an error if addr is not an IPv6
			// address.
			log.Fatalf("header.ScopeForIPv6Address(%s): %s", addr, err)
		}

		cs = append(cs, ipv6AddrCandidate{
			ref:   r,
			scope: scope,
		})
	}

	remoteScope, err := header.ScopeForIPv6Address(remoteAddr)
	if err != nil {
		// primaryIPv6Endpoint should never be called with an invalid IPv6 address.
		log.Fatalf("header.ScopeForIPv6Address(%s): %s", remoteAddr, err)
	}

	// Sort the addresses as per RFC 6724 section 5 rules 1-3.
	//
	// TODO(b/146021396): Implement rules 4-8 of RFC 6724 section 5.
	sort.Slice(cs, func(i, j int) bool {
		sa := cs[i]
		sb := cs[j]

		// Prefer same address as per RFC 6724 section 5 rule 1.
		if sa.ref.ep.ID().LocalAddress == remoteAddr {
			return true
		}
		if sb.ref.ep.ID().LocalAddress == remoteAddr {
			return false
		}

		// Prefer appropriate scope as per RFC 6724 section 5 rule 2.
		if sa.scope < sb.scope {
			return sa.scope >= remoteScope
		} else if sb.scope < sa.scope {
			return sb.scope < remoteScope
		}

		// Avoid deprecated addresses as per RFC 6724 section 5 rule 3.
		if saDep, sbDep := sa.ref.deprecated, sb.ref.deprecated; saDep != sbDep {
			// If sa is not deprecated, it is preferred over sb.
			return sbDep
		}

		// sa and sb are equal, return the endpoint that is closest to the front of
		// the primary endpoint list.
		return i < j
	})

	// Return the most preferred address that can have its reference count
	// incremented.
	for _, c := range cs {
		if r := c.ref; r.tryIncRef() {
			return r
		}
	}

	return nil
}

// hasPermanentAddrLocked returns true if n has a permanent (including currently
// tentative) address, addr.
func (n *NIC) hasPermanentAddrLocked(addr tcpip.Address) bool {
	ref, ok := n.mu.endpoints[NetworkEndpointID{addr}]

	if !ok {
		return false
	}

	kind := ref.getKind()

	return kind == permanent || kind == permanentTentative
}

type getRefBehaviour int

const (
	// spoofing indicates that the NIC's spoofing flag should be observed when
	// getting a NIC's referenced network endpoint.
	spoofing getRefBehaviour = iota

	// promiscuous indicates that the NIC's promiscuous flag should be observed
	// when getting a NIC's referenced network endpoint.
	promiscuous

	// forceSpoofing indicates that the NIC should be assumed to be spoofing,
	// regardless of what the NIC's spoofing flag is when getting a NIC's
	// referenced network endpoint.
	forceSpoofing
)

func (n *NIC) getRef(protocol tcpip.NetworkProtocolNumber, dst tcpip.Address) *referencedNetworkEndpoint {
	return n.getRefOrCreateTemp(protocol, dst, CanBePrimaryEndpoint, promiscuous)
}

// findEndpoint finds the endpoint, if any, with the given address.
func (n *NIC) findEndpoint(protocol tcpip.NetworkProtocolNumber, address tcpip.Address, peb PrimaryEndpointBehavior) *referencedNetworkEndpoint {
	return n.getRefOrCreateTemp(protocol, address, peb, spoofing)
}

// getRefEpOrCreateTemp returns the referenced network endpoint for the given
// protocol and address.
//
// If none exists a temporary one may be created if we are in promiscuous mode
// or spoofing. Promiscuous mode will only be checked if promiscuous is true.
// Similarly, spoofing will only be checked if spoofing is true.
func (n *NIC) getRefOrCreateTemp(protocol tcpip.NetworkProtocolNumber, address tcpip.Address, peb PrimaryEndpointBehavior, tempRef getRefBehaviour) *referencedNetworkEndpoint {
	id := NetworkEndpointID{address}

	n.mu.RLock()

	var spoofingOrPromiscuous bool
	switch tempRef {
	case spoofing:
		spoofingOrPromiscuous = n.mu.spoofing
	case promiscuous:
		spoofingOrPromiscuous = n.mu.promiscuous
	case forceSpoofing:
		spoofingOrPromiscuous = true
	}

	if ref, ok := n.mu.endpoints[id]; ok {
		// An endpoint with this id exists, check if it can be used and return it.
		switch ref.getKind() {
		case permanentExpired:
			if !spoofingOrPromiscuous {
				n.mu.RUnlock()
				return nil
			}
			fallthrough
		case temporary, permanent:
			if ref.tryIncRef() {
				n.mu.RUnlock()
				return ref
			}
		}
	}

	// A usable reference was not found, create a temporary one if requested by
	// the caller or if the address is found in the NIC's subnets.
	createTempEP := spoofingOrPromiscuous
	if !createTempEP {
		for _, sn := range n.mu.addressRanges {
			// Skip the subnet address.
			if address == sn.ID() {
				continue
			}
			// For now just skip the broadcast address, until we support it.
			// FIXME(b/137608825): Add support for sending/receiving directed
			// (subnet) broadcast.
			if address == sn.Broadcast() {
				continue
			}
			if sn.Contains(address) {
				createTempEP = true
				break
			}
		}
	}

	n.mu.RUnlock()

	if !createTempEP {
		return nil
	}

	// Try again with the lock in exclusive mode. If we still can't get the
	// endpoint, create a new "temporary" endpoint. It will only exist while
	// there's a route through it.
	n.mu.Lock()
	if ref, ok := n.mu.endpoints[id]; ok {
		// No need to check the type as we are ok with expired endpoints at this
		// point.
		if ref.tryIncRef() {
			n.mu.Unlock()
			return ref
		}
		// tryIncRef failing means the endpoint is scheduled to be removed once the
		// lock is released. Remove it here so we can create a new (temporary) one.
		// The removal logic waiting for the lock handles this case.
		n.removeEndpointLocked(ref)
	}

	// Add a new temporary endpoint.
	netProto, ok := n.stack.networkProtocols[protocol]
	if !ok {
		n.mu.Unlock()
		return nil
	}
	ref, _ := n.addAddressLocked(tcpip.ProtocolAddress{
		Protocol: protocol,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   address,
			PrefixLen: netProto.DefaultPrefixLen(),
		},
	}, peb, temporary, static, false)

	n.mu.Unlock()
	return ref
}

// addAddressLocked adds a new protocolAddress to n.
//
// If n already has the address in a non-permanent state, and the kind given is
// permanent, that address will be promoted in place and its properties set to
// the properties provided. Otherwise, it returns tcpip.ErrDuplicateAddress.
func (n *NIC) addAddressLocked(protocolAddress tcpip.ProtocolAddress, peb PrimaryEndpointBehavior, kind networkEndpointKind, configType networkEndpointConfigType, deprecated bool) (*referencedNetworkEndpoint, *tcpip.Error) {
	// TODO(b/141022673): Validate IP addresses before adding them.

	// Sanity check.
	id := NetworkEndpointID{LocalAddress: protocolAddress.AddressWithPrefix.Address}
	if ref, ok := n.mu.endpoints[id]; ok {
		// Endpoint already exists.
		if kind != permanent {
			return nil, tcpip.ErrDuplicateAddress
		}
		switch ref.getKind() {
		case permanentTentative, permanent:
			// The NIC already have a permanent endpoint with that address.
			return nil, tcpip.ErrDuplicateAddress
		case permanentExpired, temporary:
			// Promote the endpoint to become permanent and respect the new peb,
			// configType and deprecated status.
			if ref.tryIncRef() {
				// TODO(b/147748385): Perform Duplicate Address Detection when promoting
				// an IPv6 endpoint to permanent.
				ref.setKind(permanent)
				ref.deprecated = deprecated
				ref.configType = configType

				refs := n.mu.primary[ref.protocol]
				for i, r := range refs {
					if r == ref {
						switch peb {
						case CanBePrimaryEndpoint:
							return ref, nil
						case FirstPrimaryEndpoint:
							if i == 0 {
								return ref, nil
							}
							n.mu.primary[r.protocol] = append(refs[:i], refs[i+1:]...)
						case NeverPrimaryEndpoint:
							n.mu.primary[r.protocol] = append(refs[:i], refs[i+1:]...)
							return ref, nil
						}
					}
				}

				n.insertPrimaryEndpointLocked(ref, peb)

				return ref, nil
			}
			// tryIncRef failing means the endpoint is scheduled to be removed once
			// the lock is released. Remove it here so we can create a new
			// (permanent) one. The removal logic waiting for the lock handles this
			// case.
			n.removeEndpointLocked(ref)
		}
	}

	netProto, ok := n.stack.networkProtocols[protocolAddress.Protocol]
	if !ok {
		return nil, tcpip.ErrUnknownProtocol
	}

	// Create the new network endpoint.
	ep, err := netProto.NewEndpoint(n.id, protocolAddress.AddressWithPrefix, n.stack, n, n.linkEP, n.stack)
	if err != nil {
		return nil, err
	}

	isIPv6Unicast := protocolAddress.Protocol == header.IPv6ProtocolNumber && header.IsV6UnicastAddress(protocolAddress.AddressWithPrefix.Address)

	// If the address is an IPv6 address and it is a permanent address,
	// mark it as tentative so it goes through the DAD process if the NIC is
	// enabled. If the NIC is not enabled, DAD will be started when the NIC is
	// enabled.
	if isIPv6Unicast && kind == permanent {
		kind = permanentTentative
	}

	ref := &referencedNetworkEndpoint{
		refs:       1,
		ep:         ep,
		nic:        n,
		protocol:   protocolAddress.Protocol,
		kind:       kind,
		configType: configType,
		deprecated: deprecated,
	}

	// Set up cache if link address resolution exists for this protocol.
	if n.linkEP.Capabilities()&CapabilityResolutionRequired != 0 {
		if _, ok := n.stack.linkAddrResolvers[protocolAddress.Protocol]; ok {
			ref.linkCache = n.stack
		}
	}

	// If we are adding an IPv6 unicast address, join the solicited-node
	// multicast address.
	if isIPv6Unicast {
		snmc := header.SolicitedNodeAddr(protocolAddress.AddressWithPrefix.Address)
		if err := n.joinGroupLocked(protocolAddress.Protocol, snmc); err != nil {
			return nil, err
		}
	}

	n.mu.endpoints[id] = ref

	n.insertPrimaryEndpointLocked(ref, peb)

	// If we are adding a tentative IPv6 address, start DAD if the NIC is enabled.
	if isIPv6Unicast && kind == permanentTentative && n.mu.enabled {
		if err := n.mu.ndp.startDuplicateAddressDetection(protocolAddress.AddressWithPrefix.Address, ref); err != nil {
			return nil, err
		}
	}

	return ref, nil
}

// AddAddress adds a new address to n, so that it starts accepting packets
// targeted at the given address (and network protocol).
func (n *NIC) AddAddress(protocolAddress tcpip.ProtocolAddress, peb PrimaryEndpointBehavior) *tcpip.Error {
	// Add the endpoint.
	n.mu.Lock()
	_, err := n.addAddressLocked(protocolAddress, peb, permanent, static, false /* deprecated */)
	n.mu.Unlock()

	return err
}

// AllAddresses returns all addresses (primary and non-primary) associated with
// this NIC.
func (n *NIC) AllAddresses() []tcpip.ProtocolAddress {
	n.mu.RLock()
	defer n.mu.RUnlock()

	addrs := make([]tcpip.ProtocolAddress, 0, len(n.mu.endpoints))
	for nid, ref := range n.mu.endpoints {
		// Don't include tentative, expired or temporary endpoints to
		// avoid confusion and prevent the caller from using those.
		switch ref.getKind() {
		case permanentExpired, temporary:
			continue
		}
		addrs = append(addrs, tcpip.ProtocolAddress{
			Protocol: ref.protocol,
			AddressWithPrefix: tcpip.AddressWithPrefix{
				Address:   nid.LocalAddress,
				PrefixLen: ref.ep.PrefixLen(),
			},
		})
	}
	return addrs
}

// PrimaryAddresses returns the primary addresses associated with this NIC.
func (n *NIC) PrimaryAddresses() []tcpip.ProtocolAddress {
	n.mu.RLock()
	defer n.mu.RUnlock()

	var addrs []tcpip.ProtocolAddress
	for proto, list := range n.mu.primary {
		for _, ref := range list {
			// Don't include tentative, expired or tempory endpoints
			// to avoid confusion and prevent the caller from using
			// those.
			switch ref.getKind() {
			case permanentTentative, permanentExpired, temporary:
				continue
			}

			addrs = append(addrs, tcpip.ProtocolAddress{
				Protocol: proto,
				AddressWithPrefix: tcpip.AddressWithPrefix{
					Address:   ref.ep.ID().LocalAddress,
					PrefixLen: ref.ep.PrefixLen(),
				},
			})
		}
	}
	return addrs
}

// primaryAddress returns the primary address associated with this NIC.
//
// primaryAddress will return the first non-deprecated address if such an
// address exists. If no non-deprecated address exists, the first deprecated
// address will be returned.
func (n *NIC) primaryAddress(proto tcpip.NetworkProtocolNumber) tcpip.AddressWithPrefix {
	n.mu.RLock()
	defer n.mu.RUnlock()

	list, ok := n.mu.primary[proto]
	if !ok {
		return tcpip.AddressWithPrefix{}
	}

	var deprecatedEndpoint *referencedNetworkEndpoint
	for _, ref := range list {
		// Don't include tentative, expired or tempory endpoints to avoid confusion
		// and prevent the caller from using those.
		switch ref.getKind() {
		case permanentTentative, permanentExpired, temporary:
			continue
		}

		if !ref.deprecated {
			return tcpip.AddressWithPrefix{
				Address:   ref.ep.ID().LocalAddress,
				PrefixLen: ref.ep.PrefixLen(),
			}
		}

		if deprecatedEndpoint == nil {
			deprecatedEndpoint = ref
		}
	}

	if deprecatedEndpoint != nil {
		return tcpip.AddressWithPrefix{
			Address:   deprecatedEndpoint.ep.ID().LocalAddress,
			PrefixLen: deprecatedEndpoint.ep.PrefixLen(),
		}
	}

	return tcpip.AddressWithPrefix{}
}

// AddAddressRange adds a range of addresses to n, so that it starts accepting
// packets targeted at the given addresses and network protocol. The range is
// given by a subnet address, and all addresses contained in the subnet are
// used except for the subnet address itself and the subnet's broadcast
// address.
func (n *NIC) AddAddressRange(protocol tcpip.NetworkProtocolNumber, subnet tcpip.Subnet) {
	n.mu.Lock()
	n.mu.addressRanges = append(n.mu.addressRanges, subnet)
	n.mu.Unlock()
}

// RemoveAddressRange removes the given address range from n.
func (n *NIC) RemoveAddressRange(subnet tcpip.Subnet) {
	n.mu.Lock()

	// Use the same underlying array.
	tmp := n.mu.addressRanges[:0]
	for _, sub := range n.mu.addressRanges {
		if sub != subnet {
			tmp = append(tmp, sub)
		}
	}
	n.mu.addressRanges = tmp

	n.mu.Unlock()
}

// AddressRanges returns the Subnets associated with this NIC.
func (n *NIC) AddressRanges() []tcpip.Subnet {
	n.mu.RLock()
	defer n.mu.RUnlock()
	sns := make([]tcpip.Subnet, 0, len(n.mu.addressRanges)+len(n.mu.endpoints))
	for nid := range n.mu.endpoints {
		sn, err := tcpip.NewSubnet(nid.LocalAddress, tcpip.AddressMask(strings.Repeat("\xff", len(nid.LocalAddress))))
		if err != nil {
			// This should never happen as the mask has been carefully crafted to
			// match the address.
			panic("Invalid endpoint subnet: " + err.Error())
		}
		sns = append(sns, sn)
	}
	return append(sns, n.mu.addressRanges...)
}

// insertPrimaryEndpointLocked adds r to n's primary endpoint list as required
// by peb.
//
// n MUST be locked.
func (n *NIC) insertPrimaryEndpointLocked(r *referencedNetworkEndpoint, peb PrimaryEndpointBehavior) {
	switch peb {
	case CanBePrimaryEndpoint:
		n.mu.primary[r.protocol] = append(n.mu.primary[r.protocol], r)
	case FirstPrimaryEndpoint:
		n.mu.primary[r.protocol] = append([]*referencedNetworkEndpoint{r}, n.mu.primary[r.protocol]...)
	}
}

func (n *NIC) removeEndpointLocked(r *referencedNetworkEndpoint) {
	id := *r.ep.ID()

	// Nothing to do if the reference has already been replaced with a different
	// one. This happens in the case where 1) this endpoint's ref count hit zero
	// and was waiting (on the lock) to be removed and 2) the same address was
	// re-added in the meantime by removing this endpoint from the list and
	// adding a new one.
	if n.mu.endpoints[id] != r {
		return
	}

	if r.getKind() == permanent {
		panic("Reference count dropped to zero before being removed")
	}

	delete(n.mu.endpoints, id)
	refs := n.mu.primary[r.protocol]
	for i, ref := range refs {
		if ref == r {
			n.mu.primary[r.protocol] = append(refs[:i], refs[i+1:]...)
			break
		}
	}

	r.ep.Close()
}

func (n *NIC) removeEndpoint(r *referencedNetworkEndpoint) {
	n.mu.Lock()
	n.removeEndpointLocked(r)
	n.mu.Unlock()
}

func (n *NIC) removePermanentAddressLocked(addr tcpip.Address) *tcpip.Error {
	r, ok := n.mu.endpoints[NetworkEndpointID{addr}]
	if !ok {
		return tcpip.ErrBadLocalAddress
	}

	kind := r.getKind()
	if kind != permanent && kind != permanentTentative {
		return tcpip.ErrBadLocalAddress
	}

	isIPv6Unicast := r.protocol == header.IPv6ProtocolNumber && header.IsV6UnicastAddress(addr)

	if isIPv6Unicast {
		// If we are removing a tentative IPv6 unicast address, stop
		// DAD.
		if kind == permanentTentative {
			n.mu.ndp.stopDuplicateAddressDetection(addr)
		}

		// If we are removing an address generated via SLAAC, cleanup
		// its SLAAC resources and notify the integrator.
		if r.configType == slaac {
			n.mu.ndp.cleanupAutoGenAddrResourcesAndNotify(addr)
		}
	}

	r.setKind(permanentExpired)
	if !r.decRefLocked() {
		// The endpoint still has references to it.
		return nil
	}

	// At this point the endpoint is deleted.

	// If we are removing an IPv6 unicast address, leave the solicited-node
	// multicast address.
	if isIPv6Unicast {
		snmc := header.SolicitedNodeAddr(addr)
		if err := n.leaveGroupLocked(snmc); err != nil {
			return err
		}
	}

	return nil
}

// RemoveAddress removes an address from n.
func (n *NIC) RemoveAddress(addr tcpip.Address) *tcpip.Error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.removePermanentAddressLocked(addr)
}

// joinGroup adds a new endpoint for the given multicast address, if none
// exists yet. Otherwise it just increments its count.
func (n *NIC) joinGroup(protocol tcpip.NetworkProtocolNumber, addr tcpip.Address) *tcpip.Error {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.joinGroupLocked(protocol, addr)
}

// joinGroupLocked adds a new endpoint for the given multicast address, if none
// exists yet. Otherwise it just increments its count. n MUST be locked before
// joinGroupLocked is called.
func (n *NIC) joinGroupLocked(protocol tcpip.NetworkProtocolNumber, addr tcpip.Address) *tcpip.Error {
	// TODO(b/143102137): When implementing MLD, make sure MLD packets are
	// not sent unless a valid link-local address is available for use on n
	// as an MLD packet's source address must be a link-local address as
	// outlined in RFC 3810 section 5.

	id := NetworkEndpointID{addr}
	joins := n.mu.mcastJoins[id]
	if joins == 0 {
		netProto, ok := n.stack.networkProtocols[protocol]
		if !ok {
			return tcpip.ErrUnknownProtocol
		}
		if _, err := n.addAddressLocked(tcpip.ProtocolAddress{
			Protocol: protocol,
			AddressWithPrefix: tcpip.AddressWithPrefix{
				Address:   addr,
				PrefixLen: netProto.DefaultPrefixLen(),
			},
		}, NeverPrimaryEndpoint, permanent, static, false /* deprecated */); err != nil {
			return err
		}
	}
	n.mu.mcastJoins[id] = joins + 1
	return nil
}

// leaveGroup decrements the count for the given multicast address, and when it
// reaches zero removes the endpoint for this address.
func (n *NIC) leaveGroup(addr tcpip.Address) *tcpip.Error {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.leaveGroupLocked(addr)
}

// leaveGroupLocked decrements the count for the given multicast address, and
// when it reaches zero removes the endpoint for this address. n MUST be locked
// before leaveGroupLocked is called.
func (n *NIC) leaveGroupLocked(addr tcpip.Address) *tcpip.Error {
	id := NetworkEndpointID{addr}
	joins := n.mu.mcastJoins[id]
	switch joins {
	case 0:
		// There are no joins with this address on this NIC.
		return tcpip.ErrBadLocalAddress
	case 1:
		// This is the last one, clean up.
		if err := n.removePermanentAddressLocked(addr); err != nil {
			return err
		}
	}
	n.mu.mcastJoins[id] = joins - 1
	return nil
}

func handlePacket(protocol tcpip.NetworkProtocolNumber, dst, src tcpip.Address, localLinkAddr, remotelinkAddr tcpip.LinkAddress, ref *referencedNetworkEndpoint, pkt tcpip.PacketBuffer) {
	r := makeRoute(protocol, dst, src, localLinkAddr, ref, false /* handleLocal */, false /* multicastLoop */)
	r.RemoteLinkAddress = remotelinkAddr
	ref.ep.HandlePacket(&r, pkt)
	ref.decRef()
}

// DeliverNetworkPacket finds the appropriate network protocol endpoint and
// hands the packet over for further processing. This function is called when
// the NIC receives a packet from the link endpoint.
// Note that the ownership of the slice backing vv is retained by the caller.
// This rule applies only to the slice itself, not to the items of the slice;
// the ownership of the items is not retained by the caller.
func (n *NIC) DeliverNetworkPacket(linkEP LinkEndpoint, remote, local tcpip.LinkAddress, protocol tcpip.NetworkProtocolNumber, pkt tcpip.PacketBuffer) {
	n.mu.RLock()
	enabled := n.mu.enabled
	// If the NIC is not yet enabled, don't receive any packets.
	if !enabled {
		n.mu.RUnlock()

		n.stats.DisabledRx.Packets.Increment()
		n.stats.DisabledRx.Bytes.IncrementBy(uint64(pkt.Data.Size()))
		return
	}

	n.stats.Rx.Packets.Increment()
	n.stats.Rx.Bytes.IncrementBy(uint64(pkt.Data.Size()))

	netProto, ok := n.stack.networkProtocols[protocol]
	if !ok {
		n.mu.RUnlock()
		n.stack.stats.UnknownProtocolRcvdPackets.Increment()
		return
	}

	// If no local link layer address is provided, assume it was sent
	// directly to this NIC.
	if local == "" {
		local = n.linkEP.LinkAddress()
	}

	// Are any packet sockets listening for this network protocol?
	packetEPs := n.mu.packetEPs[protocol]
	// Check whether there are packet sockets listening for every protocol.
	// If we received a packet with protocol EthernetProtocolAll, then the
	// previous for loop will have handled it.
	if protocol != header.EthernetProtocolAll {
		packetEPs = append(packetEPs, n.mu.packetEPs[header.EthernetProtocolAll]...)
	}
	n.mu.RUnlock()
	for _, ep := range packetEPs {
		ep.HandlePacket(n.id, local, protocol, pkt.Clone())
	}

	if netProto.Number() == header.IPv4ProtocolNumber || netProto.Number() == header.IPv6ProtocolNumber {
		n.stack.stats.IP.PacketsReceived.Increment()
	}

	if len(pkt.Data.First()) < netProto.MinimumPacketSize() {
		n.stack.stats.MalformedRcvdPackets.Increment()
		return
	}

	src, dst := netProto.ParseAddresses(pkt.Data.First())

	if n.stack.handleLocal && !n.isLoopback() && n.getRef(protocol, src) != nil {
		// The source address is one of our own, so we never should have gotten a
		// packet like this unless handleLocal is false. Loopback also calls this
		// function even though the packets didn't come from the physical interface
		// so don't drop those.
		n.stack.stats.IP.InvalidSourceAddressesReceived.Increment()
		return
	}
	if ref := n.getRef(protocol, dst); ref != nil {
		handlePacket(protocol, dst, src, linkEP.LinkAddress(), remote, ref, pkt)
		return
	}

	// This NIC doesn't care about the packet. Find a NIC that cares about the
	// packet and forward it to the NIC.
	//
	// TODO: Should we be forwarding the packet even if promiscuous?
	if n.stack.Forwarding() {
		r, err := n.stack.FindRoute(0, "", dst, protocol, false /* multicastLoop */)
		if err != nil {
			n.stack.stats.IP.InvalidDestinationAddressesReceived.Increment()
			return
		}

		// Found a NIC.
		n := r.ref.nic
		n.mu.RLock()
		ref, ok := n.mu.endpoints[NetworkEndpointID{dst}]
		ok = ok && ref.isValidForOutgoingRLocked() && ref.tryIncRef()
		n.mu.RUnlock()
		if ok {
			r.LocalLinkAddress = n.linkEP.LinkAddress()
			r.RemoteLinkAddress = remote
			r.RemoteAddress = src
			// TODO(b/123449044): Update the source NIC as well.
			ref.ep.HandlePacket(&r, pkt)
			ref.decRef()
		} else {
			// n doesn't have a destination endpoint.
			// Send the packet out of n.
			// TODO(b/128629022): move this logic to route.WritePacket.
			if ch, err := r.Resolve(nil); err != nil {
				if err == tcpip.ErrWouldBlock {
					n.stack.forwarder.enqueue(ch, n, &r, protocol, pkt)
					// forwarder will release route.
					return
				}
				n.stack.stats.IP.InvalidDestinationAddressesReceived.Increment()
			} else {
				// The link-address resolution finished immediately.
				n.writeForwardingPacket(&r, protocol, pkt)
			}
		}
		r.Release()
		return
	}

	// If a packet socket handled the packet, don't treat it as invalid.
	if len(packetEPs) == 0 {
		n.stack.stats.IP.InvalidDestinationAddressesReceived.Increment()
	}
}

func (n *NIC) writeForwardingPacket(r *Route, protocol tcpip.NetworkProtocolNumber, pkt tcpip.PacketBuffer) {
	// TODO(b/143425874) Decrease the TTL field in forwarded packets.
	pkt.Header = buffer.NewPrependableFromView(pkt.Data.First())
	pkt.Data.RemoveFirst()

	if err := n.linkEP.WritePacket(r, nil /* gso */, protocol, pkt); err != nil {
		r.Stats().IP.OutgoingPacketErrors.Increment()
	} else {
		n.stats.Tx.Packets.Increment()
		n.stats.Tx.Bytes.IncrementBy(uint64(pkt.Header.UsedLength() + pkt.Data.Size()))
	}
}

// DeliverTransportPacket delivers the packets to the appropriate transport
// protocol endpoint.
func (n *NIC) DeliverTransportPacket(r *Route, protocol tcpip.TransportProtocolNumber, pkt tcpip.PacketBuffer) {
	state, ok := n.stack.transportProtocols[protocol]
	if !ok {
		n.stack.stats.UnknownProtocolRcvdPackets.Increment()
		return
	}

	transProto := state.proto

	// Raw socket packets are delivered based solely on the transport
	// protocol number. We do not inspect the payload to ensure it's
	// validly formed.
	n.stack.demux.deliverRawPacket(r, protocol, pkt)

	if len(pkt.Data.First()) < transProto.MinimumPacketSize() {
		n.stack.stats.MalformedRcvdPackets.Increment()
		return
	}

	srcPort, dstPort, err := transProto.ParsePorts(pkt.Data.First())
	if err != nil {
		n.stack.stats.MalformedRcvdPackets.Increment()
		return
	}

	id := TransportEndpointID{dstPort, r.LocalAddress, srcPort, r.RemoteAddress}
	if n.stack.demux.deliverPacket(r, protocol, pkt, id) {
		return
	}

	// Try to deliver to per-stack default handler.
	if state.defaultHandler != nil {
		if state.defaultHandler(r, id, pkt) {
			return
		}
	}

	// We could not find an appropriate destination for this packet, so
	// deliver it to the global handler.
	if !transProto.HandleUnknownDestinationPacket(r, id, pkt) {
		n.stack.stats.MalformedRcvdPackets.Increment()
	}
}

// DeliverTransportControlPacket delivers control packets to the appropriate
// transport protocol endpoint.
func (n *NIC) DeliverTransportControlPacket(local, remote tcpip.Address, net tcpip.NetworkProtocolNumber, trans tcpip.TransportProtocolNumber, typ ControlType, extra uint32, pkt tcpip.PacketBuffer) {
	state, ok := n.stack.transportProtocols[trans]
	if !ok {
		return
	}

	transProto := state.proto

	// ICMPv4 only guarantees that 8 bytes of the transport protocol will
	// be present in the payload. We know that the ports are within the
	// first 8 bytes for all known transport protocols.
	if len(pkt.Data.First()) < 8 {
		return
	}

	srcPort, dstPort, err := transProto.ParsePorts(pkt.Data.First())
	if err != nil {
		return
	}

	id := TransportEndpointID{srcPort, local, dstPort, remote}
	if n.stack.demux.deliverControlPacket(n, net, trans, typ, extra, pkt, id) {
		return
	}
}

// ID returns the identifier of n.
func (n *NIC) ID() tcpip.NICID {
	return n.id
}

// Stack returns the instance of the Stack that owns this NIC.
func (n *NIC) Stack() *Stack {
	return n.stack
}

// isAddrTentative returns true if addr is tentative on n.
//
// Note that if addr is not associated with n, then this function will return
// false. It will only return true if the address is associated with the NIC
// AND it is tentative.
func (n *NIC) isAddrTentative(addr tcpip.Address) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()

	ref, ok := n.mu.endpoints[NetworkEndpointID{addr}]
	if !ok {
		return false
	}

	return ref.getKind() == permanentTentative
}

// dupTentativeAddrDetected attempts to inform n that a tentative addr
// is a duplicate on a link.
//
// dupTentativeAddrDetected will delete the tentative address if it exists.
func (n *NIC) dupTentativeAddrDetected(addr tcpip.Address) *tcpip.Error {
	n.mu.Lock()
	defer n.mu.Unlock()

	ref, ok := n.mu.endpoints[NetworkEndpointID{addr}]
	if !ok {
		return tcpip.ErrBadAddress
	}

	if ref.getKind() != permanentTentative {
		return tcpip.ErrInvalidEndpointState
	}

	return n.removePermanentAddressLocked(addr)
}

// setNDPConfigs sets the NDP configurations for n.
//
// Note, if c contains invalid NDP configuration values, it will be fixed to
// use default values for the erroneous values.
func (n *NIC) setNDPConfigs(c NDPConfigurations) {
	c.validate()

	n.mu.Lock()
	n.mu.ndp.configs = c
	n.mu.Unlock()
}

// handleNDPRA handles an NDP Router Advertisement message that arrived on n.
func (n *NIC) handleNDPRA(ip tcpip.Address, ra header.NDPRouterAdvert) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.mu.ndp.handleRA(ip, ra)
}

type networkEndpointKind int32

const (
	// A permanentTentative endpoint is a permanent address that is not yet
	// considered to be fully bound to an interface in the traditional
	// sense. That is, the address is associated with a NIC, but packets
	// destined to the address MUST NOT be accepted and MUST be silently
	// dropped, and the address MUST NOT be used as a source address for
	// outgoing packets. For IPv6, addresses will be of this kind until
	// NDP's Duplicate Address Detection has resolved, or be deleted if
	// the process results in detecting a duplicate address.
	permanentTentative networkEndpointKind = iota

	// A permanent endpoint is created by adding a permanent address (vs. a
	// temporary one) to the NIC. Its reference count is biased by 1 to avoid
	// removal when no route holds a reference to it. It is removed by explicitly
	// removing the permanent address from the NIC.
	permanent

	// An expired permanent endpoint is a permanent endpoint that had its address
	// removed from the NIC, and it is waiting to be removed once no more routes
	// hold a reference to it. This is achieved by decreasing its reference count
	// by 1. If its address is re-added before the endpoint is removed, its type
	// changes back to permanent and its reference count increases by 1 again.
	permanentExpired

	// A temporary endpoint is created for spoofing outgoing packets, or when in
	// promiscuous mode and accepting incoming packets that don't match any
	// permanent endpoint. Its reference count is not biased by 1 and the
	// endpoint is removed immediately when no more route holds a reference to
	// it. A temporary endpoint can be promoted to permanent if its address
	// is added permanently.
	temporary
)

func (n *NIC) registerPacketEndpoint(netProto tcpip.NetworkProtocolNumber, ep PacketEndpoint) *tcpip.Error {
	n.mu.Lock()
	defer n.mu.Unlock()

	eps, ok := n.mu.packetEPs[netProto]
	if !ok {
		return tcpip.ErrNotSupported
	}
	n.mu.packetEPs[netProto] = append(eps, ep)

	return nil
}

func (n *NIC) unregisterPacketEndpoint(netProto tcpip.NetworkProtocolNumber, ep PacketEndpoint) {
	n.mu.Lock()
	defer n.mu.Unlock()

	eps, ok := n.mu.packetEPs[netProto]
	if !ok {
		return
	}

	for i, epOther := range eps {
		if epOther == ep {
			n.mu.packetEPs[netProto] = append(eps[:i], eps[i+1:]...)
			return
		}
	}
}

type networkEndpointConfigType int32

const (
	// A statically configured endpoint is an address that was added by
	// some user-specified action (adding an explicit address, joining a
	// multicast group).
	static networkEndpointConfigType = iota

	// A slaac configured endpoint is an IPv6 endpoint that was
	// added by SLAAC as per RFC 4862 section 5.5.3.
	slaac
)

type referencedNetworkEndpoint struct {
	ep       NetworkEndpoint
	nic      *NIC
	protocol tcpip.NetworkProtocolNumber

	// linkCache is set if link address resolution is enabled for this
	// protocol. Set to nil otherwise.
	linkCache LinkAddressCache

	// refs is counting references held for this endpoint. When refs hits zero it
	// triggers the automatic removal of the endpoint from the NIC.
	refs int32

	// networkEndpointKind must only be accessed using {get,set}Kind().
	kind networkEndpointKind

	// configType is the method that was used to configure this endpoint.
	// This must never change except during endpoint creation and promotion to
	// permanent.
	configType networkEndpointConfigType

	// deprecated indicates whether or not the endpoint should be considered
	// deprecated. That is, when deprecated is true, other endpoints that are not
	// deprecated should be preferred.
	deprecated bool
}

func (r *referencedNetworkEndpoint) getKind() networkEndpointKind {
	return networkEndpointKind(atomic.LoadInt32((*int32)(&r.kind)))
}

func (r *referencedNetworkEndpoint) setKind(kind networkEndpointKind) {
	atomic.StoreInt32((*int32)(&r.kind), int32(kind))
}

// isValidForOutgoing returns true if the endpoint can be used to send out a
// packet. It requires the endpoint to not be marked expired (i.e., its address
// has been removed), or the NIC to be in spoofing mode.
func (r *referencedNetworkEndpoint) isValidForOutgoing() bool {
	r.nic.mu.RLock()
	defer r.nic.mu.RUnlock()

	return r.isValidForOutgoingRLocked()
}

// isValidForOutgoingRLocked returns true if the endpoint can be used to send
// out a packet. It requires the endpoint to not be marked expired (i.e., its
// address has been removed), or the NIC to be in spoofing mode.
//
// r's NIC must be read locked.
func (r *referencedNetworkEndpoint) isValidForOutgoingRLocked() bool {
	return r.getKind() != permanentExpired || r.nic.mu.spoofing
}

// decRef decrements the ref count and cleans up the endpoint once it reaches
// zero.
func (r *referencedNetworkEndpoint) decRef() {
	if atomic.AddInt32(&r.refs, -1) == 0 {
		r.nic.removeEndpoint(r)
	}
}

// decRefLocked is the same as decRef but assumes that the NIC.mu mutex is
// locked. Returns true if the endpoint was removed.
func (r *referencedNetworkEndpoint) decRefLocked() bool {
	if atomic.AddInt32(&r.refs, -1) == 0 {
		r.nic.removeEndpointLocked(r)
		return true
	}

	return false
}

// incRef increments the ref count. It must only be called when the caller is
// known to be holding a reference to the endpoint, otherwise tryIncRef should
// be used.
func (r *referencedNetworkEndpoint) incRef() {
	atomic.AddInt32(&r.refs, 1)
}

// tryIncRef attempts to increment the ref count from n to n+1, but only if n is
// not zero. That is, it will increment the count if the endpoint is still
// alive, and do nothing if it has already been clean up.
func (r *referencedNetworkEndpoint) tryIncRef() bool {
	for {
		v := atomic.LoadInt32(&r.refs)
		if v == 0 {
			return false
		}

		if atomic.CompareAndSwapInt32(&r.refs, v, v+1) {
			return true
		}
	}
}

// stack returns the Stack instance that owns the underlying endpoint.
func (r *referencedNetworkEndpoint) stack() *Stack {
	return r.nic.stack
}
