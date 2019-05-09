package network

import (
	"github.com/docker/libnetwork/ipamapi"
	builtinIpam "github.com/docker/libnetwork/ipams/builtin"
	nullIpam "github.com/docker/libnetwork/ipams/null"
	remoteIpam "github.com/docker/libnetwork/ipams/remote"
	"github.com/docker/libnetwork/ipamutils"

	"github.com/docker/swarmkit/log"
)

// initIPAMDrivers initializes the default IPAM drivers. If defaultPool is nil,
// compiled defaults will be used.
func initIPAMDrivers(r ipamapi.Callback, defaultPool []string, subnetSize uint32) error {
	// start by initializing the default IPAM configuration
	var addressPool []*ipamutils.NetworkToSplit
	// construct ipamutils.NetworkToSplit from the default address pool
	if defaultPool != nil {
		for _, p := range defaultPool {
			addressPool = append(addressPool, &ipamutils.NetworkToSplit{
				Base: p,
				Size: int(subnetSize),
			})
		}
		// we have to use log.L, the no-Context logger, because this function has
		// no access to a context. additionally, we're going to manually add a
		// module field that matches what we would otherwise expect, because that
		// assists in finding the right log messages during debugging. strictly
		// speaking, this isn't the "right" way to do this, but the "right" way
		// relies on a Context that we don't have, and these module values very
		// rarely change.
		//
		// Also, this log line is going to be LONG, but it's only printed once
		// on allocator init
		log.L.WithField(
			"module", "node/allocator",
		).Infof(
			"Swarm initializing global default address pool to: Subnets: %v, Size: %v",
			defaultPool, subnetSize,
		)
	}

	// now call ipamutils to configure the defaults. this works even if
	// addressPool is nil; in that case, it will use compiled defaults.
	if err := ipamutils.ConfigGlobalScopeDefaultNetworks(addressPool); err != nil {
		return err
	}

	for _, fn := range [](func(ipamapi.Callback, interface{}, interface{}) error){
		builtinIpam.Init,
		remoteIpam.Init,
		nullIpam.Init,
	} {
		if err := fn(r, nil, nil); err != nil {
			return err
		}
	}

	return nil
}
