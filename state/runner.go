// Package state includes the state interface which should be implemented by plugins, and StateRunner which manages a state
package state

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sync"

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

// StateManager launches a goroutine to accept addition and removal of hooks, and applies the state whenever Manage is called.
// A StateManager is safe for concurrent use by multiple goroutines.
type StateManager struct {
	state          StateApplier
	lockCh         chan struct{}
	priorityLockWg sync.WaitGroup
	log            zerolog.Logger
	postCheckHooks map[*postCheckHook]struct{}
	preCheckHooks  map[*preCheckHook]struct{}
	postRunHooks   map[*postRunHook]struct{}
	postRunWg      sync.WaitGroup
}

func (s *StateManager) MarshalZerologObject(e *zerolog.Event) {
	e.Type("type", s.state).Str("name", s.state.Name())
}

type stateCtxKey string

const triggerStackKey stateCtxKey = "trigger-stack"

const defaultMaxTriggerDepth int = 10
const maxTriggerDepthKey stateCtxKey = "max-trigger-depth"

const triggerCtxKey stateCtxKey = "trigger-ctx"

func getTriggerStack(ctx context.Context) []zerolog.LogObjectMarshaler {
	if a, ok := ctx.Value(triggerStackKey).([]zerolog.LogObjectMarshaler); ok {
		return a
	}
	return []zerolog.LogObjectMarshaler{}
}

// SetMaxTriggerDepth sets the maximum trigger depth allowed in this context.
// If a hook from one Manage() calls another Manage(), this will increase the trigger depth by one.
// Default limit is 10
func SetMaxTriggerDepth(ctx context.Context, max int) context.Context {
	return context.WithValue(ctx, maxTriggerDepthKey, max)
}

func maxTriggerDepth(ctx context.Context) int {
	if v, ok := ctx.Value(maxTriggerDepthKey).(int); ok {
		return v
	}
	return defaultMaxTriggerDepth
}

// WithTriggerCtx will return a context with an identifier for the trigger that is used to trigger
// a StateManager.Manage(), useful for logging state calls
func WithTriggerCtx(ctx context.Context, trigger zerolog.LogObjectMarshaler) context.Context {
	return context.WithValue(ctx, triggerCtxKey, trigger)
}

func addTriggerCtx(ctx context.Context, trigger zerolog.LogObjectMarshaler) context.Context {
	return context.WithValue(ctx, triggerStackKey, append(getTriggerStack(ctx), trigger))
}

type callerId string

func (c callerId) MarshalZerologObject(e *zerolog.Event) {
	e.Str("caller", string(c))
}

type unknownTrigger struct{}

func (t unknownTrigger) MarshalZerologObject(e *zerolog.Event) {
	e.Str("trigger", "unkown")
}

func triggerCtx(ctx context.Context) (context.Context, zerolog.LogObjectMarshaler) {
	if v, ok := ctx.Value(triggerCtxKey).(zerolog.LogObjectMarshaler); ok {
		return addTriggerCtx(ctx, v), v
	}
	hook, ok := ctx.Value(hookCtxKey).(zerolog.LogObjectMarshaler)
	if ok && hook != nil {
		return addTriggerCtx(ctx, hook), hook
	}
	if pc, file, line, ok := runtime.Caller(3); ok {
		// TODO: Test this!
		id := callerId(zerolog.CallerMarshalFunc(pc, file, line))
		return addTriggerCtx(ctx, id), id
	}
	return addTriggerCtx(ctx, unknownTrigger{}), unknownTrigger{}
}

var ErrTriggerDepthExceeded = errors.New("trigger depth exceeded")

// Manage will apply the state.
//
// Multiple request to Manage will be queued.
func (s *StateManager) Manage(ctx context.Context) (changed bool, err error) {
	ctx, trigger := triggerCtx(ctx)
	log := s.log.With().Object("trigger", trigger).Logger()
	if len(getTriggerStack(ctx)) > maxTriggerDepth(ctx) {
		err := ErrTriggerDepthExceeded
		log.Error().Err(err).Msg("")
		return false, err
	}
	if err := s.Wait(ctx); err != nil {
		return false, err
	}
	unlock, err := s.lock(ctx)
	defer unlock()
	if err != nil {
		return false, err
	}
	return s.runTrigger(ctx, log)
}

// ErrStateNotRun indicates that the Runner has not been Applied.
var ErrStateNotRun = errors.New("state has not yet run")

var checkChangesButNoRunChanges = "check indicated changes were required, but run did not report changes"

func (s *StateManager) runTrigger(ctx context.Context, log zerolog.Logger) (bool, error) {
	runCtx, runDone := context.WithCancel(ctx)
	changed, err := s.runState(runCtx, log)
	s.postRunWg.Add(1)
	{
		if len(s.postRunHooks) > 0 {
			log.Debug().Int("numHooks", len(s.postRunHooks)).Msg("running PostRunHooks")
		}
		wg := sync.WaitGroup{}
		for h := range s.postRunHooks {
			wg.Add(1)
			go func(f *postRunHook) {
				f.f(ctx, changed, err)
				wg.Done()
			}(h)
		}
		go func() {
			wg.Wait()
			runDone()
			s.postRunWg.Done()
		}()
	}
	return changed, err
}

