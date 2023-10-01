package state

import (
	"context"
	"errors"
	"runtime"

	"github.com/rs/zerolog"
)

// TODO: Use contexts for names, add loggers

type hook struct {
	ctx context.Context
}

type stateCtxKey string

const hookCtxKey = "hook"

type hookCtx struct {
	hookType string
	hookName string
}

func (h *hookCtx) MarshalZerologObject(e *zerolog.Event) {
	e.Str("type", h.hookType).Str("name", h.hookName)
}

// makeHookLog adds a new hook log object to ctx, and returns ctx and the logger
// If ctx already has a hookCtx, ctx will be unchanged, and a Nop logger will be returned
// This ensures the returned logger is only operable if the hook is not wrapped by a higher level hook
func (s *StateManager) makeHookLog(ctx context.Context, hookType string) (context.Context, zerolog.Logger) {
	if _, ok := ctx.Value(hookCtxKey).(zerolog.LogObjectMarshaler); ok {
		return ctx, zerolog.Nop()
	}
	h := &hookCtx{
		hookType: hookType,
		hookName: "<unknown>",
	}
	if s, ok := ctx.Value(hookName).(string); ok {
		h.hookName = s
	} else if pc, file, line, ok := runtime.Caller(3); ok {
		h.hookName = zerolog.CallerMarshalFunc(pc, file, line)
	}
	ctx = context.WithValue(ctx, hookCtxKey, h)
	log := s.log.With().Object(hookCtxKey, h).Logger()
	return ctx, log
}

func (s *StateManager) getHookLog(ctx context.Context) zerolog.Logger {
	v := ctx.Value(hookCtxKey)
	if h, ok := v.(zerolog.LogObjectMarshaler); ok {
		return s.log.With().Object(hookCtxKey, h).Logger()
	}
	return s.log.With().Any(hookCtxKey, v).Logger()
}

const hookName stateCtxKey = "hook-name"

const hookWrapped stateCtxKey = "hook-wrapped"

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

func logAndWrapHookErr(err error, parentErr error, log zerolog.Logger) error {
	if err == nil {
		log.Debug().Msg("hook complete")
		return nil
	}
	log.Error().Err(err).Msg("hook complete")
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
		runLog.Debug().Msg("running hook")
		err := f(ctx)
		if err != nil && !errors.Is(err, ErrConditionNotMet) {
			runLog.Error().Err(err).Msg("hook complete")
			return errors.Join(ErrPreCheckHook, err)
		}
		if errors.Is(err, ErrConditionNotMet) {
			runLog.Debug().Err(err).Msg("hook complete")
			return errors.Join(ErrPreCheckHook, err)
		}
		runLog.Debug().Msg("hook complete")
		return nil
	}
	h := &preCheckHook{hook{ctx}, hf}
	s.preCheckHooks[h] = struct{}{}
	log.Debug().Msg("added hook")
	context.AfterFunc(ctx, func() {
		unlock, _ = s.priorityLock(context.Background())
		defer unlock()
		delete(s.preCheckHooks, h)
		log.Debug().Msg("removed hook")
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
		log.Debug().Msg("running hook")
		v, err := f(ctx)
		if err == nil && !v {
			log.Debug().Msg("condition not met")
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
			runLog := runLog.With().Bool("changeNeeded", changeNeeded).Logger()
			runLog.Debug().Msg("running hook")
			err := f(ctx, changeNeeded)
			return logAndWrapHookErr(err, ErrPostCheckHook, log)
		}}
	s.postCheckHooks[h] = struct{}{}
	log.Debug().Msg("added hook")
	context.AfterFunc(ctx, func() {
		unlock, _ = s.priorityLock(context.Background())
		defer unlock()
		delete(s.postCheckHooks, h)
		log.Debug().Msg("removed hook")
	})
	return nil
}

var ErrChangesRequiredFailed = errors.New("changes-required hook error")

