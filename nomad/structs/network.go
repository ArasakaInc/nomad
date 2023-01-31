package structs

import (
	"fmt"
	"math/rand"
	"net"
	"sync"

	"golang.org/x/exp/maps"
)

const (
	// DefaultMinDynamicPort is the smallest dynamic port generated by
	// default
	DefaultMinDynamicPort = 20000

	// DefaultMaxDynamicPort is the largest dynamic port generated by
	// default
	DefaultMaxDynamicPort = 32000

	// maxRandPortAttempts is the maximum number of attempt
	// to assign a random port
	maxRandPortAttempts = 20

	// MaxValidPort is the max valid port number
	MaxValidPort = 65536
)

var (
	// bitmapPool is used to pool the bitmaps used for port collision
	// checking. They are fairly large (8K) so we can re-use them to
	// avoid GC pressure. Care should be taken to call Clear() on any
	// bitmap coming from the pool.
	bitmapPool = new(sync.Pool)
)

// NetworkIndex is used to index the available network resources
// and the used network resources on a machine given allocations
//
// Fields are exported so they may be JSON serialized for debugging.
// Fields are *not* intended to be used directly.
type NetworkIndex struct {
	// TaskNetworks are the node networks available for
	// task.resources.network asks.
	TaskNetworks []*NetworkResource

	// GroupNetworks are the node networks available for group.network
	// asks.
	GroupNetworks []*NodeNetworkResource

	// HostNetworks indexes addresses by host network alias
	HostNetworks map[string][]NodeNetworkAddress

	// UsedPorts tracks which ports are used on a per-IP address basis. For
	// example if a node has `network_interface=lo` and port 22 reserved,
	// then on a dual stack loopback interface UsedPorts would contain:
	// {
	//  "127.0.0.1": Bitmap{22},
	//  "::1":       Bitmap{22},
	// }
	UsedPorts map[string]Bitmap

	// Deprecated bandwidth fields
	AvailBandwidth map[string]int // Bandwidth by device
	UsedBandwidth  map[string]int // Bandwidth by device

	MinDynamicPort int // The smallest dynamic port generated
	MaxDynamicPort int // The largest dynamic port generated
}

// NewNetworkIndex is used to construct a new network index
func NewNetworkIndex() *NetworkIndex {
	return &NetworkIndex{
		HostNetworks:   make(map[string][]NodeNetworkAddress),
		UsedPorts:      make(map[string]Bitmap),
		AvailBandwidth: make(map[string]int),
		UsedBandwidth:  make(map[string]int),
		MinDynamicPort: DefaultMinDynamicPort,
		MaxDynamicPort: DefaultMaxDynamicPort,
	}
}

func (idx *NetworkIndex) getUsedPortsFor(ip string) Bitmap {
	used := idx.UsedPorts[ip]
	if used == nil {
		// Try to get a bitmap from the pool, else create
		raw := bitmapPool.Get()
		if raw != nil {
			used = raw.(Bitmap)
			used.Clear()
		} else {
			used, _ = NewBitmap(MaxValidPort)
		}
		idx.UsedPorts[ip] = used
	}
	return used
}

func (idx *NetworkIndex) Copy() *NetworkIndex {
	if idx == nil {
		return nil
	}

	c := new(NetworkIndex)
	*c = *idx

	c.TaskNetworks = copyNetworkResources(idx.TaskNetworks)
	c.GroupNetworks = copyNodeNetworks(idx.GroupNetworks)
	c.HostNetworks = copyAvailAddresses(idx.HostNetworks)
	if idx.AvailBandwidth != nil && len(idx.AvailBandwidth) == 0 {
		c.AvailBandwidth = make(map[string]int)
	} else {
		c.AvailBandwidth = maps.Clone(idx.AvailBandwidth)
	}
	if len(idx.UsedPorts) > 0 {
		c.UsedPorts = make(map[string]Bitmap, len(idx.UsedPorts))
		for k, v := range idx.UsedPorts {
			c.UsedPorts[k], _ = v.Copy()
		}
	}
	if idx.UsedBandwidth != nil && len(idx.UsedBandwidth) == 0 {
		c.UsedBandwidth = make(map[string]int)
	} else {
		c.UsedBandwidth = maps.Clone(idx.UsedBandwidth)
	}

	return c
}

func copyNetworkResources(resources []*NetworkResource) []*NetworkResource {
	l := len(resources)
	if l == 0 {
		return nil
	}

	c := make([]*NetworkResource, l)
	for i, resource := range resources {
		c[i] = resource.Copy()
	}
	return c
}

