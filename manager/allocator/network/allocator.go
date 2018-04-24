package network

import (
	"fmt"

	"github.com/docker/docker/pkg/plugingetter"
	"github.com/docker/libnetwork/drvregistry"

	// the allocator types
	"github.com/docker/swarmkit/manager/allocator/network/driver"
	"github.com/docker/swarmkit/manager/allocator/network/errors"
	"github.com/docker/swarmkit/manager/allocator/network/ipam"
	"github.com/docker/swarmkit/manager/allocator/network/port"

	"github.com/docker/swarmkit/api"
)

type DrvRegistry interface {
	driver.DrvRegistry
	ipam.DrvRegistry
}

type Allocator interface {
	Restore([]*api.Network, []*api.Service, []*api.Task) error

	AllocateNetwork(*api.Network) error
	DeallocateNetwork(*api.Network) error

	AllocateService(*api.Service) error
	DeallocateService(*api.Service) error

	AllocateTask(*api.Task) error
	DeallocateTask(*api.Task) error
}

type allocator struct {
	// in order to figure out if the dependencies of a task are fulfilled, we
	// need to keep track of what we have allocated already. this also allows
	// us to avoid having to pass an endpoint from the service when allocating
	// a task.
	services map[string]*api.Service
	// note that we don't need to keep track of networks; the lower-level
	// components handle networks directly and keep track of them as needed,
	// unlike services, which exist strictly at this level and above.

	// also attachments don't need to be kept track of, because nothing depends
	// on them.

	reg    DrvRegistry
	ipam   ipam.Allocator
	driver driver.Allocator
	port   port.Allocator
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
	// while we have access to a real DrvRegistry object, because this is the
	// only place we need it, let's init the drivers. If this fails, it means
	// the whole system is megascrewed
	for _, init := range initializers {
		if err := reg.AddDriver(init.ntype, init.fn, nil); err != nil {
			panic(fmt.Sprintf("reg.AddDriver returned an error: %v", err))
		}
	}

	// then, initialize the IPAM drivers
	if err := initIPAMDrivers(reg); err != nil {
		panic(fmt.Sprintf("initIPAMDrivers returned an error: %v", err))
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
// provided. It also initializes the default drivers to the reg.
//
// If an error occurs during the restore, the local state may be inconsistent,
// and this allocator should be abandoned
func (a *allocator) Restore(networks []*api.Network, services []*api.Service, tasks []*api.Task) error {
	endpoints := make([]*api.Endpoint, 0, len(services))
servicesLoop:
	for _, service := range services {
		// nothing to do if we have a nil endpoint
		if service.Endpoint == nil {
			continue servicesLoop
		}
		endpoints = append(endpoints, service.Endpoint)
		// this is kind of tricky... we need to figure out which service
		// endpoints are fully allocated or not here so we can add the fully
		// allocated ones to the services map. note that even if a service
		// isn't fully allocated, we still need to pass it to the Restore
		// methods, because we absolutely must have the entire state, fully
		// allocated or not, before we can pursue new allocations.
		if service.Spec.Endpoint != nil {
			// if the mode differs, the service isn't fully allocated
			if service.Endpoint.Spec.Mode != service.Spec.Endpoint.Mode {
				continue servicesLoop
			}
			// if we're using vips, check that we're using the right vips
			if service.Spec.Endpoint.Mode == api.ResolutionModeVirtualIP {
				if len(service.Endpoint.VirtualIPs) != len(service.Spec.Task.Networks) {
					continue servicesLoop
				}
				// i'm not totally happy with this part because I think it slightly
				// breaks the separation of concerns, but i can't think of a
				// particularly better way right now that's worth the effort.
			vipsLoop:
				for _, vip := range service.Endpoint.VirtualIPs {
					for _, nw := range service.Spec.Task.Networks {
						if nw != nil && nw.Target == vip.NetworkID {
							// if we find a target that matches this vip, then
							// we can go to the next VIP and check it
							continue vipsLoop
						}
					}
					// if we get all the way through the networks and there is
					// nothing matching this VIP, the service isn't fully
					// allocated
					continue servicesLoop
				}
			}
			// if we got this far, and the ports are also already allocated,
			// then the service is fully allocated and we can track it in our
			// map.
			if port.AlreadyAllocated(service.Endpoint, service.Spec.Endpoint) {
				a.services[service.ID] = service
			}
		}
	}

	attachments := []*api.NetworkAttachment{}
	// get all of the attachments out of tasks
	for _, task := range tasks {
		for _, attachment := range task.Networks {
			attachments = append(attachments, attachment)
		}
	}

	// now restore the various components
	// port can never error.
	a.port.Restore(endpoints)
	// errors from deeper components are always structured and can be returned
	// directly.
	if err := a.ipam.Restore(networks, endpoints, attachments); err != nil {
		return err
	}
	if err := a.driver.Restore(networks); err != nil {
		return err
	}
	return nil
}

// Allocate network takes the given network and allocates it to match the
// provided network spec
func (a *allocator) AllocateNetwork(n *api.Network) error {
	// first, figure out if the network is node-local, so we know whether or
	// not to run the IPAM allocator
	local, err := a.driver.IsNetworkNodeLocal(n)
	if err != nil {
		return err
	}
	if local {
		// if the network is already allocated and we try to call allocate
		// again, ipam.AllocateNetwork will return ErrAlreadyAllocated, so we
		// don't need to check that at this level
		if err := a.ipam.AllocateNetwork(n); err != nil {
			return err
		}
	}
	if err := a.driver.Allocate(n); err != nil {
		return err
	}
	return nil
}

func (a *allocator) DeallocateNetwork(n *api.Network) error {
	// we don't need to worry about whether or not the network is node-local
	// for deallocation because it won't have ipam data anyway
	if err := a.driver.Deallocate(n); err != nil {
		return err
	}
	a.ipam.DeallocateNetwork(n)
	return nil
}

func (a *allocator) AllocateService(service *api.Service) error {
	// first, check if we have already allocated this service. Do this by
	// checking the service map for the service. Then, if it exists, check if
	// the spec version is the same.
	//
	// we only update the services map entry with the newer service version if
	// allocation succeeds, so if the spec version hasn't changed, then the
	// service hasn't changed.
	if oldService, ok := a.services[service.ID]; ok {
		var oldVersion, newVersion uint64
		// we need to do this dumb dance because for some crazy reason
		// SpecVersion is nullable
		if oldService.SpecVersion != nil {
			oldVersion = oldService.SpecVersion.Index
		}
		if service.SpecVersion != nil {
			newVersion = service.SpecVersion.Index
		}
		if oldVersion == newVersion {
			return errors.ErrAlreadyAllocated()
		}
	}
	// handle the cases where service bits are nil
	endpoint := service.Endpoint
	if endpoint == nil {
		endpoint = &api.Endpoint{}
	}
	endpointSpec := service.Spec.Endpoint
	if endpointSpec == nil {
		endpointSpec = &api.EndpointSpec{}
	}
	proposal, err := a.port.Allocate(endpoint, service.Spec.Endpoint)
	if err != nil {
		return err
	}

	// TODO(dperny) this handles the case of spec.Networks, which we should
	// deprecate before removing this code entirely
	networks := service.Spec.Task.Networks
	if len(service.Spec.Task.Networks) == 0 && len(service.Spec.Networks) != 0 {
		networks = service.Spec.Networks
	}
	ids := make([]string, 0, len(networks))
	// build up a list of network ids to allocate vips for
	for _, nw := range networks {
		ids = append(ids, nw.Target)
	}
	if err := a.ipam.AllocateVIPs(endpoint, ids); err != nil {
		// if the error is a result of the service already being fully
		// allocated, then commit the port allocations and return the service
		if !errors.IsErrAlreadyAllocated(err) {
			return err
		}
		// however, if the ports are also already allocated, then the whole
		// service is known to be fully allocated, and we can return
		// ErrAlreadyAllocated
		if port.AlreadyAllocated(service.Endpoint, service.Spec.Endpoint) {
			return errors.ErrAlreadyAllocated()
		}
	}
	proposal.Commit()
	service.Endpoint.Ports = proposal.Ports()
	service.Endpoint = endpoint
	service.Endpoint.Spec = endpointSpec
	// save the service endpoint to the endpoints map
	a.services[service.ID] = service

	return nil
}

func (a *allocator) DeallocateService(service *api.Service) error {
	if service.Endpoint != nil {
		a.port.Deallocate(service.Endpoint)
		a.ipam.DeallocateVIPs(service.Endpoint)
	}
	return nil
}

func (a *allocator) AllocateTask(task *api.Task) error {
	// if the task state is past new, then it's already allocated
	if task.Status.State > api.TaskStateNew {
		return errors.ErrAlreadyAllocated()
	}
	// if the task has an empty service ID, it doesn't depend on the service
	// being allocated.
	if task.ServiceID != "" {
		service, ok := a.services[task.ServiceID]
		if !ok {
			return errors.ErrDependencyNotAllocated("service", task.ServiceID)
		}
		// set the task endpoint to match the service endpoint
		task.Endpoint = service.Endpoint
	}
	attachments, err := a.ipam.AllocateAttachments(task.Spec.Networks)
	if err != nil {
		return err
	}
	task.Networks = attachments
	return nil
}

func (a *allocator) DeallocateTask(task *api.Task) error {
	a.ipam.DeallocateAttachments(task.Networks)
	for _, attachment := range task.Networks {
		// remove the addresses after we've deallocated every attachment
		attachment.Addresses = nil
	}
	return nil
}
