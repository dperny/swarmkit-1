package network

import (
	// standard libraries
	"context"
	"fmt"
	"net"
	"sync"

	// external libraries
	"github.com/pkg/errors"

	// docker's libraries
	"github.com/docker/docker/pkg/plugingetter"
	"github.com/docker/go-events"

	// libnetwork-specific imports
	"github.com/docker/libnetwork/datastore"
	"github.com/docker/libnetwork/driverapi"
	"github.com/docker/libnetwork/drvregistry"
	"github.com/docker/libnetwork/ipamapi"
	builtinIpam "github.com/docker/libnetwork/ipams/builtin"
	nullIpam "github.com/docker/libnetwork/ipams/null"
	remoteIpam "github.com/docker/libnetwork/ipams/remote"
	"github.com/docker/libnetwork/netlabel"

	// internal libraries
	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/log"
	// TODO(dperny): clean up networkallocator
)

const DefaultDriver = "overylay"

// NetworkAllocator is the subcomponent in charge of allocating network
// resources for swarmkit resources. Give it objects, and it gives you
// resources.
//
// NetworkAllocator provides, essentially, an interface between swarmkit
// objects and the underlying libnetwork primatives. It carries a lot of local
// state by consequence of the fact that the underlying primatives are
// local-only and need to be initialized each time they are used.
//
// In swarmkit you'll see two kinds of objects: components with an event loop,
// and components which are strictly accessed through method calls. This is the
// latter.
//
// NetworkAlloctor doesn't have direct access to the raft store. The caller
// will provide all of the objects, and be in charge of committing those
// objects to the raft store. Instead, it works directly on the api objects,
// and gives them back to the caller to figure out how to store. This frees us
// from having to worry about the overlying storage method, and reduces the
// already rather bloated surface are of this component
//
// NetworkAllocator fully owns a few fields, and should be the _only_ component
// that ever writes to them. Conversely, these are the _only_ fields that the
// NetworkAllocator ever writes to, meaning it can safely work on an object
// concurrently with other routines, (as long as they don't try to read these
// fields) and the fields it owns can be merged into objects safely. These
// fields are:
//
// - api.Network:
//   - DriverState
//   - IPAM
// - api.Node:
//   - Attachment
//   - Attachments
// - api.Service
//   - Endpoint
// - api.Task
//   - Networks
//   - Endpoint
//
// These are essentially all of the objects of types:
// - api.Driver
// - api.IPAMOptions
// - api.NetworkAttachment
// - api.Endpoint
//
// In addition, these objects are treated internally as "atomic". They're
// either allcoated or not. A higher-level object like a Service or Task might
// be partially allocated, with only some of the NetworkAttachments or
// Endpoints matching the desired state, but each individual NetworkAttachment
// and Endpoint will either be completely allocated or not allocated at all.
// Treating these objects this way avoids the difficult problem of handling a
// bunch of partial-allocation edge-cases.
type NetworkAllocator struct {
	// store is the memory store of cluster
	// store *store.MemoryStore
	// actually, we don't need store. we shouldn't use store. initialization
	// should be provided by the caller giving us a list of all of the object
	// types we handle, and run-time should be accomplished through an event
	// channel. we should return the objects we've allocated to the caller
	// through a channel of our own

	// We need to keep a local copy of the plugingetter so that we can
	// instantiate the driver registry in Init instead of in New, which lets us
	// avoid having the constructor return errors.
	pg          plugingetter.PluginGetter
	drvRegistry *drvregistry.DrvRegistry

	// networks keeps track of the local state of networks we're currently
	// servicing. because IPAM state is totally local, every run of the
	// allocator is going to produce a new set of local ipam ids.
	// it is a map of network IDs to localNetworkState objects
	networks map[string]*localNetworkState
	// networksMutex protects concurrent access to the networks map
	networksMutex sync.RWMutex
}

type localNetworkState struct {
	// TODO(dperny): how much overhead is a mutex, and how important is it here
	// this mutex protects access to the local network state object
	sync.Mutex

	// network is a local cache of the store object.
	network *api.Network

	// pools is used to save the internal poolIDs needed when releasing the
	// pool. It maps an ip address to a pool ID.
	pools map[string]string

	// endpoints is a map of endpoint IP to the poolID from which it
	// was allocated.
	endpoints map[string]string

	// isNodeLocal indicates whether the scope of the network's resources
	// is local to the node. If true, it means the resources can only be
	// allocated locally by the node where the network will be deployed.
	// In this the swarm manager will skip the allocations.
	isNodeLocal bool
}

