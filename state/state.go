package state

import (
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// State is any state that can be run
type State interface {
	Check() (bool, error)
	Run() (bool, error)
	// Error value from last run
	LastError() error
	// End timestamp of last run
	LastRun() time.Time
	// Whether or not the state made changes during it's last run
	Changed() bool

	// provided by BaseState
	Running() bool
	setRunning(bool)
	getTrigger() <-chan struct{}
	preChecks() ([]func() error, func())
	postChecks() ([]func() error, func())
	postRuns() ([]func() error, func())
	setLastError(error)
	setLastRun(time.Time)
	setChanged(bool)
}

// BaseState provides the basic functionality for states, if embedded into a state, state only needs to implement Check() and Run()
// TODO: Better name for this, this is to be embedded in actual state plugins
// This embedding will implement everything needed for the State interface, except for check and run
type BaseState struct {
	lastError       error
	lastRun         time.Time
	changed         bool
	trigger         chan struct{}
	running         atomic.Bool
	preChecksMutex  sync.Mutex
	preChecks_      []func() error
	postChecksMutex sync.Mutex
	postChecks_     []func() error
	postRunsMutex   sync.Mutex
	postRuns_       []func() error
}

func (b *BaseState) Running() bool {
	return b.running.Load()
}

func (b *BaseState) setRunning(v bool) {
	b.running.Store(v)
}

// Adds a trigger. Triggers are only processed if the state is not in one of the running states. Close stop channel to remove trigger.
func (b *BaseState) AddTrigger(trigger <-chan struct{}, stop <-chan struct{}) {
	go func() {
		for {
			select {
			case <-trigger:
				if b.Running() {
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

func (b *BaseState) AddPreCheck(f func() error) {
	b.preChecksMutex.Lock()
	b.preChecks_ = append(b.preChecks_, f)
	b.preChecksMutex.Unlock()
}

// gets the preChecks slice, and the function to runlock when done reading it
func (b *BaseState) preChecks() ([]func() error, func()) {
	b.preChecksMutex.Lock()
	return b.preChecks_, b.preChecksMutex.Unlock
}

func (b *BaseState) AddPostCheck(f func() error) {
	b.postChecksMutex.Lock()
	b.postChecks_ = append(b.postChecks_, f)
	b.postChecksMutex.Unlock()
}

// gets the postChecks slice, and the function to runlock when done reading it
func (b *BaseState) postChecks() ([]func() error, func()) {
	b.postChecksMutex.Lock()
	return b.postChecks_, b.postChecksMutex.Unlock
}

func (b *BaseState) AddPreRun(f func() error) {
	f = func() error {
		if b.lastError != nil {
			return nil
		}
		return f()
	}
	b.AddPostCheck(f)
}

func (b *BaseState) AddPostRun(f func() error) {
	b.postRunsMutex.Lock()
	b.postRuns_ = append(b.postRuns_, f)
	b.postRunsMutex.Unlock()
}

// gets the postRuns slice, and the function to runlock when done reading it
func (b *BaseState) postRuns() ([]func() error, func()) {
	b.postRunsMutex.Lock()
	return b.postRuns_, b.postRunsMutex.Unlock
}

func (b *BaseState) AddPostSuccess(f func() error) {
	f = func() error {
		if b.lastError != nil {
			return nil
		}
		return f()
	}
	b.AddPostRun(f)
}

func (b *BaseState) AddPostFailure(f func() error) {
	f = func() error {
		if b.lastError == nil {
			return nil
		}
		return f()
	}
	b.AddPostRun(f)
}

func (b *BaseState) setLastError(err error) {
	b.lastError = err
}

func (b *BaseState) LastError() error {
	return b.lastError
}

func (b *BaseState) setLastRun(t time.Time) {
	b.lastRun = t
}

func (b *BaseState) LastRun() time.Time {
	return b.lastRun
}

func (b *BaseState) setChanged(v bool) {
	b.changed = v
}

func (b *BaseState) Changed() bool {
	return b.changed
}

// ManageState listens for triggers and manages a state, should be launched in a goroutine
func ManageState(s State) {
	go manageState(s)
}

func manageState(s State) {
	for range s.getTrigger() {
		runState(s)
	}
}

func runState(s State) {
	s.setRunning(true)
	s.setLastError(nil)
	defer s.setRunning(false)
	defer s.setLastRun(time.Now())
	s.setChanged(false)
	err := func() error {
		{ // Run preChecks
			g := new(errgroup.Group)
			preChecks, unlock := s.preChecks()
			for _, f := range preChecks {
				g.Go(f)
			}
			unlock()
			if err := g.Wait(); err != nil {
				return err
			}
		}
		{ // Run the check
			check, err := s.Check()
			if err != nil {
				return err
			}
			if !check {
				return nil
				// TODO Debug log, no changes required
			}
		}
		{ // Run the postChecks
			g := new(errgroup.Group)
			postChecks, unlock := s.postChecks()
			for _, f := range postChecks {
				g.Go(f)
			}
			unlock()
			if err := g.Wait(); err != nil {
				return err
			}
		}
		// Run the state
		changed, err := s.Run()
		s.setChanged(changed)
		return err
	}()
	s.setLastError(err)

	// Run the postRuns
	err = func() error {
		g := new(errgroup.Group)
		postRuns, unlock := s.postRuns()
		for _, f := range postRuns {
			g.Go(f)
		}
		unlock()
		if err := g.Wait(); err != nil {
			return err
		}
		return nil
	}()

	// Only use the postrun errors if the main run did not already set an error
	if s.LastError() == nil && err != nil {
		s.setLastError(err)
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
