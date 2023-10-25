package bossbox

import (
	"context"
	"errors"
	"log/slog"
)

type hook struct {
	ctx context.Context
}

const hookCtxKey ctxKey = "hook"

type hookCtx struct {
	hookType string
	hookName string
	module   *Manager
}

func (h *hookCtx) LogValue() slog.Value {
	vals := make([]slog.Attr, 0, 3)
	if h.hookType != "" {
		vals = append(vals, slog.String("type", h.hookType))
	}
	if h.hookName != "" {
		vals = append(vals, slog.String("name", h.hookName))
	}
	if h.module != nil {
		vals = append(vals, slog.Attr{Key: "module", Value: h.module.LogValue()})
	}
	return slog.GroupValue(vals...)
}

func WithHookName(name string) func(context.Context) context.Context {
	return func(ctx context.Context) context.Context {
		if h, ok := ctx.Value(hookCtxKey).(*hookCtx); ok {
			h.hookName = name
			return ctx
		}
		return context.WithValue(ctx, hookCtxKey, &hookCtx{
			hookType: "",
			hookName: name,
		})
	}
}

func withHookCtx(hookType string) func(context.Context) context.Context {
	return func(ctx context.Context) context.Context {
		var h *hookCtx
		if v, ok := ctx.Value(hookCtxKey).(*hookCtx); ok {
			h = v
		} else {
			h = &hookCtx{
				hookType: "",
				hookName: "",
			}
			ctx = context.WithValue(ctx, hookCtxKey, h)
		}
		if h.hookType == "" {
			h.hookType = hookType
		}
		return ctx
	}
}

