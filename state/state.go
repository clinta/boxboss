// Package state includes the state interface which should be implemented by plugins, and StateRunner which manages a state
package state

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"
)

func init() {
	// TODO this should be somewhere else
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
}

// State interface is the interface that a state module must implement
type State interface {
	// Check checks whether or not changes are necessary before running them
	Check(context.Context) (bool, error)
	// Run runs the states, making system modifications
	Run(context.Context) (bool, error)
}

// StateRunner runs the state whenever a trigger is received
type StateRunner struct {
	state         State
	trigger       chan context.Context
	getLastResult chan chan<- *StateRunResult
	addBeforeFunc chan *beforeFunc
	addAfterFunc  chan *afterFunc
}

type StateRunResult struct {
	changed   bool
	completed time.Time
	err       error
}

// Apply will apply the state. If the state is already running, it will block until the existing state run is complete, then run again
//
// If multiple request to apply come in while the state is running, they will not all block until the net run completes, but the next run will
// only run once. Errors from that application will be returned to all callers, but only the first callers context will be used.
func (s *StateRunner) Apply(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.trigger <- ctx:
		return s.Wait(ctx).err
	}
}

// ApplyOnce will apply only if the state has not applied already, if state has already been applied, will return the last error
//
// ApplyOnce will block if the state is currently applying
func (s *StateRunner) ApplyOnce(ctx context.Context) error {
	res := s.Wait(ctx)
	if res != StateNotRunResult {
		return res.err
	}
	return s.Apply(ctx)
}

var ErrStateNotRun = errors.New("state has not yet run")
var StateNotRunResult = &StateRunResult{false, time.Time{}, ErrStateNotRun}

// Manage is the function that manages the state, runnning whenever a trigger is recieved
//
// Provided context can be used to stop
func (s *StateRunner) manage(ctx context.Context) error {
	lastResult := StateNotRunResult
	beforeFuncs := make(map[*beforeFunc]struct{})
	rmBeforeFunc := make(chan *beforeFunc)
	afterFuncs := make(map[*afterFunc]struct{})
	rmAfterFunc := make(chan *afterFunc)

	for {
		var triggerCtx context.Context
		// wait for the first trigger to come in
		select {
		case <-ctx.Done():
			for f := range beforeFuncs {
				f.cancel()
			}
			for f := range afterFuncs {
				f.cancel()
			}
			// consume all waiting removals, don't bother actually removing, they will be GCd
			for {
				select {
				case <-rmBeforeFunc:
					continue
				case <-rmAfterFunc:
					continue
				default:
					return ctx.Err()
				}
			}

		case resCh := <-s.getLastResult:
			resCh <- lastResult
			continue

		case f := <-s.addBeforeFunc:
			go func() {
				<-f.ctx.Done()
				rmBeforeFunc <- f
			}()
			beforeFuncs[f] = struct{}{}
			continue

		case f := <-rmBeforeFunc:
			delete(beforeFuncs, f)
			continue

		case f := <-s.addAfterFunc:
			go func() {
				<-f.ctx.Done()
				rmAfterFunc <- f
			}()
			afterFuncs[f] = struct{}{}
			continue

		case f := <-rmAfterFunc:
			delete(afterFuncs, f)
			continue

		case triggerCtx = <-s.trigger:
		}

		// collect all pending triggers
		for {
			select {
			case <-s.trigger:
				continue
			default:
			}
			break
		}

		// TODO: run preFuncs
		changed, err := func() (bool, error) {
			bfg, bfCtx := errgroup.WithContext(triggerCtx)
			for f := range beforeFuncs {
				f := f
				bfg.Go(func() error { return f.f(bfCtx) })
			}
			err := bfg.Wait()
			if err != nil {
				// TODO: add information into the context indicating that this error is from a beforeFunc
				// TODO: Check for requirementnotmet which is not an error
				return false, err
			}

			changeNeeded, err := s.state.Check(triggerCtx)
			if err != nil {
				// TODO: add information into context indicating that this error is from changes
				return false, err
			}

			if !changeNeeded {
				return false, nil
			}

			return s.state.Run(triggerCtx)
			// TODO: Add information to context indicating that this error is from Run()
		}()

		lastResult = &StateRunResult{changed, time.Now(), err}

		afwg := sync.WaitGroup{}
		for f := range afterFuncs {
			f := f
			afwg.Add(1)
			go func() { f.f(triggerCtx, err) }()
		}

		// Return any waiting results before listening for new trigggers
		for {
			select {
			case resCh := <-s.getLastResult:
				resCh <- lastResult
				continue
			default:
			}
			break
		}
	}
}

