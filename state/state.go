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

var ErrCheckFailed = errors.New("state check failed")
var ErrRunFailed = errors.New("state run failed")

// State interface is the interface that a state module must implement
type State interface {
	// Name is a name identifying the state, used in logging
	Name() string

	// Check checks whether or not changes are necessary before running them
	Check(context.Context) (bool, error)
	// Run runs the states, making system modifications
	Run(context.Context) (bool, error)
}

// BasicState is a simple implementation of the state interface
type BasicState struct {
	name  string
	check func(context.Context) (bool, error)
	run   func(context.Context) (bool, error)
}

func (b *BasicState) Name() string {
	return b.name
}

func (b *BasicState) Check(ctx context.Context) (bool, error) {
	return b.check(ctx)
}

func (b *BasicState) Run(ctx context.Context) (bool, error) {
	return b.run(ctx)
}

// NewBasicState creates a new simple state
func NewBasicState(name string, check func(context.Context) (bool, error), run func(context.Context) (bool, error)) State {
	return &BasicState{
		name:  name,
		check: check,
		run:   run,
	}
}

// StateRunner runs the state whenever a trigger is received
type StateRunner struct {
	state         State
	ctx           context.Context
	trigger       chan context.Context
	getLastResult chan chan<- *StateRunResult
	addBeforeFunc chan *beforeFunc
	addAfterFunc  chan *afterFunc
}

// StateRunResult holds the result of a state run
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
		return s.Result(ctx).err
	}
}

// ApplyOnce will apply only if the state has not applied already, if state has already been applied, will return the last error
//
// ApplyOnce will block if the state is currently applying
func (s *StateRunner) ApplyOnce(ctx context.Context) error {
	res := s.Result(ctx)
	if res != StateNotRunResult {
		return res.err
	}
	return s.Apply(ctx)
}

// ErrStateNotRun is the error in the StateResult of a StateRunner that has not run yet
var ErrStateNotRun = errors.New("state has not yet run")

// StateNotRunResult is in the StateResult if the StateRunner has not yet run at all
var StateNotRunResult = &StateRunResult{false, time.Time{}, ErrStateNotRun}

