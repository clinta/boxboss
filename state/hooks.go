package state

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"runtime"
)

type hook struct {
	ctx context.Context
}

const hookCtxKey stateCtxKey = "hook"

type hookCtx struct {
	hookType string
	hookName string
}

func (h *hookCtx) LogValuer() slog.Value {
	return slog.GroupValue(slog.String("type", h.hookType), slog.String("name", h.hookName))
}

var nullHandler *slog.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
	Level: slog.Level(math.MaxInt),
},
))

// makeHookLog adds a new hook log object to ctx, and returns ctx and the logger
// If ctx already has a hookCtx, ctx will be unchanged, and a Nop logger will be returned
// This ensures the returned logger is only operable if the hook is not wrapped by a higher level hook
func (s *StateManager) makeHookLog(ctx context.Context, hookType string) (context.Context, *slog.Logger) {
	if _, ok := ctx.Value(hookCtxKey).(slog.LogValuer); ok {
		return ctx, nullHandler
	}
	h := &hookCtx{
		hookType: hookType,
		hookName: "<unknown>",
	}
	if s, ok := ctx.Value(hookName).(string); ok {
		h.hookName = s
	} else if _, file, line, ok := runtime.Caller(2); ok {
		h.hookName = file + ":" + fmt.Sprint(line)
	}
	ctx = context.WithValue(ctx, hookCtxKey, h)
	log := s.log.With(hookCtxKey, h)
	return ctx, log
}

func (s *StateManager) getHookLog(ctx context.Context) *slog.Logger {
	v := ctx.Value(hookCtxKey)
	return s.log.With(hookCtxKey, v)
}

const hookName stateCtxKey = "hook-name"

type preCheckHook struct {
	hook
	f func(ctx context.Context) error
}

type postCheckHook struct {
	hook
	f func(ctx context.Context, changeNeeded bool) error
}

type postRunHook struct {
	hook
	f func(ctx context.Context, changed bool, err error)
}

func wrapErr(parent error, child error) error {
	if child == nil {
		return nil
	}
	return errors.Join(parent, child)
}

func logAndWrapHookErr(err error, parentErr error, log *slog.Logger) error {
	if err == nil {
		log.Debug("hook complete")
		return nil
	}
	log.Error("hook complete", "err", err)
	return errors.Join(parentErr, err)
}

// ErrPreCheckHook is returned if a PreCheckHook errors. It will wrap underlying errors.
var ErrPreCheckHook = errors.New("PreCheck hook error")

// AddPreCheckHook adds a function that is run before the Check step of the State.
//
// An error in this hook will propegate to the Apply and prevent Check or Run from running, except for ErrConditionNotMet.
//
// If multiple PreCheckHooks are specified, they will run concurrently, and an error from any will cancel the contexts of the
// remaining PreCheckHooks. The first error will be returned.
func (s *StateManager) AddPreCheckHook(ctx context.Context, f func(context.Context) error) error {
	ctx, runLog := s.makeHookLog(ctx, "PreCheckHook")
	log := s.getHookLog(ctx)
	unlock, err := s.priorityLock(ctx)
	defer unlock()
	if err != nil {
		return err
	}
	hf := func(ctx context.Context) error {
		runLog.Debug("running hook")
		err := f(ctx)
		if err != nil {
			runLog.Error("hook complete", "error", err)
			return errors.Join(ErrPreCheckHook, err)
		}
		runLog.Debug("hook complete")
		return nil
	}
	h := &preCheckHook{hook{ctx}, hf}
	s.preCheckHooks[h] = struct{}{}
	log.Debug("added hook")
	context.AfterFunc(ctx, func() {
		unlock, _ = s.priorityLock(context.Background())
		defer unlock()
		delete(s.preCheckHooks, h)
		log.Debug("removed hook")
	})
	return nil
}

// ErrConditionNotMet signals that a precheck condition was not met and the state should not run,
// but did not error in an unexpected way.
//
// This error is not propegated to Apply, Apply will return nil if a ConditionNotMet is returned from a PreCheckHook.
var ErrConditionNotMet = errors.New("condition not met")

// ErrCondition is returned by the state when a Condition errors.
//
// This will wrap the actual error returned by the condition, and will itself be wrapped in an ErrPreCheckHook, because
// a condition is a PreCheckHook.
var ErrCondition = errors.New("condition hook error")

// AddCondition adds a function that is a condition to determine whether or not Check should run.
//
// If conditionMet returns false, any concurrently running PreCheckHook contexts will be canceled, and the State will
// not be applied. But Apply will not return an error.
//
// AddCondition returns a function that can be used to remove the hook.
func (s *StateManager) AddCondition(ctx context.Context, f func(context.Context) (conditionMet bool, err error)) error {
	ctx, log := s.makeHookLog(ctx, "Condition")
	cf := func(ctx context.Context) error {
		log.Debug("running hook")
		v, err := f(ctx)
		if err == nil && !v {
			log.Debug("condition not met")
			return ErrConditionNotMet
		}
		return logAndWrapHookErr(err, ErrCondition, log)
	}
	return s.AddPreCheckHook(ctx, cf)
}