func (s *StateManager) runState(ctx context.Context, log zerolog.Logger) (bool, error) {
	{
		if len(s.preCheckHooks) > 0 {
			log.Debug().Int("numHooks", len(s.preCheckHooks)).Msg("running PreCheckHooks")
		}
		eg, egCtx := errgroup.WithContext(ctx)
		for h := range s.preCheckHooks {
			h := h
			eg.Go(func() error {
				return h.f(egCtx)
			})
		}
		err := eg.Wait()
		if err != nil {
			if errors.Is(err, ErrConditionNotMet) {
				log.Debug().Err(err).Msg("condition not met")
				return false, nil
			}
			log.Error().Err(err).Msg("PreCheckHook failed")
			return false, err
		}
	}

	log.Debug().Msg("running check")
	changeNeeded, err := s.state.Check(ctx)
	if err != nil {
		log.Error().Err(err).Msg("check error")
		return false, errors.Join(ErrCheckFailed, err)
	}

	{
		if len(s.postCheckHooks) > 0 {
			log.Debug().Int("numHooks", len(s.postCheckHooks)).Msg("running PostCheckHooks")
		}
		eg, egCtx := errgroup.WithContext(ctx)
		for h := range s.postCheckHooks {
			h := h
			eg.Go(func() error {
				return h.f(egCtx, changeNeeded)
			})
		}
		err := eg.Wait()

		if err != nil {
			log.Error().Err(err).Msg("PostCheckHook failed")
			return false, err
		}
	}

	if !changeNeeded {
		log.Debug().Msg("check indicates no changes required")
		return false, err
	}

	log.Debug().Msg("running")
	changed, err := s.state.Apply(ctx)
	err = wrapErr(ErrRunFailed, err)

	if !changed {
		log.Warn().Msg(checkChangesButNoRunChanges)
	}

	log = log.With().Bool("changed", changed).Logger()
	if err != nil {
		log.Error().Err(err).Msg("run failed")
	}

	return changed, err
}

func (s *StateManager) lock(ctx context.Context) (unlock func(), err error) {
	s.priorityLockWg.Wait()
	select {
	case s.lockCh <- struct{}{}:
		ctx, cancel := context.WithCancel(ctx)
		context.AfterFunc(ctx, func() { <-s.lockCh })
		return cancel, nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}

func (s *StateManager) priorityLock(ctx context.Context) (unlock func(), err error) {
	s.priorityLockWg.Add(1)
	defer s.priorityLockWg.Done()
	select {
	case s.lockCh <- struct{}{}:
		ctx, cancel := context.WithCancel(ctx)
		context.AfterFunc(ctx, func() { <-s.lockCh })
		return cancel, nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}

// Wait will block until any hook addition or removals, or state applications are complete
func (s *StateManager) Wait(ctx context.Context) error {
	unlock, err := s.lock(ctx)
	if err != nil {
		unlock()
		return err
	}
	for s.checkRemovedHooks(ctx) {
		unlock()
		unlock, err = s.lock(ctx)
		if err != nil {
			unlock()
			return err
		}
	}
	unlock()
	return nil
}

// WaitAll waits for hook additions or removals, state applications, and any currently running post-run hooks
func (s *StateManager) WaitAll(ctx context.Context) error {
	unlock, err := s.lock(ctx)
	defer unlock()
	if err != nil {
		return err
	}

	for s.checkRemovedHooks(ctx) {
		unlock()
		unlock, err = s.lock(ctx)
		if err != nil {
			unlock()
			return err
		}
	}
	defer unlock()

	c := make(chan struct{})
	go func() {
		s.postRunWg.Wait()
		close(c)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c:
		return nil
	}
}

// outer func needs to lock before calling this
func (s *StateManager) checkRemovedHooks(ctx context.Context) bool {
	for h := range s.preCheckHooks {
		select {
		case <-h.ctx.Done():
			return true
		default:
		}
	}
	for h := range s.postCheckHooks {
		select {
		case <-h.ctx.Done():
			return true
		default:
		}
	}
	for h := range s.postRunHooks {
		select {
		case <-h.ctx.Done():
			return true
		default:
		}
	}
	return false
}

// NewStateManager creates the state runner that will run, listening for triggers from Apply until ctx is canceled.
//
// It will run until ctx is canceled. Attempting to use the StateRunner after context is canceled will likely
// cause deadlocks.
func NewStateManager(state StateApplier) *StateManager {
	s := &StateManager{
		state:          state,
		lockCh:         make(chan struct{}, 1),
		priorityLockWg: sync.WaitGroup{},
		log:            log.Logger,
		postCheckHooks: map[*postCheckHook]struct{}{},
		preCheckHooks:  map[*preCheckHook]struct{}{},
		postRunHooks:   map[*postRunHook]struct{}{},
		postRunWg:      sync.WaitGroup{},
	}
	s.log = log.With().Object("module", s).Logger()
	return s
}
