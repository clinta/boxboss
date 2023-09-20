package state

import (
	"context"
	"errors"
	"sync"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type hookMgr struct {
	ctx context.Context
	log zerolog.Logger

	hookOp chan func()

	addPreCheckHook chan *preCheckHook
	rmPreCheckHook  chan *preCheckHook
	preCheckHooks   map[*preCheckHook]struct{}

	addPostCheckHook chan *postCheckHook
	rmPostCheckHook  chan *postCheckHook
	postCheckHooks   map[*postCheckHook]struct{}

	addPostRunHook chan *postRunHook
	rmPostRunHook  chan *postRunHook
	postRunHooks   map[*postRunHook]struct{}
}

func newHookMgr(ctx context.Context, log zerolog.Logger) *hookMgr {
	h := &hookMgr{
		ctx:              ctx,
		log:              log,
		hookOp:           make(chan func()),
		addPreCheckHook:  make(chan *preCheckHook),
		rmPreCheckHook:   make(chan *preCheckHook),
		preCheckHooks:    map[*preCheckHook]struct{}{},
		addPostCheckHook: make(chan *postCheckHook),
		rmPostCheckHook:  make(chan *postCheckHook),
		postCheckHooks:   map[*postCheckHook]struct{}{},
		addPostRunHook:   make(chan *postRunHook),
		rmPostRunHook:    make(chan *postRunHook),
		postRunHooks:     map[*postRunHook]struct{}{},
	}
	go h.manage()
	return h
}

func (h *hookMgr) manage() {
	log := h.log
	for {
		select {
		case <-h.ctx.Done():
			log.Debug().Msg("shutting down hook manager")
			return

		case f := <-h.addPreCheckHook:
			h.hookOp <- func() {
				log := log.With().Str("pre-check hook", f.name).Logger()
				log.Debug().Msg("adding pre check hook")
				go func() {
					// f.ctx is a child of s.ctx, so no need to check both
					<-f.ctx.Done()
					select {
					case <-h.ctx.Done():
						// removeCtx is a child of s.ctx, so no need to call removed()
					case h.rmPreCheckHook <- f:
					}
				}()
				h.preCheckHooks[f] = struct{}{}
			}

		case f := <-h.rmPreCheckHook:
			h.hookOp <- func() {
				log := log.With().Str("pre-check hook", f.name).Logger()
				log.Debug().Msg("removing pre-check hook")
				delete(h.preCheckHooks, f)
				f.removed()
			}

		case f := <-h.addPostCheckHook:
			h.hookOp <- func() {
				log := log.With().Str("post-check hook", f.name).Logger()
				log.Debug().Msg("adding post-check hook")
				go func() {
					<-f.ctx.Done()
					select {
					case <-h.ctx.Done():
					case h.rmPostCheckHook <- f:
					}
				}()
				h.postCheckHooks[f] = struct{}{}
			}

		case f := <-h.rmPostCheckHook:
			h.hookOp <- func() {
				log := log.With().Str("post-check hook", f.name).Logger()
				log.Debug().Msg("removing post-check hook")
				delete(h.postCheckHooks, f)
				f.removed()
			}

		case f := <-h.addPostRunHook:
			h.hookOp <- func() {
				log := log.With().Str("post-run hook", f.name).Logger()
				log.Debug().Msg("adding post-run hook")
				go func() {
					<-f.ctx.Done()
					select {
					case <-h.ctx.Done():
					case h.rmPostRunHook <- f:
					}
				}()
				h.postRunHooks[f] = struct{}{}
			}

		case f := <-h.rmPostRunHook:
			h.hookOp <- func() {
				log := log.With().Str("post-run hook", f.name).Logger()
				log.Debug().Msg("removing post-run hook")
				delete(h.postRunHooks, f)
				f.removed()
			}
		}
	}
}

type hook struct {
	ctx     context.Context
	removed func()
	name    string
}

func (s *StateRunner) newHook(name string) (hook, func()) {
	ctx, cancel := context.WithCancel(s.ctx)
	removedCtx, removed := context.WithCancel(s.ctx)
	h := hook{
		ctx:     ctx,
		removed: removed,
		name:    name,
	}
	remove := func() {
		cancel()
		<-removedCtx.Done()
	}
	return h, remove
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
	f func(ctx context.Context, res *StateRunResult)
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
//
// TODO: these are not hashed by f, so duplicates are possible, fix this
func (s *StateRunner) AddPreCheckHook(name string, f func(context.Context) error) func() {
	h, remove := s.newHook(name)
	select {
	case s.hookMgr.addPreCheckHook <- &preCheckHook{h, wrapErrf(ErrPreCheckHook, f)}:
	case <-s.ctx.Done():
		h.removed()
	}
	return remove
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
func (s *StateRunner) AddCondition(name string, f func(context.Context) (conditionMet bool, err error)) func() {
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
	return s.AddPreCheckHook("condition: "+name, wrapErrf(ErrCondition, bf))
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
func (s *StateRunner) AddPostCheckHook(name string, f func(ctx context.Context, changeNeeded bool) error) func() {
	h, remove := s.newHook(name)
	select {
	case s.hookMgr.addPostCheckHook <- &postCheckHook{h,
		func(ctx context.Context, changeNeeded bool) error {
			return wrapErr(ErrPostCheckHook, f(ctx, changeNeeded))
		}}:
	case <-s.ctx.Done():
		h.removed()
	}
	return remove
}

// AddChangesRequiredHook adds a function that is run after the check step, only if changes are required.
func (s *StateRunner) AddChangesRequiredHook(name string, f func(context.Context) error) func() {
	return s.AddPostCheckHook("changesRequired: "+name, func(ctx context.Context, changeNeeded bool) error {
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
func (s *StateRunner) AddPostRunHook(name string, f func(ctx context.Context, res *StateRunResult)) func() {
	h, remove := s.newHook(name)
	select {
	case s.hookMgr.addPostRunHook <- &postRunHook{h, f}:
	case <-s.ctx.Done():
		h.removed()
	}
	return remove
}

// AddPostSuccessHook adds a PostRunHook that is run after a successful state run.
func (s *StateRunner) AddPostSuccessHook(name string, f func(context.Context)) func() {
	return s.AddPostRunHook("afterSuccess: "+name, func(ctx context.Context, res *StateRunResult) {
		if res.Err() == nil {
			f(ctx)
		}
	})
}

// AddPostFailureHook adds a PostRunHook that is run after a failed state run.
func (s *StateRunner) AddPostFailureHook(name string, f func(ctx context.Context, res *StateRunResult)) func() {
	return s.AddPostRunHook("afterFailure: "+name, func(ctx context.Context, res *StateRunResult) {
		err := res.Err()
		if err != nil && !errors.Is(err, ErrConditionNotMet) {
			f(ctx, res)
		}
	})
}

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
	f := wrapErrf(ErrDependancyFailed, d.ApplyOnce)
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
		cb(s.Apply(ctx))
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