// ErrPostCheckHook is returned if a PostCheckHook errors causing the Apply to error
var ErrPostCheckHook = errors.New("PostCheck hook error")

// AddPostCheckHook adds a function that is run after the Check step of the State.
//
// This is useful for actions that need to be run in preperation of a state. For example a file management state may neeed
// A service to be stopped before the file can be managed.
//
// An error in this hook will propegate to the Apply and prevent Run from running.
//
// If multiple PostCheckHooks are specified, they will run concurrently, and an error from any will cancel the contexts of the
// remaining PostCheckHooks. The first error will be returned.
//
// AddPostCheckHook returns a function that can be used to remove the hook.
func (s *StateManager) AddPostCheckHook(ctx context.Context, f func(ctx context.Context, changeNeeded bool) error) error {
	ctx, runLog := s.makeHookLog(ctx, "PostCheckHook")
	log := s.getHookLog(ctx)
	unlock, err := s.priorityLock(ctx)
	defer unlock()
	if err != nil {
		return err
	}
	h := &postCheckHook{hook{ctx},
		func(ctx context.Context, changeNeeded bool) error {
			runLog := runLog.With("changeNeeded", changeNeeded)
			runLog.Debug("running hook")
			err := f(ctx, changeNeeded)
			return logAndWrapHookErr(err, ErrPostCheckHook, log)
		}}
	s.postCheckHooks[h] = struct{}{}
	log.Debug("added hook")
	context.AfterFunc(ctx, func() {
		unlock, _ = s.priorityLock(context.Background())
		defer unlock()
		delete(s.postCheckHooks, h)
		log.Debug("removed hook")
	})
	return nil
}

var ErrChangesRequiredFailed = errors.New("changes-required hook error")

// AddChangesRequiredHook adds a function that is run after the check step, only if changes are required.
func (s *StateManager) AddChangesRequiredHook(ctx context.Context, f func(context.Context) error) error {
	ctx, log := s.makeHookLog(ctx, "ChangesRequired")
	return s.AddPostCheckHook(ctx, func(ctx context.Context, changeNeeded bool) error {
		if changeNeeded {
			log.Debug("running hook")
			err := f(ctx)
			return logAndWrapHookErr(err, ErrChangesRequiredFailed, log)
		}
		return nil
	})
}

// AddPostRunHook adds a function that is run at the end of the state execution. This will run regardless of the source
// of any errors. The function is responsible for checking the type of error.
//
// PostRunHooks do not block returning the StateContext result. This means that a subsequent state run could run the PostRunHook before the previous one finished.
//
// AddPostRunHook returns a function that can be used to remove the hook.
func (s *StateManager) AddPostRunHook(ctx context.Context, f func(ctx context.Context, changed bool, err error)) error {
	ctx, runLog := s.makeHookLog(ctx, "PostRunHook")
	log := s.getHookLog(ctx)
	unlock, err := s.priorityLock(ctx)
	defer unlock()
	if err != nil {
		return err
	}
	h := &postRunHook{hook{ctx}, func(ctx context.Context, changed bool, err error) {
		runLog := runLog.With("changed", changed)
		if err != nil {
			runLog = runLog.With("applyErr", err)
		}
		runLog.Debug("running hook")
		f(ctx, changed, err)
		runLog.Debug("hook complete")
	}}
	s.postRunHooks[h] = struct{}{}
	log.Debug("added hook")
	context.AfterFunc(ctx, func() {
		unlock, _ = s.priorityLock(context.Background())
		defer unlock()
		delete(s.postRunHooks, h)
		log.Debug("removed hook")
	})
	return nil
}

// AddPostSuccessHook adds a PostRunHook that is run after a successful state run.
func (s *StateManager) AddPostSuccessHook(ctx context.Context, f func(ctx context.Context, changes bool)) error {
	ctx, log := s.makeHookLog(ctx, "PostSuccess")
	return s.AddPostRunHook(ctx, func(ctx context.Context, changes bool, err error) {
		if err == nil {
			log.Debug("running hook")
			f(ctx, changes)
			log.Debug("hook complete")
		}
	})
}

// AddPostErrorHook adds a PostRunHook that is run after a state run errors.
func (s *StateManager) AddPostErrorHook(ctx context.Context, f func(ctx context.Context, err error)) error {
	ctx, log := s.makeHookLog(ctx, "PostFailure")
	return s.AddPostRunHook(ctx, func(ctx context.Context, changes bool, err error) {
		if err != nil && !errors.Is(err, ErrConditionNotMet) {
			log.Debug("running hook")
			f(ctx, err)
			log.Debug("hook complete")
		}
	})
}

// AddPostChangesHook adds a PostRunHook that is run after a successful state run that made changes.
func (s *StateManager) AddPostChangesHook(ctx context.Context, f func(ctx context.Context)) error {
	ctx, log := s.makeHookLog(ctx, "PostChanges")
	return s.AddPostSuccessHook(ctx, func(ctx context.Context, changes bool) {
		if changes {
			log.Debug("running hook")
			f(ctx)
			log.Debug("hook complete")
		}
	})
}

