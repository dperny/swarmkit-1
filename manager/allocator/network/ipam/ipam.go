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

type Allocator interface {
	Restore([]*api.Network, []*api.Endpoint, []*api.NetworkAttachment) error
	AllocateNetwork(*api.Network) error
	DeallocatNetwork(*api.Network) error
	AllocateVIPs(*api.Endpoint, *api.EndpointSpec, []string) error
	DeallocateVIPs(*api.Endpoint)
	AllocateAttachment(*api.NetworkAttachment, *api.NetworkAttachmentConfig) error
	DeallocateAttachment(*api.NetworkAttachment)
}

// allocator is an allocator for IP addresses and IPAM pools. It handles all
// allocation and deallocation of IP addresses, both for VIPs and endpoints, in
// addition to handling IPAM pools for networks.
type allocator struct {
	// networks maps network IDs to the locally stored network object
	networks map[string]*network

	// drvRegistry is the driver registry, from which we can get active network
	// drivers
	drvRegistry DrvRegistry
}

func NewAllocator(reg DrvRegistry) Allocator {
	return &allocator{
		networks:    make(map[string]*network),
		drvRegistry: reg,
	}
}

// Restore restores the state of the provided networks to the Allocator. It can
// return errors if the initialization of its dependencies fails.
func (a *allocator) Restore(networks []*api.Network, endpoints []*api.Endpoint, attachments []*api.NetworkAttachment) error {
	// Initialize the state of the networks. this gets the initial IPAM pools.
	for _, nw := range networks {
		// if the network has no IPAM field, it has no state, and there is
		// nothing to do
		if nw.IPAM == nil ||
			// if the network has no IPAM driver, it has no IPAM state, and there
			// is nothing to do.
			nw.IPAM.Driver == nil ||
			// if the network has no ipam configs, then it has no state, and there
			// is nothing to do
			len(nw.IPAM.Configs) == 0 {
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
			return ErrInvalidIPAM{ipamName, nw.ID}
		}
		_, addressSpace, err := ipam.GetDefaultAddressSpaces()
		// the only errors here result from having an invalid ipam driver
		if err != nil {
			return ErrBustedIPAM{ipamName, nw.ID, err}
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
				return ErrBadState{
					local.nw.ID,
					fmt.Sprintf("error reserving ipam pool: %v", err),
				}
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
			// and now, if we have a Gateway address for this network, we need
			// to allocate it. This condition should never happen; a network
			// with a valid IPAM config but an empty gateway is a recipe for
			// disaster.  This is just included for completeness if an older
			// version has incorrect state
			if config.Gateway != "" {
				_, _, err := ipam.RequestAddress(poolID, net.ParseIP(config.Gateway), ipamOpts)
				if err != nil {
					return ErrBadState{
						local.nw.ID,
						fmt.Sprintf("error requesting already assigned gateway address %v", err),
					}
				}
				// NOTE(dperny): this check was originally here:
				// if gwIP.IP.String() != config.Gateway {
				//   // if we get an IP address from this that isn't the one we
				//   // requested, that's Very Bad.
				//   return ErrBadState{
				//     local.nw.ID,
				//     fmt.Sprintf("got back gateway ip %v, but requested ip %v", gwIP, config.Gateway),
				//   }
				// }
				// it has been removed because we don't need it. this is a
				// check that IPAM is behaving correctly. We don't need to
				// check that IPAM is behaving correctly. Doing so makes this
				// code less clear and harder to test. This has been left in
				// so that this bolt of wisdom is shared with future
				// contributors. A similar check was found in restoreAddress
				// in the analogous location.

				// most addresses need to be added to the map of
				// address -> poolid, but we don't need to do this with the
				// gateway address because it's store in the ipam config with
				// the subnet, which is the key for the pools map.
			}
			// delete the Gateway option from the IPAM options when we're done
			delete(ipamOpts, ipamapi.RequestAddressType)
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
			if vip != nil {
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
func (a *allocator) restoreAddress(nwid string, address string) error {
	// this check on address is shared by restoring VIPs and attachments.
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
		return ErrInvalidIPAM{local.nw.IPAM.Driver.Name, local.nw.ID}
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
			return ErrBadState{
				local.nw.ID,
				fmt.Sprintf(
					"error with driver %v requesting address %v: %v",
					local.nw.IPAM.Driver.Name,
					addr.String(),
					err,
				),
			}
		}
		// if we get this far, the address belongs to this pool. add to the
		// endpoints map for deallocation later and return nil, for no error
		local.endpoints[ip.String()] = poolID
		return nil
	}
	// if we get all the way through this loop, without jumping to
	// the next iteration of the addresses loop, then we're in a
	// weird situation where the address is out of range for
	// _every_ pool on the network.
	return ErrBadState{
		local.nw.ID,
		fmt.Sprintf("requested address %v is out of range", addr),
	}
}

// AllocateNetwork allocates the IPAM pools for the given network. The network
// must not be nil, and must not use a node-local driver
func (a *allocator) AllocateNetwork(n *api.Network) (rerr error) {
	// check if this network is already being managed and return an error if it
	// is. networks are immutable and cannot be updated.
	if _, ok := a.networks[n.ID]; ok {
		return ErrNetworkAllocated{n.ID}
	}

	// Now get the IPAM driver and options, either the defaults or the user's
	// specified options.
	ipamName := ipamapi.DefaultIPAM
	if n.Spec.IPAM != nil && n.Spec.IPAM.Driver != nil && n.Spec.IPAM.Driver.Name != "" {
		ipamName = n.Spec.IPAM.Driver.Name
	}
	// make sure here that ipamOpts is not nil so we don't need to nil check it
	// everywhere later
	ipamOpts := map[string]string{}
	if n.Spec.IPAM != nil && n.Spec.IPAM.Driver != nil && n.Spec.IPAM.Driver.Options != nil {
		ipamOpts = n.Spec.IPAM.Driver.Options
	}

	ipam, _ := a.drvRegistry.IPAM(ipamName)
	if ipam == nil {
		return ErrInvalidIPAM{ipamName, n.ID}
	}

	_, addressSpace, err := ipam.GetDefaultAddressSpaces()
	if err != nil {
		return ErrBustedIPAM{ipamName, n.ID, err}
	}

	// now create the local network state object
	local := &network{
		nw:        n,
		pools:     map[string]string{},
		endpoints: map[string]string{},
	}
	var ipamConfigs []*api.IPAMConfig
	if n.Spec.IPAM != nil && len(n.Spec.IPAM.Configs) != 0 {
		// if the user has specified any IPAM configs, we'll use those
		ipamConfigs = n.Spec.IPAM.Configs
	} else {
		// otherwise, we'll create a single default IPAM config.
		ipamConfigs = []*api.IPAMConfig{
			{
				Family: api.IPAMConfig_IPV4,
			},
		}
	}
	// make the slice to hold final IPAM configs
	finalConfigs := make([]*api.IPAMConfig, 0, len(ipamConfigs))
	// before we start allocating from ipam, set up this defer to roll back
	// allocation in the case that allocation of any particular pool fails
	defer func() {
		if rerr != nil {
			for _, config := range finalConfigs {
				// only free addresses that were actually allocated
				if ip := net.ParseIP(config.Gateway); ip != nil {
					if err := ipam.ReleaseAddress(local.pools[config.Subnet], ip); err != nil {
						rerr = ErrDoubleFault{rerr, err}
						return
					}
				}
			}
			for _, pool := range local.pools {
				if err := ipam.ReleasePool(pool); err != nil {
					rerr = ErrDoubleFault{rerr, err}
					return
				}
			}
		}
	}()

	// now go through all of the IPAM configs and allocate them. in the
	// process, copy those configs to the object's configs. we do copies in
	// order to avoid inadvertantly modifying the spec.
	//
	// NOTE(dperny): be careful with this! if this loop doesn't run (because
	// there were no items in ipamConfigs) then this will fail silently!
	for _, specConfig := range ipamConfigs {
		config := specConfig.Copy()
		// the last parameter of this is "v6 bool", but we don't support ipv6
		// so we just pass "false"
		poolID, poolIP, meta, err := ipam.RequestPool(addressSpace, config.Subnet, config.Range, ipamOpts, false)
		if err != nil {
			return ErrFailedPoolRequest{config.Subnet, config.Range, err}
		}
		local.pools[poolIP.String()] = poolID
		// The IPAM contract allows the IPAM driver to autonomously provide a
		// network gateway in response to the pool request.  But if the network
		// spec contains a gateway, we will allocate it irrespective of whether
		// the ipam driver returned one already.  If none of the above is true,
		// we need to allocate one now, and let the driver know this request is
		// for the network gateway.
		var (
			gwIP *net.IPNet
			ip   net.IP
		)

		if gws, ok := meta[netlabel.Gateway]; ok {
			if ip, gwIP, err = net.ParseCIDR(gws); err != nil {
				return ErrBustedIPAM{
					ipamName,
					n.ID,
					fmt.Errorf(
						"can't parse gateway address (%v) returned by the ipam driver: %v",
						gws, err,
					),
				}
			}
			gwIP.IP = ip
		}
		// add the option indicating that we're gonna request a gateway, and
		// remove it before we exit this function
		ipamOpts[ipamapi.RequestAddressType] = netlabel.Gateway
		defer delete(ipamOpts, ipamapi.RequestAddressType)
		if config.Gateway != "" || gwIP == nil {
			gwIP, _, err = ipam.RequestAddress(poolID, net.ParseIP(config.Gateway), ipamOpts)
			if err != nil {
				return ErrFailedAddressRequest{config.Gateway, err}
			}
		}
		if config.Subnet == "" {
			config.Subnet = poolIP.String()
		}
		if config.Gateway == "" {
			config.Gateway = gwIP.IP.String()
		}
		finalConfigs = append(finalConfigs, config)
	}

	// now that everythign has succeeded, add the fields to the network object
	n.IPAM = &api.IPAMOptions{
		Driver: &api.Driver{
			Name:    ipamName,
			Options: ipamOpts,
		},
	}
	n.IPAM.Configs = finalConfigs

	// finally, add this network to the set of allocated networks.
	a.networks[n.ID] = local
	return nil
}

func (a *allocator) DeallocateNetwork(network *api.Network) error {
	return nil
}

// AllocateVIPs allocates the VIPs for the provided endpoint and spec
func (a *allocator) AllocateVIPs(endpoint *api.Endpoint, spec *api.EndpointSpec, networkIDs []string) error {
	return nil
}

func (a *allocator) DeallocateVIPs(endpoint *api.Endpoint) {
}

// AllocateAttachment allocates the provided NetworkAttachment belonging to a
// node or task
func (a *allocator) AllocateAttachment(attachment *api.NetworkAttachment, spec *api.NetworkAttachmentConfig) error {
	return nil
}

func (a *allocator) DeallocateAttachment(attachment *api.NetworkAttachment) {

}
