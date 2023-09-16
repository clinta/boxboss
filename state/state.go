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
	state              State
	ctx                context.Context
	trigger            chan context.Context
	getLastResult      chan chan<- *StateRunResult
	addBeforeCheckHook chan *beforeCheckHook
	addAfterCheckHook  chan *afterCheckHook
	addAfterRunHook    chan *afterRunHook
}

// StateRunResult holds the result of a state run
type StateRunResult struct {
	changed   bool
	completed time.Time
	err       error
}

func (r *StateRunResult) Changed() bool {
	return r.changed
}

func (r *StateRunResult) Completed() time.Time {
	return r.completed
}

func (r *StateRunResult) Err() error {
	return r.err
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

var checkChangesButNoRunChanges = "check indicated changes were required, but run did not report changes"

// manage is the function that manages the state, runnning whenever a trigger is recieved
//
// # Provided context can be used to stop
//
// TODO: Add rate limiting
func (s *StateRunner) manage() error {
	lastResult := StateNotRunResult
	beforeCheckHooks := make(map[*beforeCheckHook]struct{})
	rmBeforeCheckHook := make(chan *beforeCheckHook)
	afterCheckHooks := make(map[*afterCheckHook]struct{})
	rmAfterCheckHook := make(chan *afterCheckHook)
	afterRunHooks := make(map[*afterRunHook]struct{})
	rmAfterRunHook := make(chan *afterRunHook)
	log := log.With().Str("stateRunner", s.state.Name()).Logger()

	for {
		// triggerCtx will be Done if all trigger contexts are canceled
		triggerCtx, triggerCancel := context.WithCancel(s.ctx)
		triggerCtxWg := sync.WaitGroup{}
		// wait for the first trigger to come in
		select {
		case <-s.ctx.Done():
			log.Debug().Msg("shutting down runner")
			triggerCancel()
			return s.ctx.Err()
		case resCh := <-s.getLastResult:
			resCh <- lastResult
			continue

		case f := <-s.addBeforeCheckHook:
			log := log.With().Str("beforeCheckHook", f.name).Logger()
			log.Debug().Msg("adding beforeCheckHook")
			go func() {
				// f.ctx is a child of s.ctx, so no need to check both
				<-f.ctx.Done()
				select {
				case <-s.ctx.Done():
					// removeCtx is a child of s.ctx, so no need to call removed()
				case rmBeforeCheckHook <- f:
				}
			}()
			beforeCheckHooks[f] = struct{}{}
			continue

		case f := <-rmBeforeCheckHook:
			log := log.With().Str("beforeCheckHook", f.name).Logger()
			log.Debug().Msg("removing beforeCheckHook")
			delete(beforeCheckHooks, f)
			f.removed()
			continue

		case f := <-s.addAfterCheckHook:
			log := log.With().Str("afterCheckHook", f.name).Logger()
			log.Debug().Msg("adding afterCheckHook")
			go func() {
				<-f.ctx.Done()
				select {
				case <-s.ctx.Done():
				case rmAfterCheckHook <- f:
				}
			}()
			afterCheckHooks[f] = struct{}{}
			continue

		case f := <-rmAfterCheckHook:
			log := log.With().Str("afterCheckHook", f.name).Logger()
			log.Debug().Msg("removing afterCheckHook")
			delete(afterCheckHooks, f)
			f.removed()
			continue

		case f := <-s.addAfterRunHook:
			log := log.With().Str("afterRunHook", f.name).Logger()
			log.Debug().Msg("adding afterRunHook")
			go func() {
				<-f.ctx.Done()
				select {
				case <-s.ctx.Done():
				case rmAfterRunHook <- f:
				}
			}()
			afterRunHooks[f] = struct{}{}
			continue

		case f := <-rmAfterRunHook:
			log := log.With().Str("afterRunHook", f.name).Logger()
			log.Debug().Msg("removing afterRunHook")
			delete(afterRunHooks, f)
			f.removed()
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
			bcfg, bcfCtx := errgroup.WithContext(triggerCtx)
			for f := range beforeCheckHooks {
				f := f
				bcfg.Go(func() error {
					log := log.With().Str("beforeCheckHook", f.name).Logger()
					log.Debug().Msg("running beforeCheckHook")
					err := f.f(bcfCtx)
					return err
				})
			}
			err := bcfg.Wait()
			if err != nil {
				log.Err(err).Msg("")
				return false, err
			}

			log.Debug().Msg("running check")
			changeNeeded, err := s.state.Check(triggerCtx)
			if err != nil {
				log.Err(err)
				return changeNeeded, errors.Join(ErrCheckFailed, err)
			}

			acfg, acfCtx := errgroup.WithContext(triggerCtx)
			for f := range afterCheckHooks {
				f := f
				acfg.Go(func() error {
					log := log.With().Str("afterCheckHook", f.name).Logger()
					log.Debug().Msg("running afterCheckHook")
					err := f.f(acfCtx, changeNeeded, err)
					return err
				})
			}
			afcErr := acfg.Wait()

			if err != nil {
				log.Err(err).Msg("")
				return false, err
			}

			if afcErr != nil {
				log.Err(err).Msg("")
			}

			if !changeNeeded {
				log.Debug().Msg("check indicates no changes required")
				return false, nil
			}

			log.Debug().Msg("running")
			changed, err := s.state.Run(triggerCtx)
			err = wrapErr(ErrRunFailed, err)

			if !changed {
				log.Warn().Msg(checkChangesButNoRunChanges)
			}

			log = log.With().Bool("changed", changed).Logger()
			if err != nil {
				log.Err(err)
			}

			return changed, err
			// TODO: Add information to context indicating that this error is from Run()
		}()

		lastResult = &StateRunResult{changed, time.Now(), err}

		afwg := sync.WaitGroup{}
		for f := range afterRunHooks {
			f := f
			afwg.Add(1)
			go func() {
				log := log.With().Str("afterHook", f.name).Logger()
				log.Debug().Msg("running afterHook")
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
		state:              state,
		ctx:                ctx,
		trigger:            make(chan context.Context),
		getLastResult:      make(chan chan<- *StateRunResult),
		addBeforeCheckHook: make(chan *beforeCheckHook),
		addAfterCheckHook:  make(chan *afterCheckHook),
		addAfterRunHook:    make(chan *afterRunHook),
	}
	go s.manage()
	return s
}

type hook struct {
	ctx     context.Context
	removed func()
	name    string
}

func (s *StateRunner) newHook(name string) (hook, func()) {
	ctx, cancel := context.WithCancel(s.ctx)
	removedCtx, removed := context.WithCancel(s.ctx)
	h := hook{
		ctx:     ctx,
		removed: removed,
		name:    name,
	}
	remove := func() {
		cancel()
		<-removedCtx.Done()
	}
	return h, remove
}

type beforeCheckHook struct {
	hook
	f func(ctx context.Context) error
}

type afterCheckHook struct {
	hook
	f func(ctx context.Context, changeNeeded bool, err error) error
}

type afterRunHook struct {
	hook
	f func(ctx context.Context, err error)
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

// ErrBeforeCheckHook is returned if a BeforeCheckHook fails causing the Apply to fail
var ErrBeforeCheckHook = errors.New("beforeCheckHook failed")

// AddBeforeCheckHook adds a function that is run before the Check step of the Runner
// If any of these functions fail, the run will fail returning the first function that errored
// returns a remove funciton that can be used to remove the hook from the runner.
// If The provided function should check the provided context so that it can exit early if the runner is stopped, or other BeforeCheck functions have errored
// If the function returns ErrPreCheckConditionNotMet, it will not be logged as an error, but simply treated as a false condition check
func (s *StateRunner) AddBeforeCheckHook(name string, f func(context.Context) error) func() {
	h, remove := s.newHook(name)
	select {
	case s.addBeforeCheckHook <- &beforeCheckHook{h, wrapErrf(ErrBeforeCheckHook, f)}:
	case <-s.ctx.Done():
		h.removed()
	}
	return remove
}

// ErrConditionNotMet signals that a precheck condition was not met and the state should not run, but did not error in an unexpected way
var ErrConditionNotMet = errors.New("condition not met")

// ErrConditionHook is returned by the state when a Condition failed
//
// This will be wrapped in an ErrBeforeHook, use errors.Is to check for this error.
var ErrConditionHook = errors.New("condition hook failed")

// AddCondition adds a function that is a condition to determine whether or not Check should run.
//
//	The provided function should check the provided context so that it can exit early if the runner is stopped
//
// Cancel ctx to remove the function from the state runner
func (s *StateRunner) AddCondition(name string, f func(context.Context) (conditionMet bool, err error)) func() {
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
	return s.AddBeforeCheckHook("condition: "+name, wrapErrf(ErrConditionHook, bf))
}

// ErrAfterCheckHook is returned if a AfterCheckHook fails causing the Apply to fail
var ErrAfterCheckHook = errors.New("before function failed")

// AddAfterCheckHook adds a function that is run after the Check step of the Runner
// If any of these functions fail, the run will fail returning the first function that errored
//
//	The provided function should check the provided context so that it can exit early if the runner is stopped
//
// Cancel ctx to remove the function from the state runner
func (s *StateRunner) AddAfterCheckHook(name string, f func(ctx context.Context, changeNeeded bool, err error) error) func() {
	h, remove := s.newHook(name)
	select {
	case s.addAfterCheckHook <- &afterCheckHook{h,
		func(ctx context.Context, changeNeeded bool, err error) error {
			return wrapErr(ErrAfterCheckHook, f(ctx, changeNeeded, err))
		}}:
	case <-s.ctx.Done():
		h.removed()
	}
	return remove
}

// AddChangesRequiredHook adds a function that is run after the check step, if changes are required
func (s *StateRunner) AddChangesRequiredHook(name string, f func(context.Context) error) func() {
	return s.AddAfterCheckHook("changesRequired: "+name, func(ctx context.Context, changeNeeded bool, err error) error {
		if changeNeeded && err == nil {
			return f(ctx)
		}
		return err
	})
}

// AddAfterRunHook adds a function that is run at the end of the Run. This may be after the Run step failed or succeeded, or after the Check step failed or succeeded,
// or after any of the other Pre functions failed or succceeded.
//
// The provided function should check the provided context so that it can exit early if the runner is stopped. The err provided to the function is the error returned from the Run.
//
// Cancel ctx to remove the function from the state runner.
//
// AfterRunHooks do not block returning the StateContext result. This means that a subsequent state run could run the AfterHook before the previous one finished.
func (s *StateRunner) AddAfterRunHook(name string, f func(ctx context.Context, err error)) func() {
	h, remove := s.newHook(name)
	select {
	case s.addAfterRunHook <- &afterRunHook{h, f}:
	case <-s.ctx.Done():
		h.removed()
	}
	return remove
}

// AddAfterSuccessHook adds a function that is run after a successful state run.
//
//	The provided function should check the provided context so that it can exit early if the runner is stopped
//
// Cancel ctx to remove the function from the state runner
func (s *StateRunner) AddAfterSuccessHook(name string, f func(context.Context)) func() {
	return s.AddAfterRunHook("afterSuccess: "+name, func(ctx context.Context, err error) {
		if err == nil {
			f(ctx)
		}
	})
}

// AddAfterFailureHook adds a function that is run after a failed state run.
//
//	The provided function should check the provided context so that it can exit early if the runner is stopped
//
// Cancel ctx to remove the function from the state runner
func (s *StateRunner) AddAfterFailureHook(name string, f func(ctx context.Context, err error)) func() {
	return s.AddAfterRunHook("afterFailure: "+name, func(ctx context.Context, err error) {
		if err != nil && !errors.Is(err, ErrConditionNotMet) {
			f(ctx, err)
		}
	})
}

// ErrDependancyFailed is returned by the state when a dependency failed
//
// This will be wrapped in an ErrBeforeCheckHook, use errors.Is to check for this error.
var ErrDependancyFailed = errors.New("dependency failed")

// DependOn makes s dependent on d. If s.Apply is called, this will make sure that d.Apply has been called at least once.
//
// d may fail and it will prevent s from running.
// if d.Apply is run again later, it will not automatically trigger s. To do so, see TriggerOn
func (s *StateRunner) DependOn(d *StateRunner) func() {
	f := wrapErrf(ErrDependancyFailed, d.ApplyOnce)
	remove := s.AddBeforeCheckHook("dependancy: "+d.state.Name(), f)
	go func() {
		<-d.ctx.Done()
		remove()
	}()
	return remove
}

func LogErr(err error) {
	log.Error().Err(err)
}

func DiscardErr(error) {
	//noop
}

// TriggerOnSuccess causes this state to be applied whenever the triggering state succeeds
//
// cb is a callback to handle the result. Use LogErr to simply log the error, or DiscardErr to do nothing
func (s *StateRunner) TriggerOnSuccess(t *StateRunner, cb func(error)) func() {
	f := func(ctx context.Context) {
		cb(s.Apply(ctx))
	}
	remove := t.AddAfterSuccessHook("triggering: "+s.state.Name(), f)
	go func() {
		<-s.ctx.Done()
		remove()
	}()
	return remove
}

// BlockOn prevents s from running while t is running
func (s *StateRunner) BlockOn(t *StateRunner) func() {
	remove := s.AddBeforeCheckHook("blockOn: "+t.state.Name(), func(ctx context.Context) error {
		t.Result(ctx)
		return nil
	})
	go func() {
		<-t.ctx.Done()
		remove()
	}()
	return remove
}