type moduleHookCtx struct {
	hookType string
	module   *StateManager
}

// makeHookLog adds a new hook log object to ctx, and returns ctx and the logger
// If ctx already has a hookCtx, ctx will be unchanged, and a Nop logger will be returned
// This ensures the returned logger is only operable if the hook is not wrapped by a higher level hook
func (s *StateManager) makeModuleHookLog(ctx context.Context, hookType string, r *StateManager) (context.Context, *slog.Logger) {
	if _, ok := ctx.Value(hookCtxKey).(slog.LogValuer); ok {
		return ctx, nullHandler
	}
	h := &moduleHookCtx{
		hookType: hookType,
		module:   r,
	}
	ctx = context.WithValue(ctx, hookCtxKey, h)
	log := s.log.With(hookCtxKey, h)
	return ctx, log
}

func (r *StateManager) logModuleApply(ctx context.Context, log *slog.Logger) (changes bool, err error) {
	log.Debug("starting hook")
	changes, err = r.Manage(ctx)
	log = log.With("changes", changes)
	if err != nil {
		log.Error("hook complete", "err", err)
	} else {
		log.Debug("hook complete")
	}
	return changes, err
}

// Require sets r as a requirement that must be successful before s can be applied
func (s *StateManager) Require(ctx context.Context, r *StateManager) error {
	ctx, log := r.makeModuleHookLog(ctx, "Require", r)
	return s.AddPreCheckHook(ctx, func(ctx context.Context) error {
		_, err := r.logModuleApply(ctx, log)
		return err
	})
}

// RequireChanges sets r as a condition and only runs s if r made changes
func (s *StateManager) RequireChanges(ctx context.Context, r *StateManager) error {
	ctx, log := r.makeModuleHookLog(ctx, "RequireChanges", r)
	return s.AddCondition(ctx, func(ctx context.Context) (bool, error) {
		changes, err := r.logModuleApply(ctx, log)
		return changes, err
	})
}

// ChangesRequire requires r as a requirement that runs only if changes are indicated by s.Check
func (s *StateManager) ChangesRequire(ctx context.Context, r *StateManager) error {
	ctx, log := r.makeModuleHookLog(ctx, "ChangesRequire", r)
	return s.AddChangesRequiredHook(ctx,
		func(ctx context.Context) error {
			_, err := r.logModuleApply(ctx, log)
			return err
		})
}

// Triggers triggers r anytime s is run (regardless of success or changes)
func (s *StateManager) Triggers(ctx context.Context, r *StateManager) error {
	ctx, log := r.makeModuleHookLog(ctx, "Triggers", r)
	return s.AddPostRunHook(ctx, func(ctx context.Context, _ bool, _ error) {
		_, _ = r.logModuleApply(ctx, log)
	})
}

// SuccessTriggers triggers r when s.Apply is successful
func (s *StateManager) SuccessTriggers(ctx context.Context, r *StateManager) error {
	ctx, log := r.makeModuleHookLog(ctx, "SuccessTriggers", r)
	return s.AddPostSuccessHook(ctx, func(ctx context.Context, _ bool) {
		_, _ = r.logModuleApply(ctx, log)
	})
}

// ChangesTriggers triggers r anytime s successfully makes changes
func (s *StateManager) ChangesTriggers(ctx context.Context, r *StateManager) error {
	ctx, log := r.makeModuleHookLog(ctx, "ChangesTrigger", r)
	return s.AddPostChangesHook(ctx, func(ctx context.Context) {
		_, _ = r.logModuleApply(ctx, log)
	})
}

// ErrorTriggers triggers r anytime s errors
func (s *StateManager) ErrorTriggers(ctx context.Context, r *StateManager) error {
	ctx, log := r.makeModuleHookLog(ctx, "FailureTrigger", r)
	return s.AddPostErrorHook(ctx, func(ctx context.Context, _ error) {
		_, _ = r.logModuleApply(ctx, log)
	})
}

/*
// conflictsWith prevents s and r from running at the same time
func (s *StateManager) conflictsWith(ctx context.Context, r *StateManager) error {
	ctx, log := s.hookedModuleLog(ctx, r, "conflicts-with")
	return s.AddPreCheckHook(ctx, func(ctx context.Context) error {
		log.Debug().Msg("waiting on conflicting module")
		err := r.Wait(ctx)
		log.Debug().Msg("done waiting on conflicting module")
		return err
	})
}

// ConflictsWith prevents s and r from running at the same time
func (s *StateManager) ConflictsWith(ctx context.Context, r *StateManager) error {
	// Note: This should not race
	// while s is waiting on r, s is already locked, so r cannot start again after r finishes until s finishes
	// TODO: This could deadlock, what to do?
	err := s.conflictsWith(ctx, r)
	if err != nil {
		return err
	}
	return r.conflictsWith(ctx, s)
}
*/