// NewNetworkAllocator returns the msot minimal NetworkAllocator object,
// otherwise uninitialized. Because initialization might be a weighty action,
// it's handled separately from the object creation. In addition, running
// initialization in a separate step lets us avoid returning errors from this
// constructor
func NewNetworkAllocator(pg plugingetter.PluginGetter) *NetworkAllocator {
	return &NetworkAllocator{
		startChan: make(chan struct{}),
		stopChan:  make(chan struct{}),
		doneChan:  make(chan struct{}),
		networks:  make(map[string]*localNetworkState),
		pg:        pg,
	}
}

// Init populates the local allocator state from the provided state, and
// unblocks the run loop when it has completed successfully
//
// The network allocator is one of the most complicated components of
// swarmkit, because it keeps a TON of local state that must be initialized
// before it will work correctly. For example, before the IPAM can be trusted
// to give out new addresses, we have to make sure that every address already
// assigned in the raft store is allocated in the local IPAM driver.
//
// This function is a minefield. Screwing up that initial state allocation will
// leave the whole allocator in an inconsistent state, essentially forever,
// because incorrect values will be checked into raft with no opportunity to
// recover. In order to avoid some of those problems, we follow some rules in
// this function to avoid acquiring new state before we've initialized the
// existing state:
//
//   1. All api objects are read-only. Never write to any objects, under any
//      circumstance. If you're writing to an object, that's a sign you may be
//      doing some new allocation
//   2. Never look at the spec. With a few exceptions, most of the fields in
//      spec are reflected in the object. the spec is the _desired_ state, and
//      the object is the _actual_ state. In Init, we only care about _actual_
//      state. If you're reading out of the Spec, it's a sign you might be
//      doing some allocation
//
// In a more civilized language, we might enforce these rules with type
// constraints, but this is Go, where we don't have that luxury. You've been
// warned.
//
// TODO(dperny): Error handling in this function is... tricky. Like,
// essentially, errors should only be returned if something is messed up in a
// way that we can literally do nothing other than crash over. However, it's
// possible that errors might occur because of some bad state, possibly
// inherited from the old allocator, or because of new bugs in this revision.
// That would cause us to get into a permastuck situation, where the allocator
// just crashes every time. We need to figure out how to correctly handle every
// possible error that could be returned.
func (na *NetworkAllocator) Init(ctx context.Context, networks []*api.Network, nodes []*api.Node, services []*api.Service, tasks []*api.Task) error {
	ctx = log.WithField(ctx, "method", "(*NetworkAllocator).Init")
	log.G(ctx).Debug("initializing network allocator")

	// initialize the drivers
	log.G(ctx).Debug("initializing drivers")
	// There are no driver configurations and notification
	// functions as of now.
	reg, err := drvregistry.New(nil, nil, nil, nil, na.pg)
	if err != nil {
		return errors.Wrap(err, "unable to initialize drvRegistry")
	}
	na.drvRegistry = reg

	// initializers is a list of driver initializers specific to the particular
	// platform we're on. the exact list can be found in the
	// drivers_<platform>.go source files.
	for _, i := range initializers {
		if err := na.DrvRegistry.AddDriver(i.ntype, i.fn, nil); err != nil {
			return errors.Wrapf(err, "unable to initialize driver %v", i.ntype)
		}
	}

	// Initialize the ipam drivers. Not sure what the difference between these
	// is
	if err := builtinIpam.Init(na.drvRegistry, nil, nil); err != nil {
		return errors.Wrap(err, "unable to initalize built in ipam driver")
	}
	if err := remoteIpam.Init(na.drvRegistry, nil, nil); err != nil {
		return errors.Wrap(err, "unable to initalize remote ipam driver")
	}
	if err := nullIpam.Init(na.drvRegistry, nil, nil); err != nil {
		return errors.Wrap(err, "unable to initalize null ipam driver")
	}

	log.G(ctx).Debug("initializing local state")
	// TODO(dperny) i'm not sure how concurrency-safe the underlying components
	// here are, but if they are we should do initialization of local state
	// concurrently for performance reasons
	log.G(ctx).Debug("initializing state for networks")
	for _, network := range networks {
		ctx := log.WithField("network.id", network.ID)
		// we don't need to lock local here because we know we're the only
		// ones with a handle on it
		local := &localNetworkState{
			network: network,
			pools:   make(map[string]string),
		}
		name := getDriverName(local.network)
		d, caps, err := na.resolveDriver(name)
		if err != nil {
			return errors.Wrapf(err, "error initializing network: %s", local.network.ID)
		}

		if caps.DataScope == datastore.LocalScope {
			// No swarm-level allocation can be provided by the network driver
			// for node-local networks. Only thing needed is populating the
			// driver's name in the driver's state. However, populating the
			// driver's name in the driver's state is an operation that alters
			// the cluster-wide network object, which we do not do during
			// initialization. If this network hasn't been handled yet, we'll
			// make this change during the main run loop.
			local.isNodeLocal = true
			continue
		}

		// now we need to initialize the IPAM pools
		log.G(ctx).Debug("initializing ipam pools")

		// first, get the IPAM driver
		ipamName, ipam, ipamOpts, err := na.resolveIPAM(local.network)
		if err != nil {
			return errors.Wrapf(err, "error initializing network: %v", local.network.ID)
		}

		// We don't support user defined address spaces yet so just
		// retrieve default address space names for the driver.
		_, asName, err := na.drvRegistry.IPAMDefaultAddressSpaces(ipamName)
		if err != nil {
			return errors.Errorf(err, "error retrieving default IPAM address spaces for network %s with ipam driver %s", network.ID, ipamName)
		}

		// if the network has an IPAM field, go through its configs and request
		// ipam pools. in addition, collect ipv4 data to use to initialize
		// local driver state.
		if local.network.IPAM != nil {
			ipv4Data := make([]driveapi.IPAMData, 0, len(local.network.IPAM.Configs))
			for _, ic := range local.network.IPAM.Configs {
				log.G(ctx).Debug("requesting pool for subnet %v", ic.Subnet)
				poolID, poolIP, meta, err := ipam.RequestPool(asName, ic.Subnet, ic.Range, dOptions, false)
				if err != nil {
					// TODO(dperny): in the original code, we releasePools on
					// failure, but maybe the better option is to just return
					// an error because the allocator is failing to start with
					// what should be a known-good config
				}
				local.pools[poolip.String()] = poolID
				// The IPAM contract allows the IPAM driver to autonomously
				// provide a network gateway in response to the pool request.
				// But if the network spec contains a gateway, we will allocate
				// it irrespective of whether the ipam driver returned one
				// already.  If none of the above is true, we need to allocate
				// one now, and let the driver know this request is for the
				// network gateway.
				// TODO(dperny) reword the above, it's unclear
				// The IPAM config likely already has a gateway allocated. To
				// ensure correctness of the IPAM state before we start
				// allocating new state, allocate it here. However, if, for
				// whatever reason, the ipam config does not have a gateway,
				// definitely do not allocate
				if dOptions == nil {
					dOptions = make(map[string]string)
				}
				// Set the ipam allocation method to serial
				dOptions[ipam.RequestAddressType] = netlabel.Gateway
				if _, ok := dOptions[ipamapi.AllocSerialPrefix]; !ok {
					dOptions[ipamapi.AllocSerialPrefix] = "true"
				}
				if ic.Gateway != "" {
					gwip := net.ParseIP(ic.Gateway)
					gateway, _, err := ipam.RequestAddress(poolID, net.ParseIP(ic.Gateway), dOptions)
					// verify that the IP address we've allocated is the same
					// one we request. this is a really dumb dead basic sanity
					// check. if we allocate an ip that isn't the one already
					// assigned, we've messed up bigtime
					if err != nil {
						// TODO(dperny): still haven't figured out the error
						// handling story here
					}
					if !gateway.IP.Equal(gwip) {
						log.G(ctx).Error("assigned gateway IP %s does not match requested IP %s", ip.IP, gwip)
					}
				}
				// remove the RequestAddressType configuration option so that
				// we haven't altered some state.
				// TODO(dperny): determine why this has side effects, why we
				// need to delete this option, and document it in a comment
				// here
				delete(dOptions, ipamapi.RequestAddressType)

				// before we go initializing driver state, if we have IPv6
				// data, skip to the next config.
				if ic.Family == api.IPAMConfig_IPV6 {
					continue
				}
				_, subnet, err := net.ParseCIDR(ic.Subnet)
				if err != nil {
					// TODO(dperny) be careful about returning errors...
					return errors.Wrapf(err, "error parsing subnet %s while allocating driver state", ic.Subnet)
				}
				ipv4data = append(ipv4data, driverapi.IPAMData{
					Pool:    subnet,
					Gateway: gateway,
				})
			}
			log.G(ctx).Debug("initializing driver state")
			// if the network has no driver state currently, we should not
			// perform any allocation now. we're still initializing local state
			if local.network.DriverState != nil {
				// TODO(dperny): is the driver state returned by
				// NetworkAllocate always the same after a leadership change?
				ds, err := d.driver.NetworkAllocate(n.ID, n.DriverState.Options, ipv4data, nil)
				if err != nil {
					// TODO(dperny): more error handling
				}
				if ds != local.network.DriverState.Options {
					// This should never happen, but log if it does. Sanity
					// check.
					log.G(ctx).Errorf(
						"driver state recieved in Init does not match driver state on network object: acquired: %v actual: %v",
						ds,
						local.network.DriverState.Options,
					)
				}
			}
			na.networksMutex.Lock()
			// finally, add the network to our local network state map
			na.networks[local.network.ID] = local
			na.networksMutex.Unlock()
		}
	}
	log.G(ctx).Debug("initializing state for nodes")
	// these are called "attachments" in the old allocator
	for _, node := range nodes {
		// shadow ctx to make a new context just for this loop
		ctx := log.WithField(ctx, "node.id", node.ID)
		for _, attachment := range node.Attachments {
			// we can trust initNetworkAttachment to perform no new
			// allocations, so we don't have to trust that each attachment is
			// allocated. anything unallocated will be ignored.
			if err := na.initNetworkAttachment(attachment); err != nil {
				// TODO(dperny) more error handling...
				log.G(ctx).WithError(err).Error("error initializing network attachment for node")
			}
		}
	}
	log.G(ctx).Debug("initializing state for services")
	for _, service := range services {
	}
	log.G(ctx).Debug("initializing state for tasks")
	for _, task := range tasks {
	}

	return nil
}