func copyNodeNetworks(resources []*NodeNetworkResource) []*NodeNetworkResource {
	l := len(resources)
	if l == 0 {
		return nil
	}

	c := make([]*NodeNetworkResource, l)
	for i, resource := range resources {
		c[i] = resource.Copy()
	}
	return c
}

func copyAvailAddresses(a map[string][]NodeNetworkAddress) map[string][]NodeNetworkAddress {
	l := len(a)
	if l == 0 {
		return nil
	}

	c := make(map[string][]NodeNetworkAddress, l)
	for k, v := range a {
		if len(v) == 0 {
			continue
		}
		c[k] = make([]NodeNetworkAddress, len(v))
		copy(c[k], v)
	}

	return c
}

// Release is called when the network index is no longer needed
// to attempt to re-use some of the memory it has allocated
func (idx *NetworkIndex) Release() {
	for _, b := range idx.UsedPorts {
		bitmapPool.Put(b)
	}
}

// Overcommitted checks if the network is overcommitted
func (idx *NetworkIndex) Overcommitted() bool {
	// TODO remove since bandwidth is deprecated
	/*for device, used := range idx.UsedBandwidth {
		avail := idx.AvailBandwidth[device]
		if used > avail {
			return true
		}
	}*/
	return false
}

// SetNode is used to initialize a node's network index with available IPs,
// reserved ports, and other details from a node's configuration and
// fingerprinting.
//
// SetNode must be idempotent as preemption causes SetNode to be called
// multiple times on the same NetworkIndex, only clearing UsedPorts between
// calls.
//
// An error is returned if the Node cannot produce a consistent NetworkIndex
// such as if reserved_ports are unparseable.
//
// Any errors returned by SetNode indicate a bug! The bug may lie in client
// code not properly validating its configuration or it may lie in improper
// Node object handling by servers. Users should not be able to cause SetNode
// to error. Data that cause SetNode to error should be caught upstream such as
// a client agent refusing to start with an invalid configuration.
func (idx *NetworkIndex) SetNode(node *Node) error {

	// COMPAT(0.11): Deprecated. taskNetworks are only used for
	// task.resources.network asks which have been deprecated since before
	// 0.11.
	// Grab the network resources, handling both new and old Node layouts
	// from clients.
	var taskNetworks []*NetworkResource
	if node.NodeResources != nil && len(node.NodeResources.Networks) != 0 {
		taskNetworks = node.NodeResources.Networks
	} else if node.Resources != nil {
		taskNetworks = node.Resources.Networks
	}

	// Reserved ports get merged downward. For example given an agent
	// config:
	//
	// client.reserved.reserved_ports = "22"
	// client.host_network["eth0"] = {reserved_ports = "80,443"}
	// client.host_network["eth1"] = {reserved_ports = "1-1000"}
	//
	// Addresses on taskNetworks reserve port 22
	// Addresses on eth0 reserve 22,80,443 (note 22 is also reserved!)
	// Addresses on eth1 reserve 1-1000
	globalResPorts := []uint{}

	if node.ReservedResources != nil && node.ReservedResources.Networks.ReservedHostPorts != "" {
		resPorts, err := ParsePortRanges(node.ReservedResources.Networks.ReservedHostPorts)
		if err != nil {
			// This is a fatal error that should have been
			// prevented by client validation.
			return fmt.Errorf("error parsing reserved_ports: %w", err)
		}

		globalResPorts = make([]uint, len(resPorts))
		for i, p := range resPorts {
			globalResPorts[i] = uint(p)
		}
	} else if node.Reserved != nil {
		// COMPAT(0.11): Remove after 0.11. Nodes stopped reporting
		// reserved ports under Node.Reserved.Resources in #4750 / v0.9
		for _, n := range node.Reserved.Networks {
			used := idx.getUsedPortsFor(n.IP)
			for _, ports := range [][]Port{n.ReservedPorts, n.DynamicPorts} {
				for _, p := range ports {
					if p.Value > MaxValidPort || p.Value < 0 {
						// This is a fatal error that
						// should have been prevented
						// by validation upstream.
						return fmt.Errorf("invalid port %d for reserved_ports", p.Value)
					}

					globalResPorts = append(globalResPorts, uint(p.Value))
					used.Set(uint(p.Value))
				}
			}

			// Reserve mbits
			if n.Device != "" {
				idx.UsedBandwidth[n.Device] += n.MBits
			}
		}
	}

	// Filter task networks down to those with a device. For example
	// taskNetworks may contain a "bridge" interface which has no device
	// set and cannot be used to fulfill asks.
	for _, n := range taskNetworks {
		if n.Device != "" {
			idx.TaskNetworks = append(idx.TaskNetworks, n)
			idx.AvailBandwidth[n.Device] = n.MBits

			// Reserve ports
			used := idx.getUsedPortsFor(n.IP)
			for _, p := range globalResPorts {
				used.Set(p)
			}
		}
	}

	// nodeNetworks are used for group.network asks.
	var nodeNetworks []*NodeNetworkResource
	if node.NodeResources != nil && len(node.NodeResources.NodeNetworks) != 0 {
		nodeNetworks = node.NodeResources.NodeNetworks
	}

	for _, n := range nodeNetworks {
		for _, a := range n.Addresses {
			// Index host networks by their unique alias for asks
			// with group.network.port.host_network set.
			idx.HostNetworks[a.Alias] = append(idx.HostNetworks[a.Alias], a)

			// Mark reserved ports as used without worrying about
			// collisions. This effectively merges
			// client.reserved.reserved_ports into each
			// host_network.
			used := idx.getUsedPortsFor(a.Address)
			for _, p := range globalResPorts {
				used.Set(p)
			}

			// If ReservedPorts is set on the NodeNetwork, use it
			// and the global reserved ports.
			if a.ReservedPorts != "" {
				rp, err := ParsePortRanges(a.ReservedPorts)
				if err != nil {
					// This is a fatal error that should
					// have been prevented by validation
					// upstream.
					return fmt.Errorf("error parsing reserved_ports for network %q: %w", a.Alias, err)
				}
				for _, p := range rp {
					used.Set(uint(p))
				}
			}
		}
	}

	// Set dynamic port range (applies to all addresses)
	if node.NodeResources != nil && node.NodeResources.MinDynamicPort > 0 {
		idx.MinDynamicPort = node.NodeResources.MinDynamicPort
	}

	if node.NodeResources != nil && node.NodeResources.MaxDynamicPort > 0 {
		idx.MaxDynamicPort = node.NodeResources.MaxDynamicPort
	}

	return nil
}

