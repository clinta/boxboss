package state

import (
	"sync"
	"sync/atomic"
)

// Status indicates the status of a a state
type Status uint64

const (
	NotRun Status = iota
	Triggered
	PreCheck
	Checking
	PostCheck
	PreRun
	Running
	Succeeeded
	Failed
)

func (s Status) IsRunning() bool {
	if s == NotRun || s == Succeeeded || s == Failed {
		return false
	}
	return true
}

// State is any state that can be run
type State interface {
	Check() (bool, error)
	Run() (bool, error)

	// provided by BaseState
	Status() Status
	getTrigger() <-chan struct{}
}

// BaseState provides the basic functionality for states, if embedded into a state, state only needs to implement Check() and Run()
// TODO: Better name for this, this is to be embedded in actual state plugins
// This embedding will implement everything needed for the State interface, except for check and run
type BaseState struct {
	status         atomic.Uint64
	trigger        chan struct{}
	listeners      []chan<- Status
	listenersMutex sync.RWMutex
}

// Status returns the current status for this state
func (b *BaseState) Status() Status {
	return Status(b.status.Load())
}

// setStatus sets the current status of this state, and dispatches all the listener channels
func (b *BaseState) setStatus(v Status) {
	b.status.Store(uint64(v))
	b.listenersMutex.RLock()
	for _, c := range b.listeners {
		go func(c chan<- Status) { c <- v }(c)
	}
	b.listenersMutex.RUnlock()
}

// Adds a trigger. Triggers are only processed if the state is not in one of the running states. Close stop channel to remove trigger.
func (b *BaseState) AddTrigger(trigger <-chan struct{}, stop <-chan struct{}) {
	go func() {
		for {
			select {
			case <-trigger:
				if b.Status().IsRunning() {
					// TODO: Log ignoring trigger because state is running
					continue
				}
				b.trigger <- struct{}{}
			case <-stop:
				return
			}
		}
	}()
}

func (b *BaseState) getTrigger() <-chan struct{} {
	return b.trigger
}

func (b *BaseState) AddListener(c chan<- Status) {
	b.listenersMutex.Lock()
	b.listeners = append(b.listeners, c)
	b.listenersMutex.Unlock()
}

// ManageState listens for triggers and manages a state, should be launched in a goroutine
func ManageState(s State) {
	go manageState(s)
}

func manageState(s State) {
	for range s.getTrigger() {
	}
}

// Compiler checks to make sure the interface is properly implemented
// TODO: Move this into a test

type dummyState struct {
	BaseState
}

func (d *dummyState) Check() (bool, error) {
	return false, nil
}

func (d *dummyState) Run() (bool, error) {
	return false, nil
}

var _ State = (*dummyState)(nil)

//////
