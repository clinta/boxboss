// Package state includes the state interface which should be implemented by plugins, and StateRunner which manages a state
package state

import (
	"context"
	"errors"
	"os"
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

// Manage will apply the state.
//
// Multiple request to Manage will be queued.
func (s *StateManager) Manage(ctx context.Context) (changed bool, err error) {
	if err := s.Wait(ctx); err != nil {
		return false, err
	}
	unlock, err := s.lock(ctx)
	defer unlock()
	if err != nil {
		return false, err
	}
	return s.runTrigger(ctx)
}

// ErrStateNotRun indicates that the Runner has not been Applied.
var ErrStateNotRun = errors.New("state has not yet run")

var checkChangesButNoRunChanges = "check indicated changes were required, but run did not report changes"

func (s *StateManager) runTrigger(ctx context.Context) (bool, error) {
	runCtx, runDone := context.WithCancel(ctx)
	changed, err := s.runState(runCtx)
	s.postRunWg.Add(1)
	{
		wg := sync.WaitGroup{}
		for h := range s.postRunHooks {
			wg.Add(1)
			go func(f *postRunHook) {
				//log := log.With().Str("post-run hook", f.name).Logger()
				log.Debug().Msg("running post-run hook")
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

func (s *StateManager) runState(ctx context.Context) (bool, error) {
	log := s.log

	{
		eg, egCtx := errgroup.WithContext(ctx)
		for h := range s.preCheckHooks {
			h := h
			eg.Go(func() error {
				//log := log.With().Str("pre-check hook", h.name).Logger()
				log.Debug().Msg("running pre-check hook")
				return h.f(egCtx)
			})
		}
		err := eg.Wait()
		if err != nil {
			if errors.Is(err, ErrConditionNotMet) {
				log := log.With().Err(err).Logger()
				log.Debug().Msg("condition not met")
				return false, nil
			}
			log.Error().Err(err).Msg("pre-check hook failed")
			return false, err
		}
	}

	log.Debug().Msg("running check")
	changeNeeded, err := s.state.Check(ctx)
	if err != nil {
		log.Error().Err(err).Msg("check failed")
		return false, errors.Join(ErrCheckFailed, err)
	}

	{
		eg, egCtx := errgroup.WithContext(ctx)
		for h := range s.postCheckHooks {
			h := h
			eg.Go(func() error {
				//log := log.With().Str("post-check hook", h.name).Logger()
				log.Debug().Msg("running post-check hook")
				err := h.f(egCtx, changeNeeded)
				return err
			})
		}
		err := eg.Wait()

		if err != nil {
			log.Error().Err(err).Msg("post-check hook failed")
			return false, err
		}
	}

	err = wrapErr(ErrCheckFailed, err)
	if err != nil {
		log.Error().Err(err).Msg("check failed")
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
	log := log.With().Str("stateRunner", state.Name()).Logger()
	s := &StateManager{
		state:          state,
		lockCh:         make(chan struct{}, 1),
		priorityLockWg: sync.WaitGroup{},
		log:            log,
		postCheckHooks: map[*postCheckHook]struct{}{},
		preCheckHooks:  map[*preCheckHook]struct{}{},
		postRunHooks:   map[*postRunHook]struct{}{},
		postRunWg:      sync.WaitGroup{},
	}
	return s
}
