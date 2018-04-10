package allocator

import (
	"context"
	"sync"
	"time"

	"github.com/docker/docker/pkg/plugingetter"

	"github.com/docker/swarmkit/manager/allocator/network"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/log"
	"github.com/docker/swarmkit/manager/state/store"
)

const (
	BatchSizeThreshold = 100
	BatchTimeThreshold = 100 * time.Millisecond
)

type Allocator struct {
	store   *store.MemoryStore
	network network.Allocator
}

func New(store *store.MemoryStore, pg plugingetter.PluginGetter) *Allocator {
	a := &Allocator{
		store:   store,
		network: network.NewAllocator(pg),
	}
}

func Run(ctx context.Context) error {
	ctx, c := context.WithCancel(ctx)
	// defer canceling the context, so that anything waiting on it will exit
	// when this routine exits.
	defer c()

	ctx = log.WithModule(ctx, "allocator")
	// Here's the approach we're taking in Run:
	//
	// Step 1: Get the object store, get all of the networky bits out, and call
	// Restore on the network allocator to bootstrap its state.
	//
	// Step 2: Put every object into the pending map, so that we'll go and
	// reconcile all of the existing objects first. We don't know if the state
	// of the objects we have currently matches the desired state of those,
	// objects and there may be outstanding work left from the previous
	// allocator
	//
	// Step 3: Start reading off of the event stream and allocating new objects
	// as the come in

	// pending maps: these maps keep track of the latest version of all objects
	// that are awaiting allocation. basically, if stuff is flying off the
	// event stream at a really high rate, these maps let us discard an older
	// version of an object if we haven't allocated it yet when a newer version
	// comes in
	pendingNetworks := map[string]struct{}{}
	pendingNetworkCond := sync.NewCond(&sync.Mutex{})
	pendingServices := map[string]struct{}{}
	pendingServicesCond := sync.NewCond(&sync.Mutex{})
	pendingTasks := map[string]struct{}{}
	pendingTasksCond := sync.NewCond(&sync.Mutex{})
	pendingNodes := map[string]struct{}{}
	pendingNodesCond := sync.NewCond(&sync.Mutex{})

	watch, cancel, err := store.ViewAndWatch(store,
		func(tx store.ReadTx) error {
			networks, err := store.FindNetworks(tx, store.All)
			if err != nil {
				// TODO(dperny): handle errors
			}
			services, err := store.FindService(tx, store.All)
			if err != nil {
				// TODO(dperny): handle errors
			}
			tasks, err := store.FindTasks(tx, store.All)
			if err != nil {
				// TODO(dperny): handle errors
			}
			endpoints := make([]*api.Endpoint, 0, len(services))
			for _, service := range services {
				if service.Endpoint != nil {
					endpoints = append(endpoints, service.Endpoint)
				}
			}
			attachments := []*api.NetworkAttachment{}
			for _, task := range tasks {
				for _, attach := range task.Networks {
					attachments = append(attachments, attach)
				}
			}
			for _, node := range nodes {
				for _, attach := range node.Attachments {
					attachments = append(attachments, attach)
				}
			}
			if err := a.network.Restore(networks, endpoints, attachments); err != nil {
				// TODO(dperny) error handling
			}
		},
		api.EventCreateNetwork{},
		api.EventUpdateNetwork{},
		api.EventDeleteNetwork{},
		api.EventCreateService{},
		api.EventUpdateService{},
		api.EventDeleteService{},
		api.EventCreateTask{},
		api.EventUpdateTask{},
		api.EventDeleteTask{},
		api.EventCreateNode{},
		api.EventUpdateNode{},
		api.EventDeleteNode{},
	)
	if err != nil {
		// TODO(dperny): error handling
		return err
	}

	// set up a routine to cancel the event stream when the context is canceled
	go func() {
		select {
		case <-ctx.Done():
			// cancel the event stream and wake all of the waiting goroutines
			cancel()
			pendingNetworksCond.Broadcast()
			pendingTasksCond.Broadcast()
			pendingServicesCond.Broadcast()
			pendingNodesCond.Broadcast()
		}
	}()

	// TODO(dperny): define what our signal is going to be be. batch size?
	// time?
	var batchSizeSignal <-chan struct{}
	// set up the batch processor
	go func() {
		batchTicker := time.NewTicker(BatchTimeThreshold)
		defer batchTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-batchTicker.C:
			case <-batchSizeSignal:
			}
		}
	}()

	// this function watches the event stream for incoming updates. It doesn't
	// keep the object around, though, because by the time we're ready to
	// service it, the object may have been updated again. instead, it does a
	// bare minimum of filtering, quickly grabs the object ID, and writes that
	// out to another channel.
	//
	// the exception is for deletions. when a deletion comes in, we don't have
	// any choice but to deallocated it. the deallocation is strictly a
	// bookkeeping step, for keeping internal state consistent.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-watch:
				switch ev := event.(type) {
				case api.EventCreateNetwork, api.EventUpdateNetwork:
					// get the network
					var n *api.Network
					if e, ok := ev.(api.EventCreateNetwork); ok {
						n = e.Network
					} else {
						n = ev.(api.EventUpdateNetwork).Network
					}
					if n != nil {
						pendingNetworkCond.L.Lock()
						pendingNetworks[n.ID] = struct{}{}
						pendingNetworkCond.Broadcast()
						pendingNetworksCond.L.Unlock()
					}
				case api.EventDeleteNetwork:
					// if the user deletes  the network, we don't have to do any
					// store actions, and we can't return any errors. The network
					// is already gone, deal with it
					if ev.Network != nil {
						a.network.DeallocateNetwork(ev.Network)
					}
				case api.EventCreateService, api.EventUpdateService:
					var s *api.Service
					if e, ok := ev.(api.EventCreateService); ok {
						s = e.Service
					} else {
						s = ev.(api.EventUpdateService).Service
					}
					if s != nil {
						pendingServicesCond.L.Lock()
						pendingServices[s.ID] = struct{}{}
						pendingServicesCond.Broadcast()
						pendingServicesCond.L.Unlock()
					}
				case api.EventDeleteService:
					if ev.Service != nil {
						a.network.DeallocateService(ev.Service)
					}
				case api.EventCreateTask, api.EventUpdateTask:
					var t *api.Task
					if e, ok := ev.(api.EventCreateTask); ok {
						t = e.Task
					} else {
						t = ev.(api.EventUpdateTask).Task
					}

					// bail out of it's a nil task
					if t == nil {
						continue
					}
					// if the task state is already past pending, or the task
					// desired state is a terminal state, then we can short circuit
					// this part.
					if t.Status.State >= api.TaskStatePending || t.DesiredState >= api.TaskStateCompleted {
						continue
					}
					pendingTasksCond.L.Lock()
					pendingTasks[t.ID] = struct{}{}
					pendingTasksCond.Broadcast()
					pendingTasksCond.L.Unlock()
				case api.EventDeleteTask:
					if ev.Task != nil {
						a.network.DeallocateTask(ev.Task)
					}
				case api.EventCreateNode, api.EventUpdateNode:
					var n *api.Node
					if e, ok := ev.(api.EventCreateNode); ok {
						n = e.Node
					} else {
						n = ev.(api.EventUpdateNode).Node
					}
					if n != nil {
						pendingNodesCond.L.Lock()
						pendingNodes[n.ID] = struct{}{}
						pendingNodesCond.Broadcast()
						pendingNodesCond.L.Unlock()
					}
				case api.EventDeleteNode:
					if ev.Node != nil {
						a.network.DeallocateNode(ev.Node)
					}
				}
			}
		}
	}()
}
