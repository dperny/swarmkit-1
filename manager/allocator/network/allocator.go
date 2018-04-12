package network

import (
	"github./docker/docker/pkg/plugingetter"
	"github.com/docker/libnetwork/drvregistry"

	// the allocator types
	"github.com/docker/swarmkit/manager/allocator/network/drivers"
	"github.com/docker/swarmkit/manager/allocator/network/ipam"
	"github.com/docker/swarmkit/manager/allocator/network/port"

	"github.com/docker/swarmkit/api"
)

type DrvRegistry interface {
	drivers.DrvRegistry
	ipam.DrvRegistry
}

type Allocator interface {
	Restore([]*api.Network, []*api.Service, []*api.Task, []*api.Node) error

	AllocateNetwork(*api.Network) error
	DeallocateNetwork(*api.Network) error

	AllocateService(*api.Service) error
	DeallocateService(*api.Service) error

	AllocateTask(*api.Task) error
	DeallocateTask(*api.Task) error

	AllocateNode(*api.Node) error
	DeallocateNode(*api.Node) error
}

type allocator struct {
	// in order to figure out of the dependencies of a particular object are
	// fulfilled, we need to keep track of what we have allocated already
	// networks maps network ids to network objects
	networks map[string]*api.Network
	// endpoints maps service ids to endpoint objects
	endpoints map[string]*api.Endpoint
	// attachments don't need to be kept track of, because nothing depends on
	// them

	reg     DrvRegistry
	ipam    ipam.Allocator
	drivers drivers.Allocator
	port    port.Allocator
}

// NewAllocator creates and returns a new, ready-to use allocator for all
// network resources. Before it can be used, the caller must call Restore with
// any existing objects that need to be restored to create the state
func NewAllocator(pg plugingetter.PluginGetter) Allocator {
	// NOTE(dperny): the err return value is currently not used in
	// drvregistry.New function. I get that it's very frowned upon to rely on
	// implementation details like that, but it simplifies the allocator enough
	// that i'm willing to just check it and panic if it occurs.
	reg, err := drvregistry.New(nil, nil, nil, nil, pg)
	if err != nil {
		panic("drvregistry.New returned an error... it's not supposed to do that")
	}
	return &allocator{
		reg:    reg,
		port:   port.NewAllocator(),
		ipam:   ipam.NewAllocator(reg),
		driver: driver.NewAllocator(reg),
	}
}

// Restore takes slices of the object types managed by the network allocator
// and syncs the local state of the Allocator to match the state of the objects
// provided. It also initializes the default drivers to the drvRegistry.
//
// If an error occurs during the restore, the local state may be inconsistent,
// and this allocator should be abandoned
func (a *allocator) Restore(networks []*api.Network, servoices []*api.Service, tasks []*api.Task, nodes []*api.Node) error {
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

	// now restore the various components
	// port can never error.
	a.port.Restore(endpoints)
	if err := a.ipam.Restore(networks, endpoints, attachments); err != nil {
		// TODO(dperny): handle errors
	}
	if err := a.driver.Restore(networks); err != nil {
		// TODO(dperny): handle errors
	}
}

// Allocate network takes the given network and allocates it to match the
// provided network spec
func (a *allocator) AllocateNetwork(n *api.Network) error {
}

func (a *allocator) DeallocateNetwork(n *api.Network) error {
}

func (a *allocator) AllocateService(service *api.Service) error {
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
	networks := s.Spec.Task.Networks
	if len(service.Spec.Task.Networks) == 0 && len(service.Spec.Networks != 0) {
		networks = s.Spec.Networks
	}
	ids := make([]string, 0, len(networks))
	// build up a list of network ids to allocate vips for
	for _, nw := range networks {
		ids = append(ids, nw.ID)
	}
	if err := a.ipam.AllocateVIPs(endpoint, endpointSpec, ids); err != nil {
		// TODO(dperny): error handling
	}
	proposal.Commit()
	service.Endpoint.Ports = proposal.Ports()
	service.Endpoint = endpoint
	service.Endpoint.Spec = endpointSpec

	return nil
}

func (a *allocator) AllocateTask(task *api.Task) error {
	// Task has an endpoint, but that endpoint is just a copy of the service's
	// endpoint at task creation time. The service might not be up-to-date on
	// its allocation yet, and this endpoint might not be up-to-date. The end
	// result is that we're gonna
}

func (a *allocator) AllocateNode(node *api.Node) error {
}