// AddAllocs is used to add the used network resources. Returns
// true if there is a collision
//
// AddAllocs may be called multiple times for the same NetworkIndex with
// UsedPorts cleared between calls (by Release). Therefore AddAllocs must be
// determistic and must not manipulate state outside of UsedPorts as that state
// would persist between Release calls.
func (idx *NetworkIndex) AddAllocs(allocs []*Allocation) (collide bool, reason string) {
	for _, alloc := range allocs {
		// Do not consider the resource impact of terminal allocations
		if alloc.TerminalStatus() {
			continue
		}

		if alloc.AllocatedResources != nil {
			// Only look at AllocatedPorts if populated, otherwise use pre 0.12 logic
			// COMPAT(1.0): Remove when network resources struct is removed.
			if len(alloc.AllocatedResources.Shared.Ports) > 0 {
				if c, r := idx.AddReservedPorts(alloc.AllocatedResources.Shared.Ports); c {
					collide = true
					reason = fmt.Sprintf("collision when reserving port for alloc %s: %v", alloc.ID, r)
				}
			} else {
				// Add network resources that are at the task group level
				if len(alloc.AllocatedResources.Shared.Networks) > 0 {
					for _, network := range alloc.AllocatedResources.Shared.Networks {
						if c, r := idx.AddReserved(network); c {
							collide = true
							reason = fmt.Sprintf("collision when reserving port for network %s in alloc %s: %v", network.IP, alloc.ID, r)
						}
					}
				}

				for task, resources := range alloc.AllocatedResources.Tasks {
					if len(resources.Networks) == 0 {
						continue
					}
					n := resources.Networks[0]
					if c, r := idx.AddReserved(n); c {
						collide = true
						reason = fmt.Sprintf("collision when reserving port for network %s in task %s of alloc %s: %v", n.IP, task, alloc.ID, r)
					}
				}
			}
		} else {
			// COMPAT(0.11): Remove in 0.11
			for task, resources := range alloc.TaskResources {
				if len(resources.Networks) == 0 {
					continue
				}
				n := resources.Networks[0]
				if c, r := idx.AddReserved(n); c {
					collide = true
					reason = fmt.Sprintf("(deprecated) collision when reserving port for network %s in task %s of alloc %s: %v", n.IP, task, alloc.ID, r)
				}
			}
		}
	}
	return
}

