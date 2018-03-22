package network

import (
	// testing packages
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"

	// standard libraries
	"context"

	// docker's libraries
	"github.com/docker/docker/pkg/plugingetter"

	// internal libraries
	"github.com/docker/swarmkit/api"
)

// stubPluginGetter is something we can pass to NetworkAllocator which fulfills
// the plugingetter interface without worrying about if the underlying
// components can correctly handle nil.
type stubPluginGetter struct{}

// Get implements plugingetter.PluginGetter.Get
func (stubPluginGetter) Get(_, _ string, _ int) (plugingetter.CompatPlugin, error) {
	return nil, errors.New("not implemented")
}

// GetAllByCap implements plugingetter.PluginGetter.GetAllByCap
func (stubPluginGetter) GetAllByCap(_ string) ([]plugingetter.CompatPlugin, error) {
	return nil, errors.New("not implemented")
}

// GetAllManagedPluginsByCap implements plugingetter.PluginGetter.GetAllManagedPluginsByCap
func (stubPluginGetter) GetAllManagedPluginsByCap(_ string) []plugingetter.CompatPlugin {
	return []CompatPlugin{}
}

// Handle implements plugingetter.PluginGetter.Handle
func (stubPluginGetter) Handle(_ string, _ func(string, *plugins.Client)) {
	// pass, do nothing. there, handled.
}

// TestNetworkAllocatorInitNoObjects tests that passing no objects to the Init
// method executes successfully, without errors, and does not alter the state
// of the network allocator in any unexpected way. This is the simplest test
// case, a smoke test which should
func TestNetworkAllocatorInitNoObjects(t *testing.T) {
	ctx := context.Background()

	// Create a new allocator and call "Init", capturing the error
	na := NewNetworkAllocator(stubPluginGetter{})
	err := na.Init(ctx, []*api.Network{}, []*api.Node{}, []*api.Service{}, []*api.Task{})
	// TODO(dperny): if there is an error, let's inspect the state of the
	// network allocator to understand better what has happened.
	assert.NoError(t, err, "error doing network allocator init")

	// check to make sure no networks exist. this includes ingress, which
	// doesn't get any special treatment at the NetworkAllocator level
	assert.Len(t, na.networks, 0)
}
