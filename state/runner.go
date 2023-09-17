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

// StateRunner runs the state whenever a trigger is received
type StateRunner struct {
	state         State
	ctx           context.Context
	log           zerolog.Logger
	trigger       chan context.Context
	getLastResult chan chan<- *StateRunResult
	lastResult    *StateRunResult
	hookMgr       *hookMgr
}

// StateRunResult holds the result of a state run
type StateRunResult struct {
	changed bool
	time    time.Time
	err     error
}

func (r *StateRunResult) Changed() bool {
	return r.changed
}

func (r *StateRunResult) Completed() time.Time {
	return r.time
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
	if !errors.Is(res.Err(), ErrStateNotRun) {
		return res.err
	}
	return s.Apply(ctx)
}

// ErrStateNotRun is the error in the StateResult of a StateRunner that has not run yet
var ErrStateNotRun = errors.New("state has not yet run")

var checkChangesButNoRunChanges = "check indicated changes were required, but run did not report changes"

// manage is the function that manages the state, runnning whenever a trigger is recieved
//
// # Provided context can be used to stop
//
// TODO: Add rate limiting
func (s *StateRunner) manage() {
	log := s.log

	for {
		select {
		case <-s.ctx.Done():
			log.Debug().Msg("shutting down runner")
			return
		case resCh := <-s.getLastResult:
			resCh <- s.lastResult
		case f := <-s.hookMgr.hookOp:
			f()
		case tCtx := <-s.trigger:
			log.Debug().Msg("triggered")
			s.runTrigger(tCtx)
		}
	}
}

func (s *StateRunner) runTrigger(triggerCtx context.Context) {
	// triggerCtx will be Done if all trigger contexts are canceled
	triggersCtx, triggersCancel := context.WithCancel(s.ctx)
	triggersWg := sync.WaitGroup{}

	addTrigger := func(tCtx context.Context) {
		triggersWg.Add(1)
		go func() {
			select {
			case <-tCtx.Done():
			case <-triggersCtx.Done():
			}
			triggersWg.Done()
		}()
	}

	addTrigger(triggerCtx)

	// collect all pending triggers and hook operations before continuing
	for {
		select {
		case <-triggersCtx.Done():
			triggersCancel()
			return
		case tCtx := <-s.trigger:
			addTrigger(tCtx)
			continue
		case f := <-s.hookMgr.hookOp:
			f()
			continue
		default:
		}
		break
	}

	go func() {
		// cancel triggerCtx if all triggers contexts are canceled
		triggersWg.Wait()
		triggersCancel()
	}()

	s.runState(triggersCtx)
	triggersCancel()
	triggersWg.Wait()

	// Return any waiting results before listening for new trigggers
	for {
		select {
		case resCh := <-s.getLastResult:
			resCh <- s.lastResult
			continue
		default:
		}
		break
	}
}

func (s *StateRunner) runState(ctx context.Context) {
	s.lastResult = &StateRunResult{false, time.Now(), s.lastResult.err}
	log := s.log

	{
		eg, egCtx := errgroup.WithContext(ctx)
		for h := range s.hookMgr.beforeCheckHooks {
			h := h
			eg.Go(func() error {
				log := log.With().Str("beforeCheckHook", h.name).Logger()
				log.Debug().Msg("running beforeCheckHook")
				return h.f(egCtx)
			})
		}
		err := eg.Wait()
		if err != nil {
			if errors.Is(err, ErrConditionNotMet) {
				log := log.With().Err(err).Logger()
				log.Debug().Msg("condition not met")
				return
			}
			log.Error().Err(err).Msg("before hook failed")
			s.lastResult = &StateRunResult{false, time.Now(), err}
			return
		}
	}

	log.Debug().Msg("running check")
	changeNeeded, err := s.state.Check(ctx)
	if err != nil {
		log.Error().Err(err).Msg("check failed")
		s.lastResult = &StateRunResult{false, time.Now(), errors.Join(ErrCheckFailed, err)}
		return
	}

	{
		eg, egCtx := errgroup.WithContext(ctx)
		for h := range s.hookMgr.afterCheckHooks {
			h := h
			eg.Go(func() error {
				log := log.With().Str("afterCheckHook", h.name).Logger()
				log.Debug().Msg("running afterCheckHook")
				err := h.f(egCtx, changeNeeded, err)
				return err
			})
		}
		err := eg.Wait()

		if err != nil {
			log.Error().Err(err).Msg("after check hook failed")
			s.lastResult = &StateRunResult{false, time.Now(), err}
			return
		}
	}

	err = wrapErr(ErrCheckFailed, err)
	if err != nil {
		log.Error().Err(err).Msg("check failed")
	}

	if !changeNeeded {
		log.Debug().Msg("check indicates no changes required")
		s.lastResult = &StateRunResult{false, time.Now(), err}
		return
	}

	log.Debug().Msg("running")
	changed, err := s.state.Run(ctx)
	err = wrapErr(ErrRunFailed, err)

	if !changed {
		log.Warn().Msg(checkChangesButNoRunChanges)
	}

	log = log.With().Bool("changed", changed).Logger()
	if err != nil {
		log.Error().Err(err).Msg("run failed")
	}

	s.lastResult = &StateRunResult{changed, time.Now(), err}

	{
		for h := range s.hookMgr.afterRunHooks {
			go func(f *afterRunHook) {
				log := log.With().Str("afterHook", f.name).Logger()
				log.Debug().Msg("running afterHook")
				f.f(ctx, err)
			}(h)
		}
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
	log := log.With().Str("stateRunner", state.Name()).Logger()
	s := &StateRunner{
		state:         state,
		ctx:           ctx,
		log:           log,
		trigger:       make(chan context.Context),
		getLastResult: make(chan chan<- *StateRunResult),
		lastResult:    &StateRunResult{false, time.Time{}, ErrStateNotRun},
		hookMgr:       newHookMgr(ctx, log),
	}
	go s.manage()
	return s
}