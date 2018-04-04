package network

import (
	"github./docker/docker/pkg/plugingetter"
	"github.com/docker/libnetwork/drvregistry"

	// the allocator types
	"github.com/docker/swarmkit/manager/allocator/network/ip"
	"github.com/docker/swarmkit/manager/allocator/network/network"
	"github.com/docker/swarmkit/manager/allocator/network/port"

	"github.com/docker/swarmkit/api"
)

type Allocator struct {
	drvRegistry      *drvRegistry.DrvRegistry
	ipAllocator      *ip.Allocator
	networkAllocator *network.Allocator
	portAllocator    *port.Allocator
}

// NewAllocator creates and returns a new, ready-to use allocator for all
// network resources. Before it can be used, the caller must call Restore with
// any existing objects that need to be restored to create the state
func NewAllocator(pg plugingetter.PluginGetter) *Allocator {
	// NOTE(dperny): the err return value is currently not used in
	// drvregistry.New function. I get that it's very frowned upon to rely on
	// implementation details like that, but it simplifies the allocator enough
	// that i'm willing to just check it and panic if it occurs.
	reg, err := drvregistry.New(nil, nil, nil, nil, pg)
	if err != nil {
		panic("drvregistry.New returned an error... it's not supposed to do that")
	}
	return &Allocator{
		drvRegistry:   reg,
		portAllocator: port.NewAllocator(),
	}
}

// Restore takes slices of the object types managed by the network allocator
// and syncs the local state of the Allocator to match the state of the objects
// provided. It also initializes the default drivers to the drvRegistry.
//
// If an error occurs during the restore, the local state may be inconsistent,
// and this allocator should be abandoned
func (a *Allocator) Restore(networks []*api.Network, endpoints []*api.Endpoint, attachments []*api.NetworkAttachment) error {
	// first, initialize the default drivers. these are defined in the
	// driver_[platform].go files, and are platform specific.
	for _, init := range initializers {
		if err := a.drvRegistry.AddDriver(init.ntype, init.fn, nil); err != nil {
			// TODO(dperny) give this a concrete type
			return err
		}
	}

	// then, initialize the IPAM drivers
	if err := initIPAMDrivers(a.drvRegistry); err != nil {
		// TODO(dperny) give this a concrete type
		return err
	}

	// now, restore the various components
	a.portAllocator.Restore(endpoints)
}

// Allocate network takes the given network and allocates it to match the
// provided network spec
func (a *Allocator) AllocateNetwork(n *api.Network) error {
	a.networkAllocator.Allocate(n)
}

func (a *Allocator) DeallocateNetwork(n *api.Network) error {

}

func (a *Allocator) AllocateService(service *api.Service) error {
	// handle the cases where service bits are nil
	endpoint := service.Endpoint
	if endpoint == nil {
		endpoint = &api.Endpoint{}
	}
	endpointSpec := service.Spec.Endpoint
	if endpointSpec == nil {
		endpointSpec = &api.EndpointSpec{}
	}
	proposal, err := a.portAllocator.Allocate(endpoint, spec)
	if err != nil {
		// TODO(dperny) structure this error
		return err
	}

	// TODO(dperny) this handles the case of spec.Networks, which we should
	// deprecate before removing this code entirely
	var networks []*api.NetworkAttachmentConfig
	if len(service.Spec.Task.Networks) == 0 && len(service.Spec.Networks != 0) {
		networks = s.Spec.Networks
	}
}

func (a *Allocator) AllocateTask(task *api.Task) error {
}

func (a *Allocator) AllocateNode(node *api.Node) error {
}