func (s *StateRunner) Wait(ctx context.Context) *StateRunResult {
	res := make(chan *StateRunResult)
	select {
	case s.getLastResult <- res:
		return <-res
	case <-ctx.Done():
		return &StateRunResult{false, time.Time{}, ctx.Err()}
	}
}

func (s *StateRunner) Running() bool {
	res := make(chan *StateRunResult)
	select {
	case s.getLastResult <- res:
		<-res
		return true
	default:
		return false
	}
}

// NewStateRunner creates the state runner that will run as long as the context is valid
//
// Do not attempt to use the state runner after the context is canceled, many operations will block forever
func NewStateRunner(ctx context.Context, state State) *StateRunner {
	s := &StateRunner{
		state:         state,
		trigger:       make(chan context.Context),
		getLastResult: make(chan chan<- *StateRunResult),
		addBeforeFunc: make(chan *beforeFunc),
		addAfterFunc:  make(chan *afterFunc),
	}
	go s.manage(ctx)
	return s
}

type beforeFunc struct {
	ctx    context.Context
	cancel func()
	f      func(ctx context.Context) error
}

type afterFunc struct {
	ctx    context.Context
	cancel func()
	f      func(ctx context.Context, err error)
}

// AddBeforeFunc adds a function that is run before the Check step of the Runner
// If any PreChecks fail, the run will be canceled
//
// If the function returns ErrPreCheckConditionNotMet, it will not be logged as an error, but simply treated as a false condition check
// Cancel the provided context to remove the function from the state runner
func (s *StateRunner) AddBeforeFunc(ctx context.Context, f func(context.Context) error) {
	ctx, cancel := context.WithCancel(ctx)
	s.addBeforeFunc <- &beforeFunc{ctx, cancel, f}
}

// ErrConditionNotMet signals that a precheck condition was not met and the state should not run, but did not error in an unexpected way
var ErrConditionNotMet = errors.New("PreCheck condition not met")

// AddCondition adds a function that is a condition to determine whether or not Check should even run.
// Cancel the provided context to remove the function from the state runner
func (s *StateRunner) AddCondition(ctx context.Context, f func(context.Context) (conditionMet bool, err error)) {
	s.AddBeforeFunc(ctx, func(ctx context.Context) error {
		v, err := f(ctx)
		if err != nil {
			return err
		}
		if !v {
			return ErrConditionNotMet
		}
		return nil
	})
}

// AddAfterFunc adds a function that is run at the end of the Run. This may be after the Run step failed or succeeded, or after the Check step failed or succeeded,
// or after any of the other Pre functions failed or succceeded.
//
// Cancel the provided context to remove the function from the state runner
func (s *StateRunner) AddAfterFunc(ctx context.Context, f func(ctx context.Context, err error)) {
	ctx, cancel := context.WithCancel(ctx)
	s.addAfterFunc <- &afterFunc{ctx, cancel, f}
}

func (s *StateRunner) AddAfterSuccess(ctx context.Context, f func(context.Context)) {
	s.AddAfterFunc(ctx, func(ctx context.Context, err error) {
		if err == nil {
			f(ctx)
		}
	})
}

func (s *StateRunner) AddAfterFailure(ctx context.Context, f func(ctx context.Context, err error)) {
	s.AddAfterFunc(ctx, func(ctx context.Context, err error) {
		if err != nil {
			f(ctx, err)
		}
	})
}

// Run runs the StateRunner and returns a poitner to that can be used for
// checking the running status, or triggering a state
//
// ctx can be used to stop the Runner.
func (s *StateRunner) Run(ctx context.Context) {
	go s.manage(ctx)
}
