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

// manage is the function that manages the state, runnning whenever a trigger is recieved
//
// Provided context can be used to stop
func (s *StateRunner) manage() error {
	ctx := s.ctx
	lastResult := StateNotRunResult
	beforeFuncs := make(map[*beforeFunc]struct{})
	rmBeforeFunc := make(chan *beforeFunc)
	afterFuncs := make(map[*afterFunc]struct{})
	rmAfterFunc := make(chan *afterFunc)
	log := log.With().Str("state", s.state.Name()).Logger()

	for {
		var triggerCtx context.Context
		// wait for the first trigger to come in
		select {
		case <-ctx.Done():
			log.Debug().Msg("shutting down runner")

			// better to panic than deadlock
			close(s.trigger)
			close(s.getLastResult)
			close(s.addBeforeFunc)
			close(s.addAfterFunc)
			return ctx.Err()
		case resCh := <-s.getLastResult:
			resCh <- lastResult
			continue

		case f := <-s.addBeforeFunc:
			go func() {
				select {
				case <-ctx.Done():
					return
				case <-f.ctx.Done():
				}
				rmBeforeFunc <- f
			}()
			beforeFuncs[f] = struct{}{}
			continue

		case f := <-rmBeforeFunc:
			delete(beforeFuncs, f)
			continue

		case f := <-s.addAfterFunc:
			go func() {
				select {
				case <-ctx.Done():
					return
				case <-f.ctx.Done():
				}
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
				return false, errors.Join(ErrCheckFailed, err)
			}

			if !changeNeeded {
				return false, nil
			}

			changed, err := s.state.Run(triggerCtx)
			err = wrapErr(ErrRunFailed, err)
			return changed, err
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
	case <-s.ctx.Done():
		return &StateRunResult{false, time.Time{}, s.ctx.Err()}
	}
}

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

// NewStateRunner creates the state runner that will run as long as the context is valid
//
// Do not attempt to use the state runner after the context is canceled, many operations will block forever
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
	cancel func()
	f      func(ctx context.Context) error
}

type afterFunc struct {
	ctx    context.Context
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

var ErrBeforeFunc = errors.New("before function failed")

// AddBeforeFunc adds a function that is run before the Check step of the Runner
// If any PreChecks fail, the run will be canceled
//
// If the function returns ErrPreCheckConditionNotMet, it will not be logged as an error, but simply treated as a false condition check
// Cancel the provided context to remove the function from the state runner
func (s *StateRunner) AddBeforeFunc(ctx context.Context, f func(context.Context) error) {
	ctx, cancel := context.WithCancel(ctx)
	log.With().Str("funcType", "beforeFunc").Logger().WithContext(ctx)
	select {
	case s.addBeforeFunc <- &beforeFunc{ctx, cancel, wrapErrf(ErrBeforeFunc, f)}:
	case <-ctx.Done():
	case <-s.ctx.Done():
	}
}

// ErrConditionNotMet signals that a precheck condition was not met and the state should not run, but did not error in an unexpected way
var ErrConditionNotMet = errors.New("condition not met")
var ErrConditionFunc = errors.New("condition function failed")

// AddCondition adds a function that is a condition to determine whether or not Check should even run.
// Cancel the provided context to remove the function from the state runner
func (s *StateRunner) AddCondition(ctx context.Context, f func(context.Context) (conditionMet bool, err error)) {
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
	s.AddBeforeFunc(ctx, wrapErrf(ErrConditionFunc, bf))
}

// AddAfterFunc adds a function that is run at the end of the Run. This may be after the Run step failed or succeeded, or after the Check step failed or succeeded,
// or after any of the other Pre functions failed or succceeded.
//
// Cancel the provided context to remove the function from the state runner
func (s *StateRunner) AddAfterFunc(ctx context.Context, f func(ctx context.Context, err error)) {
	ctx, cancel := context.WithCancel(ctx)
	select {
	case s.addAfterFunc <- &afterFunc{ctx, cancel, f}:
	case <-ctx.Done():
	case <-s.ctx.Done():
	}
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
		if err != nil && !errors.Is(err, ErrConditionNotMet) {
			f(ctx, err)
		}
	})
}

var ErrDependancyFailed = errors.New("dependency failed")

func (s *StateRunner) DependOn(ctx context.Context, d *StateRunner) {
	f := wrapErrf(ErrDependancyFailed, d.ApplyOnce)
	s.AddBeforeFunc(ctx, f)
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
	t.AddAfterSuccess(ctx, f)
}

// TODO - add dependencies, dependants requirements ect... use systemd as a model
