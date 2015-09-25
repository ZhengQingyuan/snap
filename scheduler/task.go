package scheduler

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/intelsdi-x/gomit"

	"github.com/intelsdi-x/pulse/core"
	"github.com/intelsdi-x/pulse/core/scheduler_event"
	"github.com/intelsdi-x/pulse/pkg/schedule"
	"github.com/intelsdi-x/pulse/scheduler/wmap"
)

const (
	DefaultDeadlineDuration = time.Second * 5
	DefaultStopOnFailure    = 3
)

var (
	schedulerLogger = log.WithField("_module", "scheduler-task")

	ErrTaskNotFound            = errors.New("Task not found")
	ErrTaskNotStopped          = errors.New("Task must be stopped")
	ErrTaskHasAlreadyBeenAdded = errors.New("Task has already been added")
	ErrTaskDisabledOnFailures  = errors.New("Task disabled due to consecutive failures")
)

type task struct {
	sync.Mutex //protects state

	id                 uint64
	name               string
	schResponseChan    chan schedule.Response
	killChan           chan struct{}
	schedule           schedule.Schedule
	workflow           *schedulerWorkflow
	state              core.TaskState
	creationTime       time.Time
	lastFireTime       time.Time
	manager            managesWork
	metricsManager     managesMetrics
	deadlineDuration   time.Duration
	hitCount           uint
	missedIntervals    uint
	failedRuns         uint
	lastFailureMessage string
	lastFailureTime    time.Time
	stopOnFailure      uint
	eventEmitter       gomit.Emitter
}

//NewTask creates a Task
func newTask(s schedule.Schedule, wf *schedulerWorkflow, m *workManager, mm managesMetrics, emitter gomit.Emitter, opts ...core.TaskOption) *task {

	//Task would always be given a default name.
	//However if a user want to change this name, she can pass optional arguments, in form of core.TaskOption
	//The new name then get over written.
	taskId := id()
	name := "Task-" + string(strconv.FormatInt(int64(taskId), 10))
	wf.eventEmitter = emitter
	task := &task{
		id:               taskId,
		name:             name,
		schResponseChan:  make(chan schedule.Response),
		schedule:         s,
		state:            core.TaskStopped,
		creationTime:     time.Now(),
		workflow:         wf,
		manager:          m,
		metricsManager:   mm,
		deadlineDuration: DefaultDeadlineDuration,
		stopOnFailure:    DefaultStopOnFailure,
		eventEmitter:     emitter,
	}
	//set options
	for _, opt := range opts {
		opt(task)
	}
	return task
}

// Option sets the options specified.
// Returns an option to optionally restore the last arg's previous value.
func (t *task) Option(opts ...core.TaskOption) core.TaskOption {
	var previous core.TaskOption
	for _, opt := range opts {
		previous = opt(t)
	}
	return previous
}

//Returns the name of the task
func (t *task) GetName() string {
	return t.name
}

func (t *task) SetName(name string) {
	t.name = name
}

// CreateTime returns the time the task was created.
func (t *task) CreationTime() *time.Time {
	return &t.creationTime
}

func (t *task) DeadlineDuration() time.Duration {
	return t.deadlineDuration
}

func (t *task) SetDeadlineDuration(d time.Duration) {
	t.deadlineDuration = d
}

// HitCount returns the number of times the task has fired.
func (t *task) HitCount() uint {
	return t.hitCount
}

// Id returns the tasks Id.
func (t *task) ID() uint64 {
	return t.id
}

// LastRunTime returns the time of the tasks last run.
func (t *task) LastRunTime() *time.Time {
	return &t.lastFireTime
}

// MissedCount returns the number of intervals missed.
func (t *task) MissedCount() uint {
	return t.missedIntervals
}

// FailedRuns returns the number of intervals missed.
func (t *task) FailedCount() uint {
	return t.failedRuns
}

// LastFailureMessage returns the last error from a task run
func (t *task) LastFailureMessage() string {
	return t.lastFailureMessage
}

// State returns state of the task.
func (t *task) State() core.TaskState {
	return t.state
}

// Status returns the state of the workflow.
func (t *task) Status() WorkflowState {
	return t.workflow.State()
}

func (t *task) SetStopOnFailure(v uint) {
	t.stopOnFailure = v
}

func (t *task) GetStopOnFailure() uint {
	return t.stopOnFailure
}

// Spin will start a task spinning in its own routine while it waits for its
// schedule.
func (t *task) Spin() {
	// We need to lock long enough to change state
	t.Lock()
	defer t.Unlock()
	if t.state == core.TaskStopped {
		t.state = core.TaskSpinning
		t.killChan = make(chan struct{})
		// spin in a goroutine
		go t.spin()
	}
}

func (t *task) Stop() {
	t.Lock()
	defer t.Unlock()
	if t.state == core.TaskFiring || t.state == core.TaskSpinning {
		close(t.killChan)
	}
}

func (t *task) Kill() {
	t.Lock()
	defer t.Unlock()
	if t.state == core.TaskFiring || t.state == core.TaskSpinning {
		close(t.killChan)
		t.state = core.TaskDisabled
	}
}

