package state

import (
	"context"
	"errors"

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
	f func(ctx context.Context, changeNeeded bool, err error) error
}

type postRunHook struct {
	hook
	f func(ctx context.Context, err error)
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

// ErrPreCheckHook is returned if a PreCheckHook fails causing the Apply to fail
var ErrPreCheckHook = errors.New("pre-check hook failed")

// AddPreCheckHook adds a function that is run before the Check step of the Runner
// If any of these functions fail, the run will fail returning the first function that errored
// returns a remove funciton that can be used to remove the hook from the runner.
// If The provided function should check the provided context so that it can exit early if the runner is stopped, or other PreCheck hooks have errored
// If the function returns ErrPreCheckConditionNotMet, it will not be logged as an error, but simply treated as a false condition check
func (s *StateRunner) AddPreCheckHook(name string, f func(context.Context) error) func() {
	h, remove := s.newHook(name)
	select {
	case s.hookMgr.addPreCheckHook <- &preCheckHook{h, wrapErrf(ErrPreCheckHook, f)}:
	case <-s.ctx.Done():
		h.removed()
	}
	return remove
}

// ErrConditionNotMet signals that a precheck condition was not met and the state should not run, but did not error in an unexpected way
var ErrConditionNotMet = errors.New("condition not met")

// ErrConditionHook is returned by the state when a Condition failed
//
// This will be wrapped in an ErrPreCheckHook, use errors.Is to check for this error.
var ErrConditionHook = errors.New("condition hook failed")

// AddCondition adds a function that is a condition to determine whether or not Check should run.
//
//	The provided function should check the provided context so that it can exit early if the runner is stopped
//
// Cancel ctx to remove the function from the state runner
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
	return s.AddPreCheckHook("condition: "+name, wrapErrf(ErrConditionHook, bf))
}

// ErrPostCheckHook is returned if a PostCheckHook fails causing the Apply to fail
var ErrPostCheckHook = errors.New("post-check hook failed")

// AddPostCheckHook adds a function that is run after the Check step of the Runner
// If any of these functions fail, the run will fail returning the first function that errored
//
//	The provided function should check the provided context so that it can exit early if the runner is stopped
//
// Cancel ctx to remove the function from the state runner
func (s *StateRunner) AddPostCheckHook(name string, f func(ctx context.Context, changeNeeded bool, err error) error) func() {
	h, remove := s.newHook(name)
	select {
	case s.hookMgr.addPostCheckHook <- &postCheckHook{h,
		func(ctx context.Context, changeNeeded bool, err error) error {
			return wrapErr(ErrPostCheckHook, f(ctx, changeNeeded, err))
		}}:
	case <-s.ctx.Done():
		h.removed()
	}
	return remove
}

// AddChangesRequiredHook adds a function that is run after the check step, if changes are required
func (s *StateRunner) AddChangesRequiredHook(name string, f func(context.Context) error) func() {
	return s.AddPostCheckHook("changesRequired: "+name, func(ctx context.Context, changeNeeded bool, err error) error {
		if changeNeeded && err == nil {
			return f(ctx)
		}
		return err
	})
}

// AddPostRunHook adds a function that is run at the end of the Run. This may be after the Run step failed or succeeded, or after the Check step failed or succeeded,
// or after any of the other Pre functions failed or succceeded.
//
// The provided function should check the provided context so that it can exit early if the runner is stopped. The err provided to the function is the error returned from the Run.
//
// Cancel ctx to remove the function from the state runner.
//
// PostRunHooks do not block returning the StateContext result. This means that a subsequent state run could run the PostRunHook before the previous one finished.
func (s *StateRunner) AddPostRunHook(name string, f func(ctx context.Context, err error)) func() {
	h, remove := s.newHook(name)
	select {
	case s.hookMgr.addPostRunHook <- &postRunHook{h, f}:
	case <-s.ctx.Done():
		h.removed()
	}
	return remove
}

// AddPostSuccessHook adds a function that is run after a successful state run.
//
//	The provided function should check the provided context so that it can exit early if the runner is stopped
//
// Cancel ctx to remove the function from the state runner
func (s *StateRunner) AddPostSuccessHook(name string, f func(context.Context)) func() {
	return s.AddPostRunHook("afterSuccess: "+name, func(ctx context.Context, err error) {
		if err == nil {
			f(ctx)
		}
	})
}

// AddPostFailureHook adds a function that is run after a failed state run.
//
//	The provided function should check the provided context so that it can exit early if the runner is stopped
//
// Cancel ctx to remove the function from the state runner
func (s *StateRunner) AddPostFailureHook(name string, f func(ctx context.Context, err error)) func() {
	return s.AddPostRunHook("afterFailure: "+name, func(ctx context.Context, err error) {
		if err != nil && !errors.Is(err, ErrConditionNotMet) {
			f(ctx, err)
		}
	})
}

// ErrDependancyFailed is returned by the state when a dependency failed
//
// This will be wrapped in an ErrPreCheckHook, use errors.Is to check for this error.
var ErrDependancyFailed = errors.New("dependency failed")

// DependOn makes s dependent on d. If s.Apply is called, this will make sure that d.Apply has been called at least once.
//
// d may fail and it will prevent s from running.
// if d.Apply is run again later, it will not automatically trigger s. To do so, see TriggerOn
func (s *StateRunner) DependOn(d *StateRunner) func() {
	f := wrapErrf(ErrDependancyFailed, d.ApplyOnce)
	remove := s.AddPreCheckHook("dependancy: "+d.state.Name(), f)
	go func() {
		<-d.ctx.Done()
		remove()
	}()
	return remove
}

func LogErr(err error) {
	log.Error().Err(err)
}

func DiscardErr(error) {
	//noop
}

// TriggerOnSuccess causes this state to be applied whenever the triggering state succeeds
//
// cb is a callback to handle the result. Use LogErr to simply log the error, or DiscardErr to do nothing
func (s *StateRunner) TriggerOnSuccess(t *StateRunner, cb func(error)) func() {
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

// BlockOn prevents s from running while t is running
func (s *StateRunner) BlockOn(t *StateRunner) func() {
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
