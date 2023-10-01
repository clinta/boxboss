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

const hookName stateCtxKey = "hook-name"

const hookWrapped stateCtxKey = "hook-wrapped"

func (s *StateManager) hookLog(ctx context.Context, hTyp string) (context.Context, zerolog.Logger) {
	if w, ok := ctx.Value(hookWrapped).(bool); ok && w {
		return ctx, zerolog.Nop()
	}

	getHookName := func() string {
		if s, ok := ctx.Value(hookName).(string); ok {
			return s
		}
		if pc, file, line, ok := runtime.Caller(3); ok {
			return zerolog.CallerMarshalFunc(pc, file, line)
		}
		return "<unknown>"
	}

	log := s.log.With().Str("hook-type", hTyp).Str("hook", getHookName()).Logger()
	return context.WithValue(ctx, hookWrapped, true), log
}

func (s *StateManager) hookedModuleLog(ctx context.Context, r *StateManager, hTyp string) (context.Context, zerolog.Logger) {
	if w, ok := ctx.Value(hookWrapped).(bool); ok && w {
		return ctx, zerolog.Nop()
	}
	ctx = context.WithValue(ctx, hookWrapped, true)
	log := s.log.With().Str("hook-type", hTyp).Type("hooked-module-type", r).Str("hooked-module-name", r.state.Name()).Logger()
	return ctx, log
}

func WithHookName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, hookName, name)
}

func getHookName(ctx context.Context) string {
	if s, ok := ctx.Value(hookName).(string); ok {
		return s
	}
	return "<unnamed>"
}

func (h *hook) name() string {
	return getHookName(h.ctx)
}

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

// ErrPreCheckHook is returned if a PreCheckHook fails. It will wrap underlying errors.
var ErrPreCheckHook = errors.New("pre-check hook failed")

// AddPreCheckHook adds a function that is run before the Check step of the State.
//
// An error in this hook will propegate to the Apply and prevent Check or Run from running, except for ErrConditionNotMet.
//
// If multiple PreCheckHooks are specified, they will run concurrently, and the failure of any will cancel the contexts of the
// remaining PreCheckHooks. The first error will be returned.
//
// AddPreCheckHook returns a function that can be used to remove the hook.
func (s *StateManager) AddPreCheckHook(ctx context.Context, f func(context.Context) error) error {
	unlock, err := s.priorityLock(ctx)
	defer unlock()
	if err != nil {
		return err
	}
	hf := func(ctx context.Context) error {
		return wrapErr(ErrPreCheckHook, f(ctx))
	}
	h := &preCheckHook{hook{ctx}, hf}
	s.preCheckHooks[h] = struct{}{}
	context.AfterFunc(ctx, func() {
		unlock, _ = s.priorityLock(context.Background())
		defer unlock()
		delete(s.preCheckHooks, h)
	})
	return nil
}

// ErrConditionNotMet signals that a precheck condition was not met and the state should not run,
// but did not error in an unexpected way.
//
// This error is not propegated to Apply, Apply will return nil if a ConditionNotMet is returned from a PreCheckHook.
var ErrConditionNotMet = errors.New("condition not met")

// ErrCondition is returned by the state when a Condition failed.
//
// This will wrap the actual error returned by the condition, and will itself be wrapped in an ErrPreCheckHook, because
// a condition is a PreCheckHook.
var ErrCondition = errors.New("condition hook failed")

// AddCondition adds a function that is a condition to determine whether or not Check should run.
//
// If conditionMet returns false, any concurrently running PreCheckHook contexts will be canceled, and the State will
// not be applied. But Apply will not return an error.
//
// AddCondition returns a function that can be used to remove the hook.
func (s *StateManager) AddCondition(ctx context.Context, f func(context.Context) (conditionMet bool, err error)) error {
	cf := func(ctx context.Context) error {
		v, err := f(ctx)
		if err == nil && !v {
			return ErrConditionNotMet
		}
		return wrapErr(ErrCondition, err)
	}
	return s.AddPreCheckHook(ctx, cf)
}

// ErrPostCheckHook is returned if a PostCheckHook fails causing the Apply to fail
var ErrPostCheckHook = errors.New("post-check hook failed")

// AddPostCheckHook adds a function that is run after the Check step of the State.
//
// This is useful for actions that need to be run in preperation of a state. For example a file management state may neeed
// A service to be stopped before the file can be managed.
//
// An error in this hook will propegate to the Apply and prevent Run from running.
//
// If multiple PostCheckHooks are specified, they will run concurrently, and the failure of any will cancel the contexts of the
// remaining PostCheckHooks. The first error will be returned.
//
// AddPostCheckHook returns a function that can be used to remove the hook.
func (s *StateManager) AddPostCheckHook(ctx context.Context, f func(ctx context.Context, changeNeeded bool) error) error {
	unlock, err := s.priorityLock(ctx)
	defer unlock()
	if err != nil {
		return err
	}
	h := &postCheckHook{hook{ctx},
		func(ctx context.Context, changeNeeded bool) error {
			return wrapErr(ErrPostCheckHook, f(ctx, changeNeeded))
		}}
	s.postCheckHooks[h] = struct{}{}
	context.AfterFunc(ctx, func() {
		unlock, _ = s.priorityLock(context.Background())
		defer unlock()
		delete(s.postCheckHooks, h)
	})
	return nil
}

