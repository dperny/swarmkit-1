package csi

import (
	"context"
	"errors"
	"sync"

	"github.com/docker/go-events"
	"github.com/sirupsen/logrus"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/log"
	"github.com/docker/swarmkit/manager/state/store"
)

type Manager struct {
	store *store.MemoryStore
	// provider is the SecretProvider which allows retrieving secrets. Used
	// when creating new Plugin objects.
	provider SecretProvider

	// newPlugin is a function which returns an object implementing the Plugin
	// interface. It allows us to swap out the implementation of plugins while
	// unit-testing the Manager
	newPlugin func(config *api.CSIConfig_Plugin, provider SecretProvider) Plugin

	// synchronization for starting and stopping the Manager
	startOnce sync.Once

	stopChan chan struct{}
	stopOnce sync.Once
	doneChan chan struct{}

	cluster *api.Cluster
	plugins map[string]Plugin

	pendingVolumes *volumeQueue
}

func NewManager(s *store.MemoryStore) *Manager {
	return &Manager{
		store:          s,
		stopChan:       make(chan struct{}),
		doneChan:       make(chan struct{}),
		newPlugin:      NewPlugin,
		plugins:        map[string]Plugin{},
		provider:       NewSecretProvider(s),
		pendingVolumes: newVolumeQueue(),
	}
}

// Run runs the manager. The provided context is used as the parent for all RPC
// calls made to the CSI plugins. Canceling this context will cancel those RPC
// calls by the nature of contexts, but this is not the preferred way to stop
// the Manager. Instead, Stop should be called, which cause all RPC calls to be
// canceled anyway. The context is also used to get the logging context for the
// Manager.
func (vm *Manager) Run(ctx context.Context) {
	vm.startOnce.Do(func() {
		vm.run(ctx)
	})
}

// run performs the actual meat of the run operation.
//
// the argument is called pctx because it's the parent context, from which we
// immediately resolve a new child context.
func (vm *Manager) run(pctx context.Context) {
	defer close(vm.doneChan)
	ctx, ctxCancel := context.WithCancel(
		log.WithModule(pctx, "csi/manager"),
	)
	defer ctxCancel()

	watch, cancel, err := store.ViewAndWatch(vm.store, func(tx store.ReadTx) error {
		cluster, err := store.FindClusters(tx, store.ByName(store.DefaultClusterName))
		if err != nil {
			return err
		}
		vm.cluster = cluster[0]
		return nil
	})
	if err != nil {
		// TODO(dperny): log message
		return
	}
	defer cancel()

	vm.init()

	// run a goroutine which periodically processes incoming volumes. the
	// handle function will trigger processing every time new events come in
	// by writing to the channel

	doneProc := make(chan struct{})
	go func() {
		for {
			id, attempt := vm.pendingVolumes.wait()
			// this case occurs when the stop method has been called on
			// pendingVolumes. stop is called on pendingVolumes when Stop is
			// called on the CSI manager.
			if id == "" && attempt == 0 {
				break
			}
			// TODO(dperny): we can launch some number of workers and process
			// more than one volume at a time, if desired.
			vm.processVolume(ctx, id, attempt)
		}

		// closing doneProc signals that this routine has exited, and allows
		// the main Run routine to exit.
		close(doneProc)
	}()

	// defer read from doneProc. doneProc is closed in the goroutine above,
	// and this defer will block until then. Because defers are executed as a
	// stack, this in turn blocks the final defer (closing doneChan) from
	// running. Ultimately, this prevents Stop from returning until the above
	// goroutine is closed.
	defer func() {
		<-doneProc
	}()

	for {
		select {
		case ev := <-watch:
			vm.handleEvent(ev)
		case <-vm.stopChan:
			vm.pendingVolumes.stop()
			return
		}
	}
}

// processVolumes encapuslates the logic for processing pending Volumes.
func (vm *Manager) processVolume(ctx context.Context, id string, attempt uint) {
	// set up log fields for a derrived context to pass to handleVolume.
	dctx := log.WithFields(ctx, logrus.Fields{
		"volume.id": id,
		"attempt":   attempt,
	})

	err := vm.handleVolume(dctx, id)
	// TODO(dperny): differentiate between retryable and non-retryable
	// errors.
	if err != nil {
		log.G(dctx).WithError(err).Info("error handling volume")
		vm.pendingVolumes.enqueue(id, attempt+1)
	}
}

// init does one-time setup work for the Manager, like creating all of
// the Plugins and initializing the local state of the component.
func (vm *Manager) init() {
	vm.updatePlugins()

	var nodes []*api.Node
	vm.store.View(func(tx store.ReadTx) {
		var err error
		nodes, err = store.FindNodes(tx, store.All)
		if err != nil {
			// TODO(dperny): log something
		}
	})

	for _, node := range nodes {
		vm.handleNode(node)
	}
}

func (vm *Manager) updatePlugins() {
	// activePlugins is a set of plugin names that are currently in the cluster
	// spec. this lets remove from the vm.plugins map any plugins that are
	// no longer in use.
	activePlugins := map[string]struct{}{}

	if vm.cluster != nil {
		for _, plugin := range vm.cluster.Spec.CSIConfig.Plugins {
			// it's exceedingly unlikely that plugin could ever be nil but
			// better this than segfault
			if plugin != nil {
				if _, ok := vm.plugins[plugin.Name]; !ok {
					vm.plugins[plugin.Name] = vm.newPlugin(plugin, vm.provider)
				}
				activePlugins[plugin.Name] = struct{}{}
			}
		}
	}

	// remove any plugins that are no longer in use.
	for pluginName := range vm.plugins {
		if _, ok := activePlugins[pluginName]; !ok {
			delete(vm.plugins, pluginName)
		}
	}
}