// AddChangesRequiredHook adds a function that is run after the check step, only if changes are required.
func (s *StateManager) AddChangesRequiredHook(ctx context.Context, f func(context.Context) error) error {
	ctx, log := s.makeHookLog(ctx, "ChangesRequired")
	return s.AddPostCheckHook(ctx, func(ctx context.Context, changeNeeded bool) error {
		if changeNeeded {
			log.Debug().Msg("running hook")
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
		runLog := runLog.With().Bool("changed", changed).AnErr("moduleErr", err).Logger()
		runLog.Debug().Msg("running hook")
		f(ctx, changed, err)
		runLog.Debug().Msg("hook complete")
	}}
	s.postRunHooks[h] = struct{}{}
	log.Debug().Msg("added hook")
	context.AfterFunc(ctx, func() {
		unlock, _ = s.priorityLock(context.Background())
		defer unlock()
		delete(s.postRunHooks, h)
		log.Debug().Msg("removed hook")
	})
	return nil
}

// AddPostSuccessHook adds a PostRunHook that is run after a successful state run.
func (s *StateManager) AddPostSuccessHook(ctx context.Context, f func(ctx context.Context, changes bool)) error {
	ctx, log := s.makeHookLog(ctx, "PostSuccess")
	return s.AddPostRunHook(ctx, func(ctx context.Context, changes bool, err error) {
		if err == nil {
			log.Debug().Msg("running hook")
			f(ctx, changes)
			log.Debug().Msg("hook complete")
		}
	})
}

// AddPostErrorHook adds a PostRunHook that is run after a state run errors.
func (s *StateManager) AddPostErrorHook(ctx context.Context, f func(ctx context.Context, err error)) error {
	ctx, log := s.makeHookLog(ctx, "PostFailure")
	return s.AddPostRunHook(ctx, func(ctx context.Context, changes bool, err error) {
		if err != nil && !errors.Is(err, ErrConditionNotMet) {
			log.Debug().Msg("running hook")
			f(ctx, err)
			log.Debug().Msg("hook complete")
		}
	})
}

// AddPostChangesHook adds a PostRunHook that is run after a successful state run that made changes.
func (s *StateManager) AddPostChangesHook(ctx context.Context, f func(ctx context.Context)) error {
	ctx, log := s.makeHookLog(ctx, "PostChanges")
	return s.AddPostSuccessHook(ctx, func(ctx context.Context, changes bool) {
		if changes {
			log.Debug().Msg("running hook")
			f(ctx)
			log.Debug().Msg("hook complete")
		}
	})
}

type moduleHookCtx struct {
	hookType string
	module   *StateManager
}

func (h *moduleHookCtx) MarshalZerologObject(e *zerolog.Event) {
	e.Str("type", h.hookType).Object("module", h.module)
}

// makeHookLog adds a new hook log object to ctx, and returns ctx and the logger
// If ctx already has a hookCtx, ctx will be unchanged, and a Nop logger will be returned
// This ensures the returned logger is only operable if the hook is not wrapped by a higher level hook
func (s *StateManager) makeModuleHookLog(ctx context.Context, hookType string, r *StateManager) (context.Context, zerolog.Logger) {
	if _, ok := ctx.Value(hookCtxKey).(zerolog.LogObjectMarshaler); ok {
		return ctx, zerolog.Nop()
	}
	h := &moduleHookCtx{
		hookType: hookType,
		module:   r,
	}
	ctx = context.WithValue(ctx, hookCtxKey, h)
	log := s.log.With().Object(hookCtxKey, h).Logger()
	return ctx, log
}

func (r *StateManager) logModuleApply(ctx context.Context, log zerolog.Logger) (changes bool, err error) {
	log.Debug().Msg("starting hook")
	changes, err = r.Manage(ctx)
	var e *zerolog.Event
	if err != nil {
		e = log.Error().Bool("changes", changes).Err(err)
	} else {
		e = log.Debug().Bool("changes", changes)
	}
	e.Msg("hook complete")
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
		r.logModuleApply(ctx, log)
	})
}

// SuccessTriggers triggers r when s.Apply is successful
func (s *StateManager) SuccessTriggers(ctx context.Context, r *StateManager) error {
	ctx, log := r.makeModuleHookLog(ctx, "SuccessTriggers", r)
	return s.AddPostSuccessHook(ctx, func(ctx context.Context, _ bool) {
		r.logModuleApply(ctx, log)
	})
}

// ChangesTriggers triggers r anytime s successfully makes changes
func (s *StateManager) ChangesTriggers(ctx context.Context, r *StateManager) error {
	ctx, log := r.makeModuleHookLog(ctx, "ChangesTrigger", r)
	return s.AddPostChangesHook(ctx, func(ctx context.Context) {
		r.logModuleApply(ctx, log)
	})
}

// ErrorTriggers triggers r anytime s errors
func (s *StateManager) ErrorTriggers(ctx context.Context, r *StateManager) error {
	ctx, log := r.makeModuleHookLog(ctx, "FailureTrigger", r)
	return s.AddPostErrorHook(ctx, func(ctx context.Context, _ error) {
		r.logModuleApply(ctx, log)
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