func logForHookType(hookType string) func(context.Context) context.Context {
	return func(ctx context.Context) context.Context {
		if v, ok := ctx.Value(hookCtxKey).(*hookCtx); ok {
			if v.hookType == hookType {
				return setDoLog(ctx)
			}
		}
		return setDoNotLog(ctx)
	}
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

func logAndWrapHookErr(ctx context.Context, err error, parentErr error) error {
	if err == nil {
		log.DebugContext(ctx, "hook complete")
		return nil
	}
	log.ErrorContext(ctx, "hook complete", "err", err)
	return errors.Join(parentErr, err)
}

// ErrPreCheckHook is returned if a PreCheckHook errors. It will wrap underlying errors.
var ErrPreCheckHook = errors.New("PreCheck hook error")

// AddPreCheckHook adds a function that is run before the Check step
//
// An error in this hook will propegate to the Apply and prevent Check or Run from running, except for ErrConditionNotMet.
//
// If multiple PreCheckHooks are specified, they will run concurrently, and an error from any will cancel the contexts of the
// remaining PreCheckHooks. The first error will be returned.
func (s *Manager) AddPreCheckHook(ctx context.Context, f func(context.Context) error, config ...func(context.Context) context.Context) error {
	ctx = applyCtxTransforms(ctx, append(config, withHookCtx("PreCheckHook"), setDoLog)...)
	unlock, err := s.priorityLock(ctx)
	defer unlock()
	if err != nil {
		return err
	}
	hf := func(ctx context.Context) error {
		ctx = applyCtxTransforms(ctx, append(config, withHookCtx("PreCheckHook"), logForHookType("PreCheckHook"))...)
		log.DebugContext(ctx, "running hook")
		err := f(ctx)
		if err != nil {
			log.ErrorContext(ctx, "hook complete", "error", err)
			return errors.Join(ErrPreCheckHook, err)
		}
		log.DebugContext(ctx, "hook complete")
		return nil
	}
	h := &preCheckHook{hook{ctx}, hf}
	s.preCheckHooks[h] = struct{}{}
	log.DebugContext(ctx, "added hook")
	context.AfterFunc(ctx, func() {
		unlock, _ = s.priorityLock(context.Background())
		defer unlock()
		delete(s.preCheckHooks, h)
		log.DebugContext(ctx, "removed hook")
	})
	return nil
}

// ErrConditionNotMet signals that a precheck condition was not met and the module should not run,
// but did not error in an unexpected way.
//
// This error is not propegated to Apply, Apply will return nil if a ConditionNotMet is returned from a PreCheckHook.
var ErrConditionNotMet = errors.New("condition not met")

// ErrCondition is returned by the module when a Condition errors.
//
// This will wrap the actual error returned by the condition, and will itself be wrapped in an ErrPreCheckHook, because
// a condition is a PreCheckHook.
var ErrCondition = errors.New("condition hook error")

// AddCondition adds a function that is a condition to determine whether or not Check should run.
//
// If conditionMet returns false, any concurrently running PreCheckHook contexts will be canceled, and the module will
// not be applied. But Apply will not return an error.
//
// AddCondition returns a function that can be used to remove the hook.
func (s *Manager) AddCondition(ctx context.Context, f func(context.Context) (conditionMet bool, err error), config ...func(context.Context) context.Context) error {
	setLog := logForHookType("Condition")
	cf := func(ctx context.Context) error {
		ctx = setLog(ctx)
		log.DebugContext(ctx, "running hook")
		v, err := f(ctx)
		if err == nil && !v {
			log.DebugContext(ctx, "condition not met")
			return ErrConditionNotMet
		}
		return logAndWrapHookErr(ctx, err, ErrCondition)
	}
	return s.AddPreCheckHook(ctx, cf, append(config, withHookCtx("Condition"))...)
}

// ErrPostCheckHook is returned if a PostCheckHook errors causing the Apply to error
var ErrPostCheckHook = errors.New("PostCheck hook error")

// AddPostCheckHook adds a function that is run after the Check step
//
// This is useful for actions that need to be run in preperation. For example a file management module may neeed
// A service to be stopped before the file can be managed.
//
// An error in this hook will propegate to the Apply and prevent Run from running.
//
// If multiple PostCheckHooks are specified, they will run concurrently, and an error from any will cancel the contexts of the
// remaining PostCheckHooks. The first error will be returned.
//
// AddPostCheckHook returns a function that can be used to remove the hook.
func (s *Manager) AddPostCheckHook(ctx context.Context, f func(ctx context.Context, changeNeeded bool) error, config ...func(context.Context) context.Context) error {
	ctx = applyCtxTransforms(ctx, append(config, withHookCtx("PostCheckHook"), setDoLog)...)
	unlock, err := s.priorityLock(ctx)
	defer unlock()
	if err != nil {
		return err
	}
	h := &postCheckHook{hook{ctx},
		func(ctx context.Context, changeNeeded bool) error {
			ctx = applyCtxTransforms(ctx, append(config, withHookCtx("PostCheckHook"), logForHookType("PostCheckHook"))...)
			log := log.With("changeNeeded", changeNeeded)
			log.DebugContext(ctx, "running hook")
			err := f(ctx, changeNeeded)
			return logAndWrapHookErr(ctx, err, ErrPostCheckHook)
		}}
	s.postCheckHooks[h] = struct{}{}
	log.DebugContext(ctx, "added hook")
	context.AfterFunc(ctx, func() {
		unlock, _ = s.priorityLock(context.Background())
		defer unlock()
		delete(s.postCheckHooks, h)
		log.DebugContext(ctx, "removed hook")
	})
	return nil
}

var ErrChangesRequiredFailed = errors.New("changes-required hook error")

// AddChangesRequiredHook adds a function that is run after the check step, only if changes are required.
func (s *Manager) AddChangesRequiredHook(ctx context.Context, f func(context.Context) error, config ...func(context.Context) context.Context) error {
	setLog := logForHookType("ChangesRequired")
	return s.AddPostCheckHook(ctx, func(ctx context.Context, changeNeeded bool) error {
		if changeNeeded {
			ctx = setLog(ctx)
			log.DebugContext(ctx, "running hook")
			err := f(ctx)
			return logAndWrapHookErr(ctx, err, ErrChangesRequiredFailed)
		}
		return nil
	}, append(config, withHookCtx("ChangesRequired"))...)
}

// AddPostRunHook adds a function that is run at the end of the execution. This will run regardless of the source
// of any errors. The function is responsible for checking the type of error.
//
// PostRunHooks do not block returning the result. This means that a subsequent run could run the PostRunHook before the previous one finished.
//
// AddPostRunHook returns a function that can be used to remove the hook.
func (s *Manager) AddPostRunHook(ctx context.Context, f func(ctx context.Context, changed bool, err error), config ...func(context.Context) context.Context) error {
	ctx = applyCtxTransforms(ctx, append(config, withHookCtx("PostRunHook"), setDoLog)...)
	unlock, err := s.priorityLock(ctx)
	defer unlock()
	if err != nil {
		return err
	}
	h := &postRunHook{hook{ctx}, func(ctx context.Context, changed bool, err error) {
		ctx = applyCtxTransforms(ctx, append(config, withHookCtx("PostRunHook"), logForHookType("PostRunHook"))...)
		log := log.With("changed", changed)
		if err != nil {
			log = log.With("applyErr", err)
		}
		log.DebugContext(ctx, "running hook")
		f(ctx, changed, err)
		log.DebugContext(ctx, "hook complete")
	}}
	s.postRunHooks[h] = struct{}{}
	log.DebugContext(ctx, "added hook")
	context.AfterFunc(ctx, func() {
		unlock, _ = s.priorityLock(context.Background())
		defer unlock()
		delete(s.postRunHooks, h)
		log.DebugContext(ctx, "removed hook")
	})
	return nil
}

// AddPostSuccessHook adds a PostRunHook that is run after a successful module run.
func (s *Manager) AddPostSuccessHook(ctx context.Context, f func(ctx context.Context, changes bool), config ...func(context.Context) context.Context) error {
	setLog := logForHookType("PostSuccess")
	return s.AddPostRunHook(ctx, func(ctx context.Context, changes bool, err error) {
		if err == nil {
			ctx = setLog(ctx)
			log.DebugContext(ctx, "running hook")
			f(ctx, changes)
			log.DebugContext(ctx, "hook complete")
		}
	}, append(config, withHookCtx("PostSuccess"))...)
}

// AddPostErrorHook adds a PostRunHook that is run after a module run errors.
func (s *Manager) AddPostErrorHook(ctx context.Context, f func(ctx context.Context, err error), config ...func(context.Context) context.Context) error {
	setLog := logForHookType("PostFailure")
	return s.AddPostRunHook(ctx, func(ctx context.Context, changes bool, err error) {
		if err != nil && !errors.Is(err, ErrConditionNotMet) {
			ctx = setLog(ctx)
			log.DebugContext(ctx, "running hook")
			f(ctx, err)
			log.DebugContext(ctx, "hook complete")
		}
	}, append(config, withHookCtx("PostFailure"))...)
}

// AddPostChangesHook adds a PostRunHook that is run after a successful module run that made changes.
func (s *Manager) AddPostChangesHook(ctx context.Context, f func(ctx context.Context), config ...func(context.Context) context.Context) error {
	setLog := logForHookType("PostChanges")
	return s.AddPostSuccessHook(ctx, func(ctx context.Context, changes bool) {
		if changes {
			ctx = setLog(ctx)
			log.Debug("running hook")
			f(ctx)
			log.Debug("hook complete")
		}
	}, append(config, withHookCtx("PostChanges"))...)
}

func (r *Manager) logModuleApply(ctx context.Context, log *slog.Logger) (changes bool, err error) {
	log.DebugContext(ctx, "starting hook")
	changes, err = r.Manage(ctx, withHookTriggerId)
	log = log.With("changes", changes)
	if err != nil {
		log.ErrorContext(ctx, "hook complete", "err", err)
	} else {
		log.DebugContext(ctx, "hook complete")
	}
	return changes, err
}

func withHookModule(hookType string, r *Manager) func(context.Context) context.Context {
	return func(ctx context.Context) context.Context {
		if h, ok := ctx.Value(hookCtxKey).(*hookCtx); ok {
			h.module = r
			h.hookType = hookType
			return ctx
		}
		return context.WithValue(ctx, hookCtxKey, &hookCtx{
			hookType: hookType,
			hookName: "",
			module:   r,
		})
	}
}

func withHookTriggerId(ctx context.Context) context.Context {
	if h, ok := ctx.Value(hookCtxKey).(*hookCtx); ok {
		return WithTriggerId(h)(ctx)
	}
	return ctx
}

// Require sets r as a requirement that must be successful before s can be applied
func (s *Manager) Require(ctx context.Context, r *Manager, config ...func(context.Context) context.Context) error {
	setLog := logForHookType("Require")
	return s.AddPreCheckHook(ctx, func(ctx context.Context) error {
		ctx = setLog(ctx)
		_, err := r.logModuleApply(ctx, log)
		return err
	}, append(config, withHookModule("Require", r))...)
}

// RequireChanges sets r as a condition and only runs s if r made changes
func (s *Manager) RequireChanges(ctx context.Context, r *Manager, config ...func(context.Context) context.Context) error {
	setLog := logForHookType("RequireChanges")
	return s.AddCondition(ctx, func(ctx context.Context) (bool, error) {
		ctx = setLog(ctx)
		changes, err := r.logModuleApply(ctx, log)
		return changes, err
	}, append(config, withHookModule("RequireChanges", r))...)
}

// ChangesRequire requires r as a requirement that runs only if changes are indicated by s.Check
func (s *Manager) ChangesRequire(ctx context.Context, r *Manager, config ...func(context.Context) context.Context) error {
	setLog := logForHookType("ChangesRequire")
	return s.AddChangesRequiredHook(ctx,
		func(ctx context.Context) error {
			ctx = setLog(ctx)
			_, err := r.logModuleApply(ctx, log)
			return err
		}, append(config, withHookModule("ChangesRequire", r))...)
}

// Triggers triggers r anytime s is run (regardless of success or changes)
func (s *Manager) Triggers(ctx context.Context, r *Manager, config ...func(context.Context) context.Context) error {
	setLog := logForHookType("Triggers")
	return s.AddPostRunHook(ctx, func(ctx context.Context, _ bool, _ error) {
		ctx = setLog(ctx)
		_, _ = r.logModuleApply(ctx, log)
	}, append(config, withHookModule("Triggers", r))...)
}

// SuccessTriggers triggers r when s.Apply is successful
func (s *Manager) SuccessTriggers(ctx context.Context, r *Manager, config ...func(context.Context) context.Context) error {
	setLog := logForHookType("SuccessTriggers")
	return s.AddPostSuccessHook(ctx, func(ctx context.Context, _ bool) {
		ctx = setLog(ctx)
		_, _ = r.logModuleApply(ctx, log)
	}, append(config, withHookModule("SuccessTriggers", r))...)
}

// ChangesTriggers triggers r anytime s successfully makes changes
func (s *Manager) ChangesTriggers(ctx context.Context, r *Manager, config ...func(context.Context) context.Context) error {
	setLog := logForHookType("ChangesTriggers")
	return s.AddPostChangesHook(ctx, func(ctx context.Context) {
		ctx = setLog(ctx)
		_, _ = r.logModuleApply(ctx, log)
	}, append(config, withHookModule("ChangesTriggers", r))...)
}

// ErrorTriggers triggers r anytime s errors
func (s *Manager) ErrorTriggers(ctx context.Context, r *Manager, config ...func(context.Context) context.Context) error {
	setLog := logForHookType("ErrorTriggers")
	return s.AddPostErrorHook(ctx, func(ctx context.Context, _ error) {
		ctx = setLog(ctx)
		_, _ = r.logModuleApply(ctx, log)
	}, append(config, withHookModule("ErrorTriggers", r))...)
}
