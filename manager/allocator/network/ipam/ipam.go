package ipam

import (
	"fmt"
	"net"

	"github.com/docker/libnetwork/ipamapi"
	"github.com/docker/libnetwork/netlabel"

	"github.com/docker/swarmkit/api"
)

// DrvRegistry is an interface defining
type DrvRegistry interface {
	IPAM(name string) (ipamapi.Ipam, *ipamapi.Capability)
}

// network is a private type used to hold the internal state of a network in
// the IP Allocator
type network struct {
	// nw is a local cache of the network object found in the store
	nw *api.Network

	// pools is used to save the internal poolIDs needed when releasing a pool.
	// It maps an ip address to a pool ID.
	pools map[string]string

	// endpoints is a map of the endpoint IP to the poolID from which it was
	// allocated.
	endpoints map[string]string
}

// Allocator is an allocator for IP addresses and IPAM pools. It handles all
// allocation and deallocation of IP addresses, both for VIPs and endpoints, in
// addition to handling IPAM pools for networks.
type Allocator struct {
	// networks maps network IDs to the locally stored network object
	networks map[string]*network

	// drvRegistry is the driver registry, from which we can get active network
	// drivers
	drvRegistry DrvRegistry
}

func NewAllocator(reg DrvRegistry) *Allocator {
	return &Allocator{
		networks:    make(map[string]*network),
		drvRegistry: reg,
	}
}

// Restore restores the state of the provided networks to the Allocator. It can
// return errors if the initialization of its dependencies fails.
func (a *Allocator) Restore(networks []*api.Network, endpoints []*api.Endpoint, attachments []*api.NetworkAttachment) error {
	// Initialize the state of the networks. this gets the initial IPAM pools.
	for _, nw := range networks {
		// if the network has no IPAM field, it has no state, and there is
		// nothing to do
		if nw.IPAM == nil {
			continue
		}
		// if the network has no IPAM driver, it has no IPAM state, and there
		// is nothing to do.
		if nw.IPAM.Driver == nil {
			continue
		}
		// if the network has no ipam configs, then it has no state, and there
		// is nothing to do
		if len(nw.IPAM.Configs) == 0 {
			continue
		}
		local := &network{
			nw:        nw,
			pools:     make(map[string]string),
			endpoints: make(map[string]string),
		}
		ipamName := nw.IPAM.Driver.Name
		ipamOpts := nw.IPAM.Driver.Options
		if ipamOpts == nil {
			ipamOpts = map[string]string{}
		}
		// if we have an ipam driver name and we have IPAM configs, but for
		// some reason we don't have IPAM options, just fill that field in.
		// shouldn't do any harm. i doubt we'll ever hit this in prod
		local.nw.IPAM.Driver.Options = ipamOpts
		// IPAM returns the Ipam object and also its capabilities. We don't
		// use the capabilities or care about them right now, so just ignore
		// that part of the return value
		ipam, _ := a.drvRegistry.IPAM(ipamName)
		if ipam == nil {
			return ErrInvalidIPAM{ipamName}
		}
		_, addressSpace, err := ipam.GetDefaultAddressSpaces()
		// the only errors here result from having an invalid ipam driver
		if err != nil {
			return ErrInvalidIPAM{ipamName}
		}

		// now initialize the IPAM pools. IPAM pools are the set of addresses
		// available for a specific network. There is one IPAM config for every
		// pool. In order for thsi restore operation to be consistent, a
		// network must have no ipam configs if it hasn't been allocated
		for _, config := range local.nw.IPAM.Configs {
			// the last param of RequestPool is "v6", meaning IPv6, which we
			// don't support, hence passing "false"
			poolID, poolIP, _, err := ipam.RequestPool(addressSpace, config.Subnet, config.Range, ipamOpts, false)
			if err != nil {
				// typically, if there was an error in requesting a pool, we
				// would release the pools we've already allocated. However, if
				// there is an error at this stage, it means the whole object
				// store is in a bad state, so we don't do that, we just
				// abandon everything.
				// TODO(dperny): return a structured error here
				return fmt.Errorf("error reserving ipam pool: %v", err)
			}
			// each IPAM pool we use has a separate ID referring to it. We also
			// keep a map of each IP address we have allocated for this network
			// and what pool it belongs to, so we can deallocate an address
			// from the correct pool
			local.pools[poolIP.String()] = poolID
			// now we just need to reserve the gateway address for this pool.
			// set the IPAM request address type to "Gateway" to tell the IPAM
			// driver that's the kind of address we're requesting
			ipamOpts[ipamapi.RequestAddressType] = netlabel.Gateway
			// and also set the ipam SerialAlloc option if it's not explicitly
			// unset. this will persist through the whole local state.
			if _, ok := ipamOpts[ipamapi.AllocSerialPrefix]; !ok {
				ipamOpts[ipamapi.AllocSerialPrefix] = "true"
			}
			// delete the Gateway option from the IPAM options when we're done
			delete(ipamOpts, ipamapi.RequestAddressType)
			// and now, if we have a Gateway address for this network, we need
			// to allocate it. This condition should never happen; a network
			// with a valid IPAM config but an empty gateway is a recipe for
			// disaster.  This is just included for completeness if an older
			// version has incorrect state
			if config.Gateway != "" {
				gwIP, _, err := ipam.RequestAddress(poolID, net.ParseIP(config.Gateway), ipamOpts)
				if err != nil {
					// TODO(dperny) structured errors
					return fmt.Errorf("error requesting already assigned gateway address %v")
				}
				if gwIP.String() != config.Gateway {
					// if we get an IP address from this that isn't the one we
					// requested, that's Very Bad.
					// TODO(dperny) not sure if we need this check
					return fmt.Errorf("got back gateway ip %v, but requested ip %v", gwIP, config.Gateway)
				}
				// most addresses need to be added to the map of
				// address -> poolid, but we don't need to do this with the
				// gateway address because it's store in the ipam config with
				// the subnet, which is the key for the pools map.
			}
			// finally, add the network to the list of networks we're keeping
			// track of.
			a.networks[local.nw.ID] = local
		}
	}

	// now restore the VIPs and attachment addresses

	// first the VIPs
	for _, endpoint := range endpoints {
		for _, vip := range endpoint.VirtualIPs {
			// there shouldn't be nil vips but lord knows what kind of black
			// magic goes on in other parts of the code or in the old
			// allocator, and it doesn't really cost anything
			// also, check that the VIP address is allocated, so we don't try
			// to allocate a new VIP. that's the most common class of error in
			// the old allocator
			if vip != nil && vip.Addr != "" {
				if err := a.restoreAddress(vip.NetworkID, vip.Addr); err != nil {
					return err
				}
			}
		}
	}

	// now the attachments
	for _, attachment := range attachments {
		// nil checking for the same reaason as VIPs. why is everything a
		// pointer
		if attachment != nil && attachment.Network != nil {
			nwid := attachment.Network.ID
			for _, addr := range attachment.Addresses {
				// we shouldn't have empty addresses here but who knows what
				// state we've inherited from the old allocator
				if addr == "" {
					continue
				}
				if err := a.restoreAddress(nwid, addr); err != nil {
					return err
				}
			}
		}
	}

	// finally, everything is reallocated and we're ready to go and allocate
	// new things.
	return nil
}