// AddReserved is used to add a reserved network usage, returns true
// if there is a port collision
func (idx *NetworkIndex) AddReserved(n *NetworkResource) (collide bool, reasons []string) {
	// Add the port usage
	used := idx.getUsedPortsFor(n.IP)

	for _, ports := range [][]Port{n.ReservedPorts, n.DynamicPorts} {
		for _, port := range ports {
			// Guard against invalid port
			if port.Value < 0 || port.Value >= MaxValidPort {
				return true, []string{fmt.Sprintf("invalid port %d", port.Value)}
			}
			if used.Check(uint(port.Value)) {
				collide = true
				reason := fmt.Sprintf("port %d already in use", port.Value)
				reasons = append(reasons, reason)
			} else {
				used.Set(uint(port.Value))
			}
		}
	}

	// Add the bandwidth
	idx.UsedBandwidth[n.Device] += n.MBits
	return
}

func (idx *NetworkIndex) AddReservedPorts(ports AllocatedPorts) (collide bool, reasons []string) {
	for _, port := range ports {
		used := idx.getUsedPortsFor(port.HostIP)
		if port.Value < 0 || port.Value >= MaxValidPort {
			return true, []string{fmt.Sprintf("invalid port %d", port.Value)}
		}
		if used.Check(uint(port.Value)) {
			collide = true
			reason := fmt.Sprintf("port %d already in use", port.Value)
			reasons = append(reasons, reason)
		} else {
			used.Set(uint(port.Value))
		}
	}

	return
}

// AddReservedPortsForIP checks whether any reserved ports collide with those
// in use for the IP address.
func (idx *NetworkIndex) AddReservedPortsForIP(ports []uint64, ip string) (collide bool, reasons []string) {
	used := idx.getUsedPortsFor(ip)
	for _, port := range ports {
		// Guard against invalid port
		if port >= MaxValidPort {
			return true, []string{fmt.Sprintf("invalid port %d", port)}
		}
		if used.Check(uint(port)) {
			collide = true
			reason := fmt.Sprintf("port %d already in use", port)
			reasons = append(reasons, reason)
		} else {
			used.Set(uint(port))
		}
	}

	return
}

// yieldIP is used to iteratively invoke the callback with
// an available IP
func (idx *NetworkIndex) yieldIP(cb func(net *NetworkResource, offerIP net.IP) bool) {
	for _, n := range idx.TaskNetworks {
		ip, ipnet, err := net.ParseCIDR(n.CIDR)
		if err != nil {
			continue
		}
		for ip := ip.Mask(ipnet.Mask); ipnet.Contains(ip); incIP(ip) {
			if cb(n, ip) {
				return
			}
		}
	}
}

func incIP(ip net.IP) {
	// Iterate over IP octects from right to left
	for j := len(ip) - 1; j >= 0; j-- {

		// Increment octect
		ip[j]++

		// If this octect did not wrap around to 0, it's the next IP to
		// try. If it did wrap (p[j]==0), then the next octect is
		// incremented.
		if ip[j] > 0 {
			break
		}
	}
}