func (vm *Manager) Stop() {
	vm.stopOnce.Do(func() {
		close(vm.stopChan)
	})

	<-vm.doneChan
}

func (vm *Manager) handleEvent(ev events.Event) {
	// TODO(dperny): instead of executing directly off of the event stream, add
	// objects received to some kind of intermediate structure, so we can
	// easily retry.
	switch e := ev.(type) {
	case api.EventUpdateCluster:
		// TODO(dperny): verify that the Cluster in this event can never be nil
		vm.cluster = e.Cluster
		vm.updatePlugins()
	case api.EventCreateVolume:
		vm.enqueueVolume(e.Volume.ID)
	case api.EventUpdateVolume:
		vm.enqueueVolume(e.Volume.ID)
	case api.EventCreateNode:
		vm.handleNode(e.Node)
	case api.EventUpdateNode:
		// for updates, we're only adding the node to every plugin. if the node
		// no longer reports CSIInfo for a specific plugin, we will just leave
		// the stale data in the plugin. this should not have any adverse
		// effect, because the memory impact is small, and this operation
		// should not be frequent. this may change as the code for volumes
		// becomes more polished.
		vm.handleNode(e.Node)
	case api.EventDeleteNode:
		vm.handleNodeRemove(e.Node.ID)
	}
}

func (vm *Manager) createVolume(ctx context.Context, v *api.Volume) error {
	l := log.G(ctx).WithField("volume.id", v.ID).WithField("driver", v.Spec.Driver.Name)
	l.Info("creating volume")

	p, ok := vm.plugins[v.Spec.Driver.Name]
	if !ok {
		l.Errorf("volume creation failed: driver %s not found", v.Spec.Driver.Name)
		return errors.New("TODO")
	}

	info, err := p.CreateVolume(ctx, v)
	if err != nil {
		l.WithError(err).Error("volume create failed")
		return err
	}

	// TODO(dperny): handle error
	err = vm.store.Update(func(tx store.Tx) error {
		v2 := store.GetVolume(tx, v.ID)
		// TODO(dperny): handle missing volume
		if v2 == nil {
			return nil
		}

		v2.VolumeInfo = info

		return store.UpdateVolume(tx, v2)
	})
	if err != nil {
		l.WithError(err).Error("committing created volume to store failed")
	}
	return err
}

// enqueueVolume enqueues a new volume event, placing the Volume ID into
// pendingVolumes to be processed. Because enqueueVolume is only called in
// response to a new Volume update event, not for a retry, the retry number is
// always reset to 0.
func (vm *Manager) enqueueVolume(id string) {
	vm.pendingVolumes.enqueue(id, 0)
}

// handleVolume processes a Volume. It determines if any relevant update has
// occurred, and does the required work to handle that update if so.
//
// returns an error if handling the volume failed and needs to be retried.
func (vm *Manager) handleVolume(ctx context.Context, id string) error {
	var volume *api.Volume
	vm.store.View(func(tx store.ReadTx) {
		volume = store.GetVolume(tx, id)
	})
	if volume == nil {
		// if the volume no longer exists, there is nothing to do, nothing to
		// retry, and no relevant error.
		return nil
	}

	if volume.VolumeInfo == nil {
		return vm.createVolume(ctx, volume)
	}

	if volume.PendingDelete {
		return vm.deleteVolume(ctx, volume)
	}

	updated := false
	for _, status := range volume.PublishStatus {
		if status.State == api.VolumePublishStatus_PENDING_PUBLISH {
			plug := vm.plugins[volume.Spec.Driver.Name]
			publishContext, err := plug.PublishVolume(ctx, volume, status.NodeID)
			if err == nil {
				// TODO(dperny): handle error
				status.State = api.VolumePublishStatus_PUBLISHED
				status.PublishContext = publishContext
				updated = true
			}
		}
	}

	if updated {
		if err := vm.store.Update(func(tx store.Tx) error {
			// the publish status is now authoritative. read-update-write the
			// volume object.
			v := store.GetVolume(tx, volume.ID)
			if v == nil {
				// volume should never be deleted with pending publishes.
				// either handle this error otherwise document why we don't.
				return nil
			}

			v.PublishStatus = volume.PublishStatus
			return store.UpdateVolume(tx, v)
		}); err != nil {
			return err
		}
	}
	return nil
}

// handleNode handles one node event
func (vm *Manager) handleNode(n *api.Node) {
	if n.Description == nil {
		return
	}
	// we just call AddNode on every update. Because it's just a map
	// assignment, this is probably faster than checking if something changed.
	for _, info := range n.Description.CSIInfo {
		p, ok := vm.plugins[info.PluginName]
		if !ok {
			// TODO(dperny): log something
			continue
		}
		p.AddNode(n.ID, info.NodeID)
	}
}

// handleNodeRemove handles a node delete event
func (vm *Manager) handleNodeRemove(nodeID string) {
	// we just call RemoveNode on every plugin, because it's probably quicker
	// than checking if the node was using that plugin.
	for _, plugin := range vm.plugins {
		plugin.RemoveNode(nodeID)
	}
}

func (vm *Manager) deleteVolume(ctx context.Context, v *api.Volume) error {
	// TODO(dperny): handle missing plugin
	plug := vm.plugins[v.Spec.Driver.Name]
	err := plug.DeleteVolume(ctx, v)
	if err != nil {
		return err
	}

	// TODO(dperny): handle update error
	return vm.store.Update(func(tx store.Tx) error {
		return store.DeleteVolume(tx, v.ID)
	})
}