// restoreAddress is the common functionality needed to mark a given address in
// use for the given network ID. if the address given is accidentally empty,
// we'll return nil as there is nothing to restore but no error has occurred
func (a *Allocator) restoreAddress(nwid string, address string) error {
	// TODO(dperny): do we want this check...? the purpose of this function is
	// primarily to deduplicate code common between restoring VIPs and
	// Attachments
	if address == "" {
		return nil
	}
	// first, get the local network state and IPAM driver
	local, ok := a.networks[nwid]
	if !ok {
		return ErrNetworkNotAllocated{nwid}
	}
	ipam, _ := a.drvRegistry.IPAM(local.nw.IPAM.Driver.Name)
	if ipam == nil {
		return ErrInvalidIPAM{local.nw.IPAM.Driver.Name}
	}
	ipamOpts := local.nw.IPAM.Driver.Options
	// NOTE(dperny): this code, where we try parsing as CIDR and
	// then as a regular IP, is from the old allocator. I do not
	// know why this is done this way
	addr, _, err := net.ParseCIDR(address)
	if err != nil {
		addr = net.ParseIP(address)
		if addr == nil {
			return ErrInvalidAddress{address}
		}
	}
	// we don't know which pool this address belongs to, so go through each
	// pool in this network and try to request this address.
	//
	// NOTE(dperny): this code is couched in 2 assumptions:
	//   - IPAM pools can't overlap
	//   - IPAM address requests will prefer "out of range" to "no available
	//	   IPs"
	// The first one I'm rather sure of, but the second i'm not... I don't know
	// what the response would be if the pool had no remaining addresses but
	// the requested address was out of range anyway
	for _, poolID := range local.pools {
		ip, _, err := ipam.RequestAddress(poolID, addr, ipamOpts)
		if err == ipamapi.ErrIPOutOfRange {
			continue
		}
		if err != nil {
			// TODO(dperny): structure errors
			return err
		}
		// make sure we got back the same address we requested
		if ip.String() != addr.String() {
			// TODO(dperny) structure errors
			return fmt.Errorf("returned address %v did not match requested address %v", ip, addr)
		}
		// if we get this far, the address belongs to this pool. add to the
		// endpoints map for deallocation later and return nil, for no error
		local.endpoints[ip.String()] = poolID
	}
	// if we get all the way through this loop, without jumping to
	// the next iteration of the addresses loop, then we're in a
	// weird situation where the address is out of range for
	// _every_ pool on the network.
	// TODO(dperny) do we really want to rely on errors from a higher package?
	// probably not.
	return ipamapi.ErrIPOutOfRange
}

// AllocateNetwork allocates the IPAM pools for the given network. The network
// IPAM driver information and config must be allocated before calling
// AllocateNetwork, or this will fail. The provided network must not be nil.
func (a *Allocator) AllocateNetwork(n *api.Network) error {
	// TODO(dperny): implement
	local := &network{
		nw:        n,
		pools:     make(map[string]string),
		endpoints: make(map[string]string),
	}
	if local.nw.IPAM == nil {
	}
	return nil
}

// AllocateEndpoint allocates the VIPs for the provided endpoint and spec
func (a *Allocator) AllocateEndpoint(endpoint *api.Endpoint, spec *api.EndpointSpec) error {
	return nil
}

// AllocateAttachment allocates the provided NetworkAttachment belonging to a
// node or task
func (a *Allocator) AllocateAttachment(attachment *api.NetworkAttachment, spec *api.NetworkAttachmentConfig) error {
	return nil
}
