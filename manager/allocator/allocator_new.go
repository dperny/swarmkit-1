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

type apiobj int

const (
	_ apiobj = iota
	networkObj
	serviceObj
	taskObj
	nodeObj
)

type pendingAllocation struct {
	obj apiobj
	id  string
}

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
	// General overview of how this function works:
	//
	// Run is a shim between the asynchronous store interface, and the
	// synchronous allocator interface. It uses a map to keep track of which
	// objects have outstanding allocations to perform, and uses a goroutine to
	// synchronize reads and writes with this map and allow it to function as a
	// a source of work.
	//
	// The first thing it does is read the object store and pass all of the
	// objects currently available to the network allocator. The network
	// allocator's restore function will add all allocated objects to the local
	// state so we can proceed with new allocations.
	//
	// It thens adds all objects in the store to the working set, so that any
	// objects currently in the store that aren't allocated can be.
	//
	// Then, it starts up two major goroutines:
	//
	// The first is the goroutine that gets object ids out of the work pile and
	// performs allocation on them. If the allocation succeeds, it writes that
	// allocation to raft. If it doesn't succeed, the object is added back to
	// the work pile to be serviced later
	//
	// The second is the goroutine that services events off the event queue. It
	// reads incoming store events and grabs just the ID and object type, and
	// adds that to the work pile. We only deal with the ID, not the full
	// object because the full object may have changed since the event came in
	// The exception in this event loop is for deallocations. When an object is
	// deleted, the event we recieve is our last chance to deal with that
	// object. In that case, we immediately call into Deallocate.

	ctx, c := context.WithCancel(ctx)
	// defer canceling the context, so that anything waiting on it will exit
	// when this routine exits.
	defer c()

	// pending allocations
	pendingAllocationsIn, pendingAllocationsOut := workQueue(ctx)

	ctx = log.WithModule(ctx, "allocator")
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
			nodes, err := store.FindNodes(tx, store.All)
			if err != nil {
				// TODO(dperny): handle errors
			}

			if err := a.network.Restore(networks, services, tasks, nodes); err != nil {
				// TODO(dperny): handle errors
			}
			for _, network := range networks {
				pendingAllocationsIn <- pendingAllocation{networkObj, network.ID}
			}
			endpoints := make([]*api.Endpoint, 0, len(services))
			for _, service := range services {
				pendingAllocationsIn <- pendingAllocation{serviceObj, service.ID}
			}
			attachments := []*api.NetworkAttachment{}
			for _, task := range tasks {
				pendingAllocationsIn <- pendingAllocation{taskObj, task.ID}
			}
			for _, node := range nodes {
				pendingAllocationsIn <- pendingAllocation{nodeObj, node.ID}
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
		}
	}()

	// this goroutine handles incoming work.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case pending := <-pendingAllocationsOut:
				// TODO(dperny): what happens if the raft write fails??? we
				// need to roll back an allocation?
				if err := store.Update(func(tx store.Tx) {
					switch pending.obj {
					case networkObj:
						n := store.GetNetwork(tx, pending.id)
						if n == nil {
							return nil
						}
						if err := a.network.AllocateNetwork(n); err != nil {
							// TODO(dperny): better error handling
							return err
						}
						return store.UpdateNetwork(tx, n)
					case serviceObj:
						s := store.GetService(tx, pending.id)
						if s == nil {
							return nil
						}
						if err := a.network.AllocateService(s); err != nil {
							return err
						}
						return store.UpdateService(tx, s)
					case taskObj:
						t := store.GetTask(tx, pending.id)
						if t == nil {
							return nil
						}
						if err := a.network.AllocateTask(t); err != nil {
							return err
						}
						return store.UpdateTask(tx, s)
					case nodeObj:
						n := store.GetNode(tx, pending.id)
						if n == nil {
							return nil
						}
						if err := a.network.AllocateNode(n); err != nil {
							return err
						}
						return store.UpdateNode(tx, n)
					}
				}); err != nil {
					// don't block on waiting for pendingAllocations to recieve
					// this allocation
					select {
					case <-ctx.Done():
						return
					case pendingAllocationsIn <- pending:
					}
				}
			}
		}
	}()

	// this goroutine handles incoming store events. all we need from the
	// events is the ID of what has been updated. by the time we service the
	// allocation, the object may have changed, so we don't save any other
	// information. we'll get to it later.
	go func() {
		for {
			var pending pendingAllocation
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
						pending = pendingAllocation{networkObj, n.ID}
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
						pending = pendingAllocation{serviceObj, s.ID}
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
					if t != nil {
						pending = pendingAllocation{taskObj, t.ID}
					}
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
						pending = pendingAllocation{nodeObj, n.ID}
					}
				case api.EventDeleteNode:
					if ev.Node != nil {
						a.network.DeallocateNode(ev.Node)
					}
				}
			}
			if pending != (pendingAllocation{}) {
				// avoid blocking on a send to pendingAllocationsIn
				select {
				case <-ctx.Done():
					return
				case pendingAllocationsIn <- pending:
				}
			}
		}
	}()
}

// workQueue essentially functions as a way to aggregate and deduplicate
// incoming work
func workQueue(ctx context.Context) (chan<- string, <-chan string) {
	work := map[pendingAllocation]struct{}{}
	// make a buffered channel for the inbox so readers are a bit less likely
	// to block
	inbox := make(chan string, 1)
	outbox := make(chan string)
	go func() {
		defer close(outbox)
		for {
			// two paths. we want callers to block until there is work ready
			// for them, not give them empty string every time they call. so,
			// we only select on the channel send if there is work
			if len(work) > 0 {
				select {
				case <-ctx.Done():
					return
				case in := <-inbox:
					work[in] = struct{}{}
				case outbox <- pick(work):
				}
			} else {
				select {
				case <-ctx.Done():
					return
				case in := <-inbox:
					work[in] = struct{}{}
				}
			}
		}
	}()
	return inbox, outbox
}

// pick selects one item from a map, deletes it from the map, and returns it.
func pick(set map[pendingAllocation]struct{}) string {
	choice := pendingAllocation{}
	for k := range set {
		choice = k
		break
	}
	delete(set, k)
	return k
}