// AddChangesRequiredHook adds a function that is run after the check step, only if changes are required.
func (s *StateManager) AddChangesRequiredHook(ctx context.Context, f func(context.Context) error) error {
	return s.AddPostCheckHook(ctx, func(ctx context.Context, changeNeeded bool) error {
		if changeNeeded {
			return f(ctx)
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
	unlock, err := s.priorityLock(ctx)
	defer unlock()
	if err != nil {
		return err
	}
	h := &postRunHook{hook{ctx}, f}
	s.postRunHooks[h] = struct{}{}
	context.AfterFunc(ctx, func() {
		unlock, _ = s.priorityLock(context.Background())
		defer unlock()
		delete(s.postRunHooks, h)
	})
	return nil
}

// AddPostSuccessHook adds a PostRunHook that is run after a successful state run.
func (s *StateManager) AddPostSuccessHook(ctx context.Context, f func(ctx context.Context, changes bool)) error {
	return s.AddPostRunHook(ctx, func(ctx context.Context, changes bool, err error) {
		if err == nil {
			f(ctx, changes)
		}
	})
}

// AddPostFailureHook adds a PostRunHook that is run after a failed state run.
func (s *StateManager) AddPostFailureHook(ctx context.Context, f func(ctx context.Context, err error)) error {
	return s.AddPostRunHook(ctx, func(ctx context.Context, changes bool, err error) {
		if err != nil && !errors.Is(err, ErrConditionNotMet) {
			f(ctx, err)
		}
	})
}

// AddPostChangesHook adds a PostRunHook that is run after a successful state run that made changes.
func (s *StateManager) AddPostChangesHook(ctx context.Context, f func(ctx context.Context)) error {
	return s.AddPostSuccessHook(ctx, func(ctx context.Context, changes bool) {
		if changes {
			f(ctx)
		}
	})
}

func manageWrap(s *StateManager) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := s.Manage(ctx)
		return err
	}
}

// Require sets r as a requirement that must be successful before s can be applied
func (s *StateManager) Require(ctx context.Context, r *StateManager) error {
	ctx = WithHookName(ctx, "requires: r.state.Name()")
	return s.AddPreCheckHook(ctx, manageWrap(r))
}

// RequireChanges sets r as a condition and only runs s if r made changes
func (s *StateManager) RequireChanges(ctx context.Context, r *StateManager) error {
	ctx = WithHookName(ctx, "requires-changes: r.state.Name()")
	return s.AddCondition(ctx, r.Manage)
}

// ChangesRequire requires r as a requirement that runs only if changes are indicated by s.Check
func (s *StateManager) ChangesRequire(ctx context.Context, r *StateManager) error {
	ctx = WithHookName(ctx, "changes-require: r.state.Name()")
	return s.AddChangesRequiredHook(ctx, manageWrap(r))
}

// Triggers triggers r anytime s is run (regardless of success or changes)
func (s *StateManager) Triggers(ctx context.Context, r *StateManager) error {
	ctx = WithHookName(ctx, "triggers: r.state.Name()")
	return s.AddPostRunHook(ctx, func(ctx context.Context, _ bool, _ error) {
		r.Manage(ctx)
	})
}

// SuccessTriggers triggers r when s.Apply is successful
func (s *StateManager) SuccessTriggers(ctx context.Context, r *StateManager) error {
	ctx = WithHookName(ctx, "success-triggers: r.state.Name()")
	return s.AddPostSuccessHook(ctx, func(ctx context.Context, _ bool) {
		r.Manage(ctx)
	})
}

// ChangesTrigger triggers r anytime s successfully makes changes
func (s *StateManager) ChangesTrigger(ctx context.Context, r *StateManager) error {
	ctx, log := s.hookedModuleLog(ctx, r, "changes-trigger")
	return s.AddPostChangesHook(ctx, func(ctx context.Context) {
		log.Debug().Msg("running hook")
		changes, err := r.Manage(ctx)
		log := log.With().Bool("changes", changes).Logger()
		if err != nil {
			log.Err(err).Msg("")
		}
		log.Debug().Msg("hook complete")
	})
}

// FailureTrigger triggers r anytime s fails
func (s *StateManager) FailureTrigger(ctx context.Context, r *StateManager) error {
	ctx, log := s.hookedModuleLog(ctx, r, "failure-trigger")
	return s.AddPostFailureHook(ctx, func(ctx context.Context, _ error) {
		log.Debug().Msg("running hook")
		changes, err := r.Manage(ctx)
		log := log.With().Bool("changes", changes).Logger()
		if err != nil {
			log.Err(err).Msg("")
			return
		}
		log.Debug().Msg("hook complete")
	})
}

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