// AssignPorts based on an ask from the scheduler processing a group.network
// block. Supports multi-interfaces through node configured host_networks.
//
// AssignTaskNetwork supports the deprecated task.resources.network block.
func (idx *NetworkIndex) AssignPorts(ask *NetworkResource) (AllocatedPorts, error) {
	var offer AllocatedPorts

	// index of host network name to slice of reserved ports, used during dynamic port assignment
	reservedIdx := map[string][]Port{}

	for _, port := range ask.ReservedPorts {
		reservedIdx[port.HostNetwork] = append(reservedIdx[port.HostNetwork], port)

		// allocPort is set in the inner for loop if a port mapping can be created
		// if allocPort is still nil after the loop, the port wasn't available for reservation
		var allocPort *AllocatedPortMapping
		var addrErr error
		for _, addr := range idx.HostNetworks[port.HostNetwork] {
			used := idx.getUsedPortsFor(addr.Address)
			// Guard against invalid port
			if port.Value < 0 || port.Value >= MaxValidPort {
				return nil, fmt.Errorf("invalid port %d (out of range)", port.Value)
			}

			// Check if in use
			if used != nil && used.Check(uint(port.Value)) {
				return nil, fmt.Errorf("reserved port collision %s=%d", port.Label, port.Value)
			}

			allocPort = &AllocatedPortMapping{
				Label:  port.Label,
				Value:  port.Value,
				To:     port.To,
				HostIP: addr.Address,
			}
			break
		}

		if allocPort == nil {
			if addrErr != nil {
				return nil, addrErr
			}

			return nil, fmt.Errorf("no addresses available for %s network", port.HostNetwork)
		}

		offer = append(offer, *allocPort)
	}

	for _, port := range ask.DynamicPorts {
		var allocPort *AllocatedPortMapping
		var addrErr error
		for _, addr := range idx.HostNetworks[port.HostNetwork] {
			used := idx.getUsedPortsFor(addr.Address)
			// Try to stochastically pick the dynamic ports as it is faster and
			// lower memory usage.
			var dynPorts []int
			// TODO: its more efficient to find multiple dynamic ports at once
			dynPorts, addrErr = getDynamicPortsStochastic(used, idx.MinDynamicPort, idx.MaxDynamicPort, reservedIdx[port.HostNetwork], 1)
			if addrErr != nil {
				// Fall back to the precise method if the random sampling failed.
				dynPorts, addrErr = getDynamicPortsPrecise(used, idx.MinDynamicPort, idx.MaxDynamicPort, reservedIdx[port.HostNetwork], 1)
				if addrErr != nil {
					continue
				}
			}

			allocPort = &AllocatedPortMapping{
				Label:  port.Label,
				Value:  dynPorts[0],
				To:     port.To,
				HostIP: addr.Address,
			}
			if allocPort.To == -1 {
				allocPort.To = allocPort.Value
			}
			break
		}

		if allocPort == nil {
			if addrErr != nil {
				return nil, addrErr
			}

			return nil, fmt.Errorf("no addresses available for %s network", port.HostNetwork)
		}
		offer = append(offer, *allocPort)
	}

	return offer, nil
}

// AssignTaskNetwork is used to offer network resources given a
// task.resources.network ask.  If the ask cannot be satisfied, returns nil
//
// AssignTaskNetwork and task.resources.network are deprecated in favor of
// AssignPorts and group.network. AssignTaskNetwork does not support multiple
// interfaces and only uses the node's default interface. AssignPorts is the
// method that is used for group.network asks.
func (idx *NetworkIndex) AssignTaskNetwork(ask *NetworkResource) (out *NetworkResource, err error) {
	err = fmt.Errorf("no networks available")
	idx.yieldIP(func(n *NetworkResource, offerIP net.IP) (stop bool) {
		// Convert the IP to a string
		offerIPStr := offerIP.String()

		// Check if we would exceed the bandwidth cap
		availBandwidth := idx.AvailBandwidth[n.Device]
		usedBandwidth := idx.UsedBandwidth[n.Device]
		if usedBandwidth+ask.MBits > availBandwidth {
			err = fmt.Errorf("bandwidth exceeded")
			return
		}

		used := idx.UsedPorts[offerIPStr]

		// Check if any of the reserved ports are in use
		for _, port := range ask.ReservedPorts {
			// Guard against invalid port
			if port.Value < 0 || port.Value >= MaxValidPort {
				err = fmt.Errorf("invalid port %d (out of range)", port.Value)
				return
			}

			// Check if in use
			if used != nil && used.Check(uint(port.Value)) {
				err = fmt.Errorf("reserved port collision %s=%d", port.Label, port.Value)
				return
			}
		}

		// Create the offer
		offer := &NetworkResource{
			Mode:          ask.Mode,
			Device:        n.Device,
			IP:            offerIPStr,
			MBits:         ask.MBits,
			DNS:           ask.DNS,
			ReservedPorts: ask.ReservedPorts,
			DynamicPorts:  ask.DynamicPorts,
		}

		// Try to stochastically pick the dynamic ports as it is faster and
		// lower memory usage.
		var dynPorts []int
		var dynErr error
		dynPorts, dynErr = getDynamicPortsStochastic(used, idx.MinDynamicPort, idx.MaxDynamicPort, ask.ReservedPorts, len(ask.DynamicPorts))
		if dynErr == nil {
			goto BUILD_OFFER
		}

		// Fall back to the precise method if the random sampling failed.
		dynPorts, dynErr = getDynamicPortsPrecise(used, idx.MinDynamicPort, idx.MaxDynamicPort, ask.ReservedPorts, len(ask.DynamicPorts))
		if dynErr != nil {
			err = dynErr
			return
		}

	BUILD_OFFER:
		for i, port := range dynPorts {
			offer.DynamicPorts[i].Value = port

			// This syntax allows you to set the mapped to port to the same port
			// allocated by the scheduler on the host.
			if offer.DynamicPorts[i].To == -1 {
				offer.DynamicPorts[i].To = port
			}
		}

		// Stop, we have an offer!
		out = offer
		err = nil
		return true
	})
	return
}

