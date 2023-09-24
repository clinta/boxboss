package state

import (
	"context"
	"errors"
)

// TODO: Use contexts for names, add loggers

type preCheckHook struct {
	ctx context.Context
	f   func(ctx context.Context) error
}

type postCheckHook struct {
	ctx context.Context
	f   func(ctx context.Context, changeNeeded bool) error
}

type postRunHook struct {
	ctx context.Context
	f   func(ctx context.Context, changed bool, err error)
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
	h := &preCheckHook{ctx, hf}
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
	h := &postCheckHook{ctx,
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
	h := &postRunHook{ctx, f}
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

/*
// ErrDependancyFailed is returned by the state when a dependency failed.
//
// This will be wrapped in an ErrPreCheckHook.
var ErrDependancyFailed = errors.New("dependency failed")

func (s *StateRunner) relate(f func(s *StateRunner, t *StateRunner) func(), ts ...*StateRunner) func() {
	wg := sync.WaitGroup{}
	rCh := make(chan func())
	for _, t := range ts {
		if s == t {
			continue
		}
		wg.Add(1)
		go func(t *StateRunner) {
			rCh <- f(s, t)
			wg.Done()
		}(t)
	}
	go func() {
		wg.Wait()
		close(rCh)
	}()
	removes := []func(){}
	for r := range rCh {
		removes = append(removes, r)
	}

	return func() {
		wg := sync.WaitGroup{}
		for _, r := range removes {
			wg.Add(1)
			go func(r func()) {
				r()
				wg.Done()
			}(r)
		}
		wg.Wait()
	}
}

func (s *StateRunner) dependOn(d *StateRunner) func() {
	f := wrapErrf(ErrDependancyFailed, func(ctx context.Context) error { _, err := d.ApplyOnce(ctx); return err })
	remove := s.AddPreCheckHook("dependancy: "+d.state.Name(), f)
	go func() {
		<-d.ctx.Done()
		remove()
	}()
	return remove
}

// DependOn makes s dependent on d. If s.Apply is called, this will make sure that d.Apply has been called at least once.
//
// If d.ApplyOnce returns an error it will prevent s.Apply from running.
func (s *StateRunner) DependOn(d ...*StateRunner) func() {
	return s.relate((*StateRunner).dependOn, d...)
}

func LogErr(err error) {
	log.Error().Err(err)
}

func DiscardErr(error) {
	//noop
}

func (s *StateRunner) triggerOnSuccess(cb func(error), t *StateRunner) func() {
	f := func(ctx context.Context) {
		_, err := s.Apply(ctx)
		cb(err)
	}
	remove := t.AddPostSuccessHook("triggering: "+s.state.Name(), f)
	go func() {
		<-s.ctx.Done()
		remove()
	}()
	return remove
}

// TriggerOnSuccess causes this s.Apply to be run whenever the t.Apply succeeds.
//
// cb is a callback to handle the result of s.Apply. Use LogErr to simply log the error, or DiscardErr to do nothing.
func (s *StateRunner) TriggerOnSuccess(cb func(error), t ...*StateRunner) func() {
	f := func(s *StateRunner, t *StateRunner) func() {
		return s.triggerOnSuccess(cb, t)
	}
	return s.relate(f, t...)
}

func (s *StateRunner) blockOn(t *StateRunner) func() {
	remove := s.AddPreCheckHook("blockOn: "+t.state.Name(), func(ctx context.Context) error {
		t.Result(ctx)
		return nil
	})
	go func() {
		<-t.ctx.Done()
		remove()
	}()
	return remove
}

// BlockOn prevents s.Apply from running while t.Apply is running
// TODO add ConflictsWith to add BlockOn in both directions.
func (s *StateRunner) BlockOn(t ...*StateRunner) func() {
	return s.relate((*StateRunner).blockOn, t...)
}

func (s *StateRunner) ConflictsWith(t ...*StateRunner) func() {
	ts := append(t, s)
	wg := sync.WaitGroup{}
	rCh := make(chan func())
	for _, s := range ts {
		wg.Add(1)
		go func(s *StateRunner) {
			rCh <- s.BlockOn(ts...)
			wg.Done()
		}(s)
	}

	go func() {
		wg.Wait()
		close(rCh)
	}()

	removes := []func(){}
	for r := range rCh {
		removes = append(removes, r)
	}

	return func() {
		wg := sync.WaitGroup{}
		for _, r := range removes {
			wg.Add(1)
			go func(r func()) {
				r()
				wg.Done()
			}(r)
		}
		wg.Wait()
	}
}
*/