// manage is the function that manages the state, runnning whenever a trigger is recieved
//
// # Provided context can be used to stop
//
// TODO: Add rate limiting
func (s *StateRunner) manage() error {
	ctx := s.ctx
	lastResult := StateNotRunResult
	beforeFuncs := make(map[*beforeFunc]struct{})
	rmBeforeFunc := make(chan *beforeFunc)
	afterFuncs := make(map[*afterFunc]struct{})
	rmAfterFunc := make(chan *afterFunc)
	log := log.With().Str("stateRunner", s.state.Name()).Logger()

	for {
		// triggerCtx will be Done if all trigger contexts are canceled
		triggerCtx, triggerCancel := context.WithCancel(ctx)
		triggerCtxWg := sync.WaitGroup{}
		// wait for the first trigger to come in
		select {
		case <-ctx.Done():
			log.Debug().Msg("shutting down runner")
			triggerCancel()

			// better for senders to panic than deadlock
			// this should only happen if someone tries to use the StateRunner
			// after they canceled ctx
			close(s.trigger)
			close(s.getLastResult)
			close(s.addBeforeFunc)
			close(s.addAfterFunc)
			return ctx.Err()
		case resCh := <-s.getLastResult:
			resCh <- lastResult
			continue

		case f := <-s.addBeforeFunc:
			log := log.With().Str("beforeFunc", f.name).Logger()
			log.Debug().Msg("adding beforeFunc")
			go func() {
				select {
				case <-ctx.Done():
					return
				case <-f.ctx.Done():
				}

				select {
				case <-ctx.Done():
					return
				case rmBeforeFunc <- f:
				}
			}()
			beforeFuncs[f] = struct{}{}
			continue

		case f := <-rmBeforeFunc:
			log := log.With().Str("beforeFunc", f.name).Logger()
			log.Debug().Msg("removing beforeFunc")
			delete(beforeFuncs, f)
			continue

		case f := <-s.addAfterFunc:
			log := log.With().Str("afterFunc", f.name).Logger()
			log.Debug().Msg("adding afterFunc")
			go func() {
				select {
				case <-ctx.Done():
					return
				case <-f.ctx.Done():
				}

				select {
				case <-ctx.Done():
					return
				case rmAfterFunc <- f:
				}
			}()
			afterFuncs[f] = struct{}{}
			continue

		case f := <-rmAfterFunc:
			log := log.With().Str("afterFunc", f.name).Logger()
			log.Debug().Msg("removing afterFunc")
			delete(afterFuncs, f)
			continue

		case tCtx := <-s.trigger:
			triggerCtxWg.Add(1)
			go func() {
				select {
				case <-tCtx.Done():
				case <-triggerCtx.Done():
				}
				triggerCtxWg.Done()
			}()
			log.Debug().Msg("triggered")
		}

		// collect all pending triggers
		for {
			select {
			case tCtx := <-s.trigger:
				triggerCtxWg.Add(1)
				go func() {
					select {
					case <-tCtx.Done():
					case <-triggerCtx.Done():
					}
					triggerCtxWg.Done()
				}()
				continue
			default:
				go func() {
					// cancel triggerCtx if all triggers contexts are canceled
					triggerCtxWg.Wait()
					triggerCancel()
				}()
			}
			break
		}

		changed, err := func() (bool, error) {
			bfg, bfCtx := errgroup.WithContext(triggerCtx)
			for f := range beforeFuncs {
				f := f
				bfg.Go(func() error {
					log := log.With().Str("beforeFunc", f.name).Logger()
					log.Debug().Msg("running beforeFunc")
					err := f.f(bfCtx)
					return err
				})
			}
			err := bfg.Wait()
			if err != nil {
				log.Err(err)
				return false, err
			}

			log.Debug().Msg("running check")
			changeNeeded, err := s.state.Check(triggerCtx)
			if err != nil {
				log.Err(err)
				return false, errors.Join(ErrCheckFailed, err)
			}

			if !changeNeeded {
				log.Debug().Msg("check indicates no changes required")
				return false, nil
			}

			log.Debug().Msg("running")
			changed, err := s.state.Run(triggerCtx)
			err = wrapErr(ErrRunFailed, err)

			log = log.With().Bool("changed", changed).Logger()
			if err != nil {
				log.Err(err)
			}

			return changed, err
			// TODO: Add information to context indicating that this error is from Run()
		}()

		lastResult = &StateRunResult{changed, time.Now(), err}

		afwg := sync.WaitGroup{}
		for f := range afterFuncs {
			f := f
			afwg.Add(1)
			go func() {
				log := log.With().Str("afterFunc", f.name).Logger()
				log.Debug().Msg("running afterFunc")
				f.f(triggerCtx, err)
			}()
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

		// done with triggerCtx
		triggerCancel()
		triggerCtxWg.Wait()
	}
}

// Result gets the StateRunner result
//
// This will block if the state is currently running, unless ctx is canceled.
func (s *StateRunner) Result(ctx context.Context) *StateRunResult {
	res := make(chan *StateRunResult)
	select {
	case s.getLastResult <- res:
		return <-res
	case <-ctx.Done():
		return &StateRunResult{false, time.Time{}, ctx.Err()}
	case <-s.ctx.Done():
		return &StateRunResult{false, time.Time{}, s.ctx.Err()}
	}
}

// Running returns true if the state is currently running
func (s *StateRunner) Running() bool {
	res := make(chan *StateRunResult)
	select {
	case s.getLastResult <- res:
		<-res
		return false
	default:
		return true
	}
}

// NewStateRunner creates the state runner that will run, listening for triggers from Apply until ctx is canceled
//
// Do not attempt to use the state runner after the context is canceled
func NewStateRunner(ctx context.Context, state State) *StateRunner {
	s := &StateRunner{
		state:         state,
		ctx:           ctx,
		trigger:       make(chan context.Context),
		getLastResult: make(chan chan<- *StateRunResult),
		addBeforeFunc: make(chan *beforeFunc),
		addAfterFunc:  make(chan *afterFunc),
	}
	go s.manage()
	return s
}

type beforeFunc struct {
	ctx    context.Context
	name   string
	cancel func()
	f      func(ctx context.Context) error
}

type afterFunc struct {
	ctx    context.Context
	name   string
	cancel func()
	f      func(ctx context.Context, err error)
}

func wrapErr(parent error, child error) error {
	if child == nil {
		return nil
	}
	return errors.Join(parent, child)
}

func wrapErrf(parent error, f func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		return wrapErr(parent, f(ctx))
	}
}

// ErrBeforeFunc is returned if a BeforeFunc fails causing the Apply to fail
var ErrBeforeFunc = errors.New("before function failed")