// getDynamicPortsPrecise takes the nodes used port bitmap which may be nil if
// no ports have been allocated yet, the network ask and returns a set of unused
// ports to fulfil the ask's DynamicPorts or an error if it failed. An error
// means the ask can not be satisfied as the method does a precise search.
func getDynamicPortsPrecise(nodeUsed Bitmap, minDynamicPort, maxDynamicPort int, reserved []Port, numDyn int) ([]int, error) {
	// Create a copy of the used ports and apply the new reserves
	var usedSet Bitmap
	var err error
	if nodeUsed != nil {
		usedSet, err = nodeUsed.Copy()
		if err != nil {
			return nil, err
		}
	} else {
		usedSet, err = NewBitmap(MaxValidPort)
		if err != nil {
			return nil, err
		}
	}

	for _, port := range reserved {
		usedSet.Set(uint(port.Value))
	}

	// Get the indexes of the unset
	availablePorts := usedSet.IndexesInRange(false, uint(minDynamicPort), uint(maxDynamicPort))

	// Randomize the amount we need
	if len(availablePorts) < numDyn {
		return nil, fmt.Errorf("dynamic port selection failed")
	}

	numAvailable := len(availablePorts)
	for i := 0; i < numDyn; i++ {
		j := rand.Intn(numAvailable)
		availablePorts[i], availablePorts[j] = availablePorts[j], availablePorts[i]
	}

	return availablePorts[:numDyn], nil
}

// getDynamicPortsStochastic takes the nodes used port bitmap which may be nil if
// no ports have been allocated yet, the network ask and returns a set of unused
// ports to fulfil the ask's DynamicPorts or an error if it failed. An error
// does not mean the ask can not be satisfied as the method has a fixed amount
// of random probes and if these fail, the search is aborted.
func getDynamicPortsStochastic(nodeUsed Bitmap, minDynamicPort, maxDynamicPort int, reservedPorts []Port, count int) ([]int, error) {
	var reserved, dynamic []int
	for _, port := range reservedPorts {
		reserved = append(reserved, port.Value)
	}

	for i := 0; i < count; i++ {
		attempts := 0
	PICK:
		attempts++
		if attempts > maxRandPortAttempts {
			return nil, fmt.Errorf("stochastic dynamic port selection failed")
		}

		randPort := minDynamicPort + rand.Intn(maxDynamicPort-minDynamicPort)
		if nodeUsed != nil && nodeUsed.Check(uint(randPort)) {
			goto PICK
		}

		for _, ports := range [][]int{reserved, dynamic} {
			if isPortReserved(ports, randPort) {
				goto PICK
			}
		}
		dynamic = append(dynamic, randPort)
	}

	return dynamic, nil
}

// IntContains scans an integer slice for a value
func isPortReserved(haystack []int, needle int) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

// AllocatedPortsToNetworkResouce is a COMPAT(1.0) remove when NetworkResource
// is no longer used for materialized client view of ports.
func AllocatedPortsToNetworkResouce(ask *NetworkResource, ports AllocatedPorts, node *NodeResources) *NetworkResource {
	out := ask.Copy()

	for i, port := range ask.DynamicPorts {
		if p, ok := ports.Get(port.Label); ok {
			out.DynamicPorts[i].Value = p.Value
			out.DynamicPorts[i].To = p.To
		}
	}
	if len(node.NodeNetworks) > 0 {
		for _, nw := range node.NodeNetworks {
			if nw.Mode == "host" {
				out.IP = nw.Addresses[0].Address
				break
			}
		}
	} else {
		for _, nw := range node.Networks {
			if nw.Mode == "host" {
				out.IP = nw.IP
			}
		}
	}
	return out
}

type ClientHostNetworkConfig struct {
	Name          string `hcl:",key"`
	CIDR          string `hcl:"cidr"`
	Interface     string `hcl:"interface"`
	ReservedPorts string `hcl:"reserved_ports"`
}

func (p *ClientHostNetworkConfig) Copy() *ClientHostNetworkConfig {
	if p == nil {
		return nil
	}

	c := new(ClientHostNetworkConfig)
	*c = *p
	return c
}