func (t *task) WMap() *wmap.WorkflowMap {
	return t.workflow.workflowMap
}

func (t *task) Schedule() schedule.Schedule {
	return t.schedule
}

func (t *task) spin() {
	var consecutiveFailures uint
	for {
		schedulerLogger.Debug("task spin loop")
		// Start go routine to wait on schedule
		go t.waitForSchedule()
		// wait here on
		//  schResponseChan - response from schedule
		//  killChan - signals task needs to be stopped
		select {
		case sr := <-t.schResponseChan:
			switch sr.State() {
			// If response show this schedule is stil active we fire
			case schedule.Active:
				t.missedIntervals += sr.Missed()
				t.lastFireTime = time.Now()
				t.hitCount++
				t.fire()
				if t.lastFailureTime == t.lastFireTime {
					consecutiveFailures++
					schedulerLogger.WithFields(log.Fields{
						"_block":                    "spin",
						"task-id":                   t.id,
						"task-name":                 t.name,
						"consecutive failures":      consecutiveFailures,
						"consecutive failure limit": t.stopOnFailure,
						"error":                     t.lastFailureMessage,
					}).Warn("Task failed")
				} else {
					consecutiveFailures = 0
				}
				if consecutiveFailures >= t.stopOnFailure {
					schedulerLogger.WithFields(log.Fields{
						"_block":               "spin",
						"task-id":              t.id,
						"task-name":            t.name,
						"consecutive failures": consecutiveFailures,
						"error":                t.lastFailureMessage,
					}).Error(ErrTaskDisabledOnFailures)
					// You must lock on state change for tasks
					t.Lock()
					t.state = core.TaskDisabled
					t.Unlock()
					// Send task disabled event
					event := new(scheduler_event.TaskDisabledEvent)
					event.TaskID = t.id
					event.Why = fmt.Sprintf("Task disabled with error: %s", t.lastFailureMessage)
					defer t.eventEmitter.Emit(event)
					return
				}
			// Schedule has ended
			case schedule.Ended:
				// You must lock task to change state
				t.Lock()
				t.state = core.TaskEnded
				t.Unlock()
				return //spin

			// Schedule has errored
			case schedule.Error:
				// You must lock task to change state
				t.Lock()
				t.state = core.TaskDisabled
				t.Unlock()
				return //spin

			}
		case <-t.killChan:
			// Only here can it truly be stopped
			t.state = core.TaskStopped
			t.lastFireTime = time.Time{}
			return
		}
	}
}

func (t *task) fire() {
	t.Lock()
	defer t.Unlock()

	t.state = core.TaskFiring
	t.workflow.Start(t)
	t.state = core.TaskSpinning
}

func (t *task) waitForSchedule() {
	select {
	case <-t.killChan:
		return
	case t.schResponseChan <- t.schedule.Wait(t.lastFireTime):
	}
}

type taskCollection struct {
	*sync.Mutex

	table map[uint64]*task
}

func newTaskCollection() *taskCollection {
	return &taskCollection{
		Mutex: &sync.Mutex{},

		table: make(map[uint64]*task),
	}
}

// Get given a task id returns a Task or nil if not found
func (t *taskCollection) Get(id uint64) *task {
	t.Lock()
	defer t.Unlock()

	if t, ok := t.table[id]; ok {
		return t
	}
	return nil
}

// Add given a reference to a task adds it to the collection of tasks.  An
// error is returned if the task alredy exists in the collection.
func (t *taskCollection) add(task *task) error {
	t.Lock()
	defer t.Unlock()

	if _, ok := t.table[task.id]; !ok {
		//If we don't already have this task in the collection save it
		t.table[task.id] = task
	} else {
		schedulerLogger.WithFields(log.Fields{
			"_module": "scheduler-taskCollection",
			"_block":  "add",
			"task id": task.id,
		}).Error(ErrTaskHasAlreadyBeenAdded.Error())
		return ErrTaskHasAlreadyBeenAdded
	}

	return nil
}

// remove will remove a given task from tasks.  The task must be stopped.
// Can return errors ErrTaskNotFound and ErrTaskNotStopped.
func (t *taskCollection) remove(task *task) error {
	t.Lock()
	defer t.Unlock()
	if _, ok := t.table[task.id]; ok {
		if task.state != core.TaskStopped {
			schedulerLogger.WithFields(log.Fields{
				"_block":  "remove",
				"task id": task.id,
			}).Error(ErrTaskNotStopped)
			return ErrTaskNotStopped
		}
		delete(t.table, task.id)
	} else {
		schedulerLogger.WithFields(log.Fields{
			"_block":  "remove",
			"task id": task.id,
		}).Error(ErrTaskNotFound)
		return ErrTaskNotFound
	}
	return nil
}

// Table returns a copy of the taskCollection
func (t *taskCollection) Table() map[uint64]*task {
	t.Lock()
	defer t.Unlock()
	tasks := make(map[uint64]*task)
	for id, t := range t.table {
		tasks[id] = t
	}
	return tasks
}

var idCounter uint64

// id generates the sequential next id (starting from 0)
func id() uint64 {
	return atomic.AddUint64(&idCounter, 1)
}