// AddBeforeFunc adds a function that is run before the Check step of the Runner
// If any of these functions fail, the run will fail returning the first function that errored
//
//	The provided function should check the provided context so that it can exit early if the runner is stopped
//
// # If the function returns ErrPreCheckConditionNotMet, it will not be logged as an error, but simply treated as a false condition check
//
// Cancel ctx to remove the function from the state runner
func (s *StateRunner) AddBeforeFunc(ctx context.Context, name string, f func(context.Context) error) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case s.addBeforeFunc <- &beforeFunc{ctx, name, cancel, wrapErrf(ErrBeforeFunc, f)}:
		case <-ctx.Done():
		case <-s.ctx.Done():
		}
	}()
}

// ErrConditionNotMet signals that a precheck condition was not met and the state should not run, but did not error in an unexpected way
var ErrConditionNotMet = errors.New("condition not met")

// ErrConditionFunc is returned by the state when a Condition failed
//
// This will be wrapped in an ErrBeforeFunc, use errors.Is to check for this error.
var ErrConditionFunc = errors.New("condition function failed")

// AddCondition adds a function that is a condition to determine whether or not Check should run.
//
//	The provided function should check the provided context so that it can exit early if the runner is stopped
//
// Cancel ctx to remove the function from the state runner
func (s *StateRunner) AddCondition(ctx context.Context, name string, f func(context.Context) (conditionMet bool, err error)) {
	bf := func(ctx context.Context) error {
		v, err := f(ctx)
		if err != nil {
			return err
		}
		if !v {
			return ErrConditionNotMet
		}
		return nil
	}
	s.AddBeforeFunc(ctx, "condition: "+name, wrapErrf(ErrConditionFunc, bf))
}

// AddAfterFunc adds a function that is run at the end of the Run. This may be after the Run step failed or succeeded, or after the Check step failed or succeeded,
// or after any of the other Pre functions failed or succceeded.
//
// The provided function should check the provided context so that it can exit early if the runner is stopped. The err provided to the function is the error returned from the Run.
//
// Cancel ctx to remove the function from the state runner.
//
// AfterFuncs do not block returning the StateContext result. This means that a subsequent state run could run the AfterFunc before the previous one finished.
func (s *StateRunner) AddAfterFunc(ctx context.Context, name string, f func(ctx context.Context, err error)) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case s.addAfterFunc <- &afterFunc{ctx, name, cancel, f}:
		case <-ctx.Done():
		case <-s.ctx.Done():
		}
	}()
}

// AddAfterSuccess adds a function that is run after a successful state run.
//
//	The provided function should check the provided context so that it can exit early if the runner is stopped
//
// Cancel ctx to remove the function from the state runner
func (s *StateRunner) AddAfterSuccess(ctx context.Context, name string, f func(context.Context)) {
	s.AddAfterFunc(ctx, "afterSuccess: "+name, func(ctx context.Context, err error) {
		if err == nil {
			f(ctx)
		}
	})
}

// AddAfterFailure adds a function that is run after a failed state run.
//
//	The provided function should check the provided context so that it can exit early if the runner is stopped
//
// Cancel ctx to remove the function from the state runner
func (s *StateRunner) AddAfterFailure(ctx context.Context, name string, f func(ctx context.Context, err error)) {
	s.AddAfterFunc(ctx, "afterFailure: "+name, func(ctx context.Context, err error) {
		if err != nil && !errors.Is(err, ErrConditionNotMet) {
			f(ctx, err)
		}
	})
}

// ErrDependancyFailed is returned by the state when a dependency failed
//
// This will be wrapped in an ErrBeforeFunc, use errors.Is to check for this error.
var ErrDependancyFailed = errors.New("dependency failed")

// DependOn makes s dependent on d. If s.Apply is called, this will make sure that d.Apply has been called at least once.
//
// d may fail and it will prevent s from running.
// if d.Apply is run again later, it will not automatically trigger s. To do so, see TriggerOn
func (s *StateRunner) DependOn(ctx context.Context, d *StateRunner) {
	f := wrapErrf(ErrDependancyFailed, d.ApplyOnce)
	s.AddBeforeFunc(ctx, "dependancy: "+d.state.Name(), f)
}

func LogErr(err error) {
	log.Error().Err(err)
}

func DiscardErr(error) {
	//noop
}

// TriggerOn causes this state to be applied whenever the triggering state succeeds
//
// cb is a callback to handle the result. Use LogErr to simply log the error, or DiscardErr to do nothing
func (s *StateRunner) TriggerOn(ctx context.Context, t *StateRunner, cb func(error)) {
	f := func(ctx context.Context) {
		cb(s.Apply(ctx))
	}
	t.AddAfterSuccess(ctx, "triggering: "+s.state.Name(), f)
}
