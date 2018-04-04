package drivers

import (
	"github.com/docker/docker/pkg/plugingetter"
	"github.com/docker/libnetwork/driverapi"
	"github.com/docker/libnetwork/ipamapi"

	"github.com/docker/swarmkit/api"
)

// DrvRegistry is an interface implementing the subset of methods from
// DrvRegistry that we actually care about. It exists to make testing this
// Allocator easier, by breaking most of the dependency with the complexity of
// DrvRegistry
//
// DrvRegistry (the real one, from libnetwork) is a tool that manages internal
// and external drivers. 99% of its use to us is the fact that it keeps track
// of available drivers, and that's it. The remaining 1% is the "remote"
// driver. That particular kind of driver has an Init function that registers a
// handler with a PluginGetter, which in turn contains a callback to the
// drvRegistry, which, as far as I can tell, causes newly added plugins to be
// available in the drvRegistry. It does a bunch of other stuff too that we
// don't care about or use.
type DrvRegistry interface {
	Driver(name string) (driverapi.Driver, *driverapi.Capability)
	IPAM(name string) (ipamapi.Ipam, *ipamapi.Capability)
}

type Allocator struct {
	drvRegistry DrvRegistry
}

func NewAllocator(drvRegistry DrvRegistry) *Allocator {
	return &Allocator{
		drvRegistry: DrvRegistry,
	}
}

// Restore takes a list of networks, and restores the driver state for all of
// the networks that are already allocated.
func (a *Allocator) Restore(networks []*api.Network) error {
	for _, n := range networks {
		if n.DriverState != nil {
			n.DriverState.Name
		}
	}
}

func (a *Allocator) Allocate(n *api.Network) error {

}

func (a *Allocator) Deallocate(n *api.Network) error {

}

// GetIPAM gets the IPAM driver for the specified network.
func (a *Allocator) GetIPAM(nw *api.Network) (ipamapi.Ipam, error) {

}
