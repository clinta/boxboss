// package bossbox includes the Module interface which should be implemented by plugins, and Manager which manages a module and it's hooks
package bossbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime"
	"sync"

	"golang.org/x/sync/errgroup"
)

var ErrCheckFailed = errors.New("check failed")
var ErrApplyFailed = errors.New("apply failed")

// Manager launches a goroutine to accept addition and removal of hooks, and applies the module whenever Manage is called.
// A Manager is safe for concurrent use by multiple goroutines.
type Manager struct {
	module         Module
	lockCh         chan struct{}
	priorityLockWg sync.WaitGroup
	postCheckHooks map[*postCheckHook]struct{}
	preCheckHooks  map[*preCheckHook]struct{}
	postRunHooks   map[*postRunHook]struct{}
	postRunWg      sync.WaitGroup
}

func (s *Manager) LogValue() slog.Value {
	return slog.GroupValue(slog.String("type", reflect.TypeOf(s.module).String()), slog.String("name", s.module.Name()))
}

type ctxKey string

const triggerStackKey ctxKey = "trigger-stack"

const defaultMaxTriggerDepth int = 10
const maxTriggerDepthCtxKey ctxKey = "max-trigger-depth"

const triggerIdCtxKey ctxKey = "trigger"

const moduleCtxKey ctxKey = "module"

// WithMaxTriggerDepth sets the maximum trigger depth allowed in this context.
// If a hook from one Manage() calls another Manage(), this will increase the trigger depth by one.
// Default limit is 10
func WithMaxTriggerDepth(max int) func(context.Context) context.Context {
	return func(ctx context.Context) context.Context {
		return context.WithValue(ctx, maxTriggerDepthCtxKey, max)
	}
}

func maxTriggerDepth(ctx context.Context) int {
	if v, ok := ctx.Value(maxTriggerDepthCtxKey).(int); ok {
		return v
	}
	return defaultMaxTriggerDepth
}

// WithTriggerId will return a context with an identifier for the trigger that is used to trigger
// a Manager.Manage(), useful for logging
func WithTriggerId(trigger slog.LogValuer) func(context.Context) context.Context {
	return func(ctx context.Context) context.Context {
		return context.WithValue(ctx, triggerIdCtxKey, trigger)
	}
}

// withTriggerId will set an ID from the caller, if one does not already exist in the context
func withTriggerId(ctx context.Context) context.Context {
	if _, ok := ctx.Value(triggerIdCtxKey).(slog.LogValuer); ok {
		return ctx
	}
	if _, file, line, ok := runtime.Caller(3); ok {
		c := &caller{file, line}
		return WithTriggerId(c)(ctx)
	}
	u := &unknownTrigger{}
	return WithTriggerId(u)(ctx)
}

func getTriggerStack(ctx context.Context) []slog.LogValuer {
	if a, ok := ctx.Value(triggerStackKey).([]slog.LogValuer); ok {
		return a
	}
	return []slog.LogValuer{}
}

func addTriggerCtx(ctx context.Context, trigger slog.LogValuer) context.Context {
	return context.WithValue(ctx, triggerStackKey, append(getTriggerStack(ctx), trigger))
}

func withTriggerStack(ctx context.Context) context.Context {
	stack := getTriggerStack(ctx)
	trigger, ok := ctx.Value(triggerIdCtxKey).(slog.LogValuer)
	if !ok {
		ctx = withTriggerId(ctx)
		trigger = ctx.Value(triggerIdCtxKey).(slog.LogValuer)
	}
	if len(stack) > 0 && stack[len(stack)-1] == trigger {
		return ctx
	}
	stack = append(stack, trigger)
	return context.WithValue(ctx, triggerStackKey, stack)
}

func (s *Manager) withModule() func(context.Context) context.Context {
	return func(ctx context.Context) context.Context {
		return context.WithValue(ctx, moduleCtxKey, s)
	}
}

type unknownTrigger struct{}

func (u *unknownTrigger) LogValue() slog.Value {
	return slog.StringValue("unknown")
}

type caller struct {
	file string
	line int
}

func (c *caller) LogValue() slog.Value {
	return slog.StringValue(c.file + ":" + fmt.Sprint(c.line))
}

var ErrTriggerDepthExceeded = errors.New("trigger depth exceeded")

// Manage will apply the module.
//
// Multiple request to Manage will be queued.
func (s *Manager) Manage(ctx context.Context, config ...func(context.Context) context.Context) (changed bool, err error) {
	ctx = applyCtxTransforms(ctx, append(config, s.withModule(), withTriggerId, withTriggerStack)...)
	if len(getTriggerStack(ctx)) > maxTriggerDepth(ctx) {
		err := ErrTriggerDepthExceeded
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

// ErrNotRun indicates that the Runner has not been Applied.
var ErrNotRun = errors.New("module has not yet run")

var checkChangesButNoRunChanges = "check indicated changes were required, but run did not report changes"

func (s *Manager) runTrigger(ctx context.Context, log *slog.Logger) (bool, error) {
	runCtx, runDone := context.WithCancel(ctx)
	changed, err := s.runModule(runCtx, log)
	s.postRunWg.Add(1)
	{
		if len(s.postRunHooks) > 0 {
			log.DebugContext(ctx, "running PostRunHooks", "numHooks", len(s.postRunHooks))
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

func (s *Manager) runModule(ctx context.Context, log *slog.Logger) (bool, error) {
	{
		if len(s.preCheckHooks) > 0 {
			log.DebugContext(ctx, "running PreCheckHooks", "numHooks", len(s.preCheckHooks))
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
				return false, nil
			}
			return false, err
		}
	}

	log.DebugContext(ctx, "running check")
	changeNeeded, err := s.module.Check(ctx)
	if err != nil {
		return false, errors.Join(ErrCheckFailed, err)
	}

	{
		if len(s.postCheckHooks) > 0 {
			log.DebugContext(ctx, "running PostCheckHooks", "numHooks", len(s.postCheckHooks))
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
			return false, err
		}
	}

	if !changeNeeded {
		log.DebugContext(ctx, "check indicates no changes required")
		return false, err
	}

	log.DebugContext(ctx, "applying")
	changed, err := s.module.Apply(ctx)
	if err != nil {
		err = errors.Join(ErrApplyFailed, err)
	}

	if !changed && err == nil {
		log.WarnContext(ctx, checkChangesButNoRunChanges)
	}

	return changed, err
}

func (s *Manager) lock(ctx context.Context) (unlock func(), err error) {
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

func (s *Manager) priorityLock(ctx context.Context) (unlock func(), err error) {
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

// Wait will block until any hook addition or removals, or applications are complete
func (s *Manager) Wait(ctx context.Context) error {
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

// WaitAll waits for hook additions or removals, applications, and any currently running post-run hooks
func (s *Manager) WaitAll(ctx context.Context) error {
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
func (s *Manager) checkRemovedHooks(ctx context.Context) bool {
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

// NewManager creates the runner that will run, listening for triggers from Apply until ctx is canceled.
//
// It will run until ctx is canceled. Attempting to use the Manager after context is canceled will likely
// cause deadlocks.
func NewManager(module Module) *Manager {
	s := &Manager{
		module:         module,
		lockCh:         make(chan struct{}, 1),
		priorityLockWg: sync.WaitGroup{},
		postCheckHooks: map[*postCheckHook]struct{}{},
		preCheckHooks:  map[*preCheckHook]struct{}{},
		postRunHooks:   map[*postRunHook]struct{}{},
		postRunWg:      sync.WaitGroup{},
	}
	return s
}
