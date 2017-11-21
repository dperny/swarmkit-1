package taskreaper

import (
	"sort"
	"time"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/log"
	"github.com/docker/swarmkit/manager/orchestrator"
	"github.com/docker/swarmkit/manager/state"
	"github.com/docker/swarmkit/manager/state/store"
	"golang.org/x/net/context"
)

const (
	// maxDirty is the size threshold for running a task pruning operation.
	maxDirty = 1000
	// reaperBatchingInterval is how often to prune old tasks.
	reaperBatchingInterval = 250 * time.Millisecond
)

// A TaskReaper deletes old tasks when more than TaskHistoryRetentionLimit tasks
// exist for the same service/instance or service/nodeid combination.
type TaskReaper struct {
	store *store.MemoryStore

	// taskHistory is the number of tasks to keep
	taskHistory int64

	// List of slot tubles to be inspected for task history cleanup.
	dirty map[orchestrator.SlotTuple]struct{}

	// List of serviceless orphaned tasks collected for cleanup.
	orphaned []string
	stopChan chan struct{}
	doneChan chan struct{}
}

// New creates a new TaskReaper.
func New(store *store.MemoryStore) *TaskReaper {
	return &TaskReaper{
		store:    store,
		dirty:    make(map[orchestrator.SlotTuple]struct{}),
		stopChan: make(chan struct{}),
		doneChan: make(chan struct{}),
	}
}

// Run is the TaskReaper's watch loop which collects candidates for cleanup.
// Task history is mainly used in task restarts but is also available for administrative purposes.
// Note that the task history is stored per-slot-per-service for replicated services
// and per-node-per-service for global services. History does not apply to serviceless
// since they are not attached to a service.
func (tr *TaskReaper) Run(ctx context.Context) {
	watcher, watchCancel := state.Watch(tr.store.WatchQueue(), api.EventCreateTask{}, api.EventUpdateTask{}, api.EventUpdateCluster{})

	defer func() {
		close(tr.doneChan)
		watchCancel()
	}()

	var tasks []*api.Task
	tr.store.View(func(readTx store.ReadTx) {
		var err error

		clusters, err := store.FindClusters(readTx, store.ByName(store.DefaultClusterName))
		if err == nil && len(clusters) == 1 {
			tr.taskHistory = clusters[0].Spec.Orchestration.TaskHistoryRetentionLimit
		}

		// On startup, scan the entire store and inspect orphaned tasks from previous life.
		tasks, err = store.FindTasks(readTx, store.ByTaskState(api.TaskStateOrphaned))
		if err != nil {
			log.G(ctx).WithError(err).Error("failed to find Orphaned tasks in task reaper init")
		}
	})

	if len(tasks) > 0 {
		for _, t := range tasks {
			// Do not reap service tasks immediately.
			// Let them go through the regular history cleanup process
			// of checking TaskHistoryRetentionLimit.
			if t.ServiceID != "" {
				continue
			}

			// Serviceless tasks can be cleaned up right away since they are not attached to a service.
			tr.orphaned = append(tr.orphaned, t.ID)
		}

		// Clean up orphaned serviceless tasks right away.
		if len(tr.orphaned) > 0 {
			tr.tick()
		}
	}

	// Clean up when we hit TaskHistoryRetentionLimit or when the timer expires,
	// whichever happens first.
	timer := time.NewTimer(reaperBatchingInterval)

	// Watch for:
	// 1. EventCreateTask for cleaning slots, which is the best time to cleanup that node/slot.
	// 2. EventUpdateTask for cleaning serviceless orphaned tasks (when orchestrator updates the task status to ORPHANED).
	// 3. EventUpdateCluster for TaskHistoryRetentionLimit update.
	for {
		select {
		case event := <-watcher:
			switch v := event.(type) {
			case api.EventCreateTask:
				t := v.Task
				tr.dirty[orchestrator.SlotTuple{
					Slot:      t.Slot,
					ServiceID: t.ServiceID,
					NodeID:    t.NodeID,
				}] = struct{}{}
			case api.EventUpdateTask:
				t := v.Task
				if t.Status.State >= api.TaskStateOrphaned && t.ServiceID == "" {
					tr.orphaned = append(tr.orphaned, t.ID)
				}
				if t.Status.State >= api.TaskStateCompleted && t.DesiredState >= api.TaskStateCompleted {
					// if a task is in desired state shutdown AND in actual
					// state shutdown, add it to the dirty list
					tr.dirty[orchestrator.SlotTuple{
						Slot:      t.Slot,
						ServiceID: t.ServiceID,
						NodeID:    t.NodeID,
					}] = struct{}{}
				}
			case api.EventUpdateCluster:
				tr.taskHistory = v.Cluster.Spec.Orchestration.TaskHistoryRetentionLimit
			}

			if len(tr.dirty)+len(tr.orphaned) > maxDirty {
				timer.Stop()
				tr.tick()
			} else {
				timer.Reset(reaperBatchingInterval)
			}
		case <-timer.C:
			timer.Stop()
			tr.tick()
		case <-tr.stopChan:
			timer.Stop()
			return
		}
	}
}