// initNetworkAttachment initializes the NetworkAllocator's local state with
// the information in a single network attachment. If the provided attachment
// is not yet assigned any addresses, we ignore it and do NOT allocate any new.
func (na *NetworkAllocator) initNetworkAttachment(attachment *api.NetworkAttachment) error {
	var ip *net.IPNet
	var opts map[string]string

	// first, check if the attachment has addresses. If not, then it's not
	// allocated yet, so we should do nothing. this is NOT an error, because
	// the purpose of this function is to init local state, and we can ignore
	// anything that's not initializing
	if len(attachment.Addresses) == 0 {
		return nil
	}

	// TODO(dperny): we only look at the attachment Network and Addresses
	// fields... what purpose to the Aliases and DriverAttachmentOpts fields
	// serve?
	ipam, _, _, err := na.resolveIPAM(attachment.Network)
	if err != nil {
		return errors.Wrap("failed to resolve IPAM while initializing")
	}
	local, ok := na.getNetwork(attachment.Network.ID)
	// if we don't get a network back, return an error. we can't allocate for a
	// network that doesn't exist.
	if !ok {
		return errors.Errorf("local state for network %v cannot be found", attachment.Network.ID)
	}

	// lock the local network state
	// TODO(dperny): a RWMutex would be ideal, but we'd have to unlock to
	// acquire a write lock on local to add to the endpoints map.
	local.Lock()
	defer local.Unlock()
	for _, rawAdder := range attachment.Addresses {
		if rawAddr == "" {
			// TODO(dperny): we should never have a partially allocated
			// attachment, so having a emptystring for a rawAddr is... not
			// good. we should do something here. but for now, just skip it.
			continue
		}
		// we're gonna try parsing the existing address two different ways here
		// once as CIDR, and then as an IP
		addr, _, err := net.ParseCIDR(rawAddr)
		if err != nil {
			addr = net.ParseIP(rawAddr)
			if addr == nil {
				// an unparseable address string shouldn't be possible, but
				// this is computers, where unfortunately, the impossible
				// happens every day
				return errors.Wrapf(err, "could not parse address string %v", rawAddr)
			}
		}
		if local.network.IPAM != nil && local.network.IPAM.Driver != nil {
			// we don't need to set IPAM serial allocation here because we're
			// not allocating anything new. but we should grab the IPAM options
			// if they exist, to make sure the same address gets correctly
			// allocated
			opts = local.network.IPAM.Driver.Options
		}
		for _, poolID := range local.pools {
			ip, _, err := ipam.RequestAddress(poolID, addr, opts)
			if err != nil {
				return errors.Wrap(err, "could not allocate IP from IPAM")
			}
			// make sure that the IP we just got matches the one we wanted
			if !ip.Equal(addr) {
				return errors.New("obtained address %v does not match requested address %v", ip, addr)
			}
			local.endpoints[ip.String()] = poolID
		}
	}
	return nil
}