// tick performs task history cleanup.
func (tr *TaskReaper) tick() {
	if len(tr.dirty) == 0 && len(tr.orphaned) == 0 {
		return
	}

	defer func() {
		tr.orphaned = nil
	}()

	deleteTasks := make(map[string]struct{})
	for _, tID := range tr.orphaned {
		deleteTasks[tID] = struct{}{}
	}

	// Check history of dirty tasks for cleanup.
	tr.store.View(func(tx store.ReadTx) {
		for dirty := range tr.dirty {
			service := store.GetService(tx, dirty.ServiceID)
			if service == nil {
				continue
			}

			taskHistory := tr.taskHistory

			// If MaxAttempts is set, keep at least one more than
			// that number of tasks (this overrides TaskHistoryRetentionLimit).
			// This is necessary to reconstruct restart history when the orchestrator starts up.
			// TODO(aaronl): Consider hiding tasks beyond the normal
			// retention limit in the UI.
			// TODO(aaronl): There are some ways to cut down the
			// number of retained tasks at the cost of more
			// complexity:
			//   - Don't force retention of tasks with an older spec
			//     version.
			//   - Don't force retention of tasks outside of the
			//     time window configured for restart lookback.
			if service.Spec.Task.Restart != nil && service.Spec.Task.Restart.MaxAttempts > 0 {
				taskHistory = int64(service.Spec.Task.Restart.MaxAttempts) + 1
			}

			// Negative value for TaskHistoryRetentionLimit is an indication to never clean up task history.
			if taskHistory < 0 {
				continue
			}

			var historicTasks []*api.Task

			switch service.Spec.GetMode().(type) {
			case *api.ServiceSpec_Replicated:
				// Clean out the slot for which we received EventCreateTask.
				var err error
				historicTasks, err = store.FindTasks(tx, store.BySlot(dirty.ServiceID, dirty.Slot))
				if err != nil {
					continue
				}

			case *api.ServiceSpec_Global:
				// Clean out the node history in case of global services.
				tasksByNode, err := store.FindTasks(tx, store.ByNodeID(dirty.NodeID))
				if err != nil {
					continue
				}

				for _, t := range tasksByNode {
					if t.ServiceID == dirty.ServiceID {
						historicTasks = append(historicTasks, t)
					}
				}
			}

			// check if the history has any live tasks
			anyLive := false
			for _, t := range historicTasks {
				if t.DesiredState <= api.TaskStateRunning || t.Status.State <= api.TaskStateRunning {
					// Don't delete running tasks
					anyLive = true
				}
			}
			// if not, delete all of the tasks in the history
			if !anyLive {
				for _, t := range historicTasks {
					deleteTasks[t.ID] = struct{}{}
				}
				continue
			}

			if int64(len(historicTasks)) <= taskHistory {
				continue
			}

			// TODO(aaronl): This could filter for non-running tasks and use quickselect
			// instead of sorting the whole slice.
			// TODO(aaronl): This sort should really use lamport time instead of wall
			// clock time. We should store a Version in the Status field.
			sort.Sort(orchestrator.TasksByTimestamp(historicTasks))

			candidates := []string{}
			runningTasks := 0
			for _, t := range historicTasks {
				if t.DesiredState <= api.TaskStateRunning || t.Status.State <= api.TaskStateRunning {
					// Don't delete running tasks
					runningTasks++
					continue
				}

				deleteTasks[t.ID] = struct{}{}

				taskHistory++
				if int64(len(historicTasks)) <= taskHistory {
					break
				}
			}

			if runningTasks <= 1 {
				delete(tr.dirty, dirty)
			}
		}
	})

	// Perform cleanup.
	if len(deleteTasks) > 0 {
		tr.store.Batch(func(batch *store.Batch) error {
			for taskID := range deleteTasks {
				batch.Update(func(tx store.Tx) error {
					return store.DeleteTask(tx, taskID)
				})
			}
			return nil
		})
	}
}

// Stop stops the TaskReaper and waits for the main loop to exit.
func (tr *TaskReaper) Stop() {
	close(tr.stopChan)
	<-tr.doneChan
}