// allocNetworkAttachment performs new allocation for the given network
// attachment, filling in its fields with the allocated addresses. It returns
// an error if allocation fails and rolls back any allocation that succeeded
func allocNetworkAttachment(attachment *api.NetworkAttachment) error {
}

// getNetwork provides a concurrency-safe map access to the localNetworkState
// object for the specified ID.
func (na *NetworkAllocator) getNetwork(id string) (*localNetworkState, bool) {
	na.networksMutex.RLock()
	defer na.networksMutex.RUnlock()
	return na.networks[id]
}

// resolveDriver returns the information for a driver for the provided name
// it returns the driver and the driver capabailities, or an error if
// resolution failed
func (na *NetworkAllocator) resolveDriver(name string) (driverapi.Driver, *driverapi.Capability, error) {
	d, drvcap := na.drvRegistry.Driver(dName)
	// if the network driver is nil, it must be a plugin
	if d == nil {
		pg := na.drvRegistry.GetPluginGetter()
		if pg == nil {
			return nil, nil, errors.Errorf("error getting driver %v: plugin store is uninitialized", dName)
		}

		_, err := pg.Get(name, driverapi.NetworkPluginEndpointType, plugingetter.Lookup)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "error getting driver %v from plugin store", name)
		}

		// NOTE(dperny): I don't understand why calling Driver a second time
		// is any different. what about the (PluginGetter).Get method is special?
		// why does it have side effects? i figured initially that this method
		// call was just to see if a plugin existed or whatever, but this must
		// have side effects
		d, drvcap = na.drvRegistry.Driver(name)
		if d == nil {
			return nil, errors.Errorf("could not resolve network driver %v", name)
		}
	}
	return d, drvcap, nil
}

// resolveIPAM retrieves the IPAM driver and options for the network provided.
// it returns the IPAM driver name, the IPAM driver, and the IPAM driver
// options, or an error if resolving the IPAM driver fails, in that order.
func (na *NetworkAllocator) resolveIPAM(n *api.Network) (string, ipamapi.Ipam, map[string]string, error) {
	// set default name and driver options
	dName := ipamapi.DefaultIPAM
	dOptions := map[string]string{}

	// check if the IPAM driver name and options are populated in the network
	// spec. if so, use them instead of the defaults
	if n.Spec.IPAM != nil && n.Spec.IPAM.Driver != nil {
		if n.Spec.IPAM.Driver.Name != "" {
			dName = n.Spec.IPAM.Driver.Name
		}
		if len(n.Spec.IPAM.Driver.Options) != 0 {
			dOptions = n.Spec.IPAM.Driver.Options
		}
	}

	// now get the IPAM driver from the driver registry. if it doesn't exist,
	// return an error
	ipam, _ := na.drvRegistry.IPAM(dName)
	if ipam == nil {
		return nil, "", nil, fmt.Errorf("could not resolve IPAM driver %s", dName)
	}

	return dName, ipam, dOptions, nil
}

// getDriverName returns the name of the driver for a given network, which is
// either specified in its DriverConfig, or is assumed to be DefaultDriver
//
// NOTE(dperny) in a previous version of this code, getDriverName was performed
// at the top of the resolveDriver method. however, this left the resolveDriver
// method returning, essentially, 4 values simultaneously. to clean up that
// function signature, i've moved getting the name to a different step
func getDriverName(n *api.Network) string {
	if n.Spec.DriverConfig != nil && n.Spec.DriverConfig.Name != "" {
		return n.Spec.DriverConfig.Name
	}
	return DefaultDriver
}
