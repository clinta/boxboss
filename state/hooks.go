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

	addBeforeCheckHook chan *beforeCheckHook
	rmBeforeCheckHook  chan *beforeCheckHook
	beforeCheckHooks   map[*beforeCheckHook]struct{}

	addAfterCheckHook chan *afterCheckHook
	rmAfterCheckHook  chan *afterCheckHook
	afterCheckHooks   map[*afterCheckHook]struct{}

	addAfterRunHook chan *afterRunHook
	rmAfterRunHook  chan *afterRunHook
	afterRunHooks   map[*afterRunHook]struct{}
}

func newHookMgr(ctx context.Context, log zerolog.Logger) *hookMgr {
	h := &hookMgr{
		ctx:                ctx,
		log:                log,
		hookOp:             make(chan func()),
		addBeforeCheckHook: make(chan *beforeCheckHook),
		rmBeforeCheckHook:  make(chan *beforeCheckHook),
		beforeCheckHooks:   map[*beforeCheckHook]struct{}{},
		addAfterCheckHook:  make(chan *afterCheckHook),
		rmAfterCheckHook:   make(chan *afterCheckHook),
		afterCheckHooks:    map[*afterCheckHook]struct{}{},
		addAfterRunHook:    make(chan *afterRunHook),
		rmAfterRunHook:     make(chan *afterRunHook),
		afterRunHooks:      map[*afterRunHook]struct{}{},
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

		case f := <-h.addBeforeCheckHook:
			h.hookOp <- func() {
				log := log.With().Str("beforeCheckHook", f.name).Logger()
				log.Debug().Msg("adding beforeCheckHook")
				go func() {
					// f.ctx is a child of s.ctx, so no need to check both
					<-f.ctx.Done()
					select {
					case <-h.ctx.Done():
						// removeCtx is a child of s.ctx, so no need to call removed()
					case h.rmBeforeCheckHook <- f:
					}
				}()
				h.beforeCheckHooks[f] = struct{}{}
			}

		case f := <-h.rmBeforeCheckHook:
			h.hookOp <- func() {
				log := log.With().Str("beforeCheckHook", f.name).Logger()
				log.Debug().Msg("removing beforeCheckHook")
				delete(h.beforeCheckHooks, f)
				f.removed()
			}

		case f := <-h.addAfterCheckHook:
			h.hookOp <- func() {
				log := log.With().Str("afterCheckHook", f.name).Logger()
				log.Debug().Msg("adding afterCheckHook")
				go func() {
					<-f.ctx.Done()
					select {
					case <-h.ctx.Done():
					case h.rmAfterCheckHook <- f:
					}
				}()
				h.afterCheckHooks[f] = struct{}{}
			}

		case f := <-h.rmAfterCheckHook:
			h.hookOp <- func() {
				log := log.With().Str("afterCheckHook", f.name).Logger()
				log.Debug().Msg("removing afterCheckHook")
				delete(h.afterCheckHooks, f)
				f.removed()
			}

		case f := <-h.addAfterRunHook:
			h.hookOp <- func() {
				log := log.With().Str("afterRunHook", f.name).Logger()
				log.Debug().Msg("adding afterRunHook")
				go func() {
					<-f.ctx.Done()
					select {
					case <-h.ctx.Done():
					case h.rmAfterRunHook <- f:
					}
				}()
				h.afterRunHooks[f] = struct{}{}
			}

		case f := <-h.rmAfterRunHook:
			h.hookOp <- func() {
				log := log.With().Str("afterRunHook", f.name).Logger()
				log.Debug().Msg("removing afterRunHook")
				delete(h.afterRunHooks, f)
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

type beforeCheckHook struct {
	hook
	f func(ctx context.Context) error
}

type afterCheckHook struct {
	hook
	f func(ctx context.Context, changeNeeded bool, err error) error
}

type afterRunHook struct {
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

// ErrBeforeCheckHook is returned if a BeforeCheckHook fails causing the Apply to fail
var ErrBeforeCheckHook = errors.New("beforeCheckHook failed")

// AddBeforeCheckHook adds a function that is run before the Check step of the Runner
// If any of these functions fail, the run will fail returning the first function that errored
// returns a remove funciton that can be used to remove the hook from the runner.
// If The provided function should check the provided context so that it can exit early if the runner is stopped, or other BeforeCheck functions have errored
// If the function returns ErrPreCheckConditionNotMet, it will not be logged as an error, but simply treated as a false condition check
func (s *StateRunner) AddBeforeCheckHook(name string, f func(context.Context) error) func() {
	h, remove := s.newHook(name)
	select {
	case s.hookMgr.addBeforeCheckHook <- &beforeCheckHook{h, wrapErrf(ErrBeforeCheckHook, f)}:
	case <-s.ctx.Done():
		h.removed()
	}
	return remove
}

// ErrConditionNotMet signals that a precheck condition was not met and the state should not run, but did not error in an unexpected way
var ErrConditionNotMet = errors.New("condition not met")

// ErrConditionHook is returned by the state when a Condition failed
//
// This will be wrapped in an ErrBeforeHook, use errors.Is to check for this error.
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
	return s.AddBeforeCheckHook("condition: "+name, wrapErrf(ErrConditionHook, bf))
}

// ErrAfterCheckHook is returned if a AfterCheckHook fails causing the Apply to fail
var ErrAfterCheckHook = errors.New("before function failed")

// AddAfterCheckHook adds a function that is run after the Check step of the Runner
// If any of these functions fail, the run will fail returning the first function that errored
//
//	The provided function should check the provided context so that it can exit early if the runner is stopped
//
// Cancel ctx to remove the function from the state runner
func (s *StateRunner) AddAfterCheckHook(name string, f func(ctx context.Context, changeNeeded bool, err error) error) func() {
	h, remove := s.newHook(name)
	select {
	case s.hookMgr.addAfterCheckHook <- &afterCheckHook{h,
		func(ctx context.Context, changeNeeded bool, err error) error {
			return wrapErr(ErrAfterCheckHook, f(ctx, changeNeeded, err))
		}}:
	case <-s.ctx.Done():
		h.removed()
	}
	return remove
}

// AddChangesRequiredHook adds a function that is run after the check step, if changes are required
func (s *StateRunner) AddChangesRequiredHook(name string, f func(context.Context) error) func() {
	return s.AddAfterCheckHook("changesRequired: "+name, func(ctx context.Context, changeNeeded bool, err error) error {
		if changeNeeded && err == nil {
			return f(ctx)
		}
		return err
	})
}

// AddAfterRunHook adds a function that is run at the end of the Run. This may be after the Run step failed or succeeded, or after the Check step failed or succeeded,
// or after any of the other Pre functions failed or succceeded.
//
// The provided function should check the provided context so that it can exit early if the runner is stopped. The err provided to the function is the error returned from the Run.
//
// Cancel ctx to remove the function from the state runner.
//
// AfterRunHooks do not block returning the StateContext result. This means that a subsequent state run could run the AfterHook before the previous one finished.
func (s *StateRunner) AddAfterRunHook(name string, f func(ctx context.Context, err error)) func() {
	h, remove := s.newHook(name)
	select {
	case s.hookMgr.addAfterRunHook <- &afterRunHook{h, f}:
	case <-s.ctx.Done():
		h.removed()
	}
	return remove
}

// AddAfterSuccessHook adds a function that is run after a successful state run.
//
//	The provided function should check the provided context so that it can exit early if the runner is stopped
//
// Cancel ctx to remove the function from the state runner
func (s *StateRunner) AddAfterSuccessHook(name string, f func(context.Context)) func() {
	return s.AddAfterRunHook("afterSuccess: "+name, func(ctx context.Context, err error) {
		if err == nil {
			f(ctx)
		}
	})
}

// AddAfterFailureHook adds a function that is run after a failed state run.
//
//	The provided function should check the provided context so that it can exit early if the runner is stopped
//
// Cancel ctx to remove the function from the state runner
func (s *StateRunner) AddAfterFailureHook(name string, f func(ctx context.Context, err error)) func() {
	return s.AddAfterRunHook("afterFailure: "+name, func(ctx context.Context, err error) {
		if err != nil && !errors.Is(err, ErrConditionNotMet) {
			f(ctx, err)
		}
	})
}

// ErrDependancyFailed is returned by the state when a dependency failed
//
// This will be wrapped in an ErrBeforeCheckHook, use errors.Is to check for this error.
var ErrDependancyFailed = errors.New("dependency failed")

// DependOn makes s dependent on d. If s.Apply is called, this will make sure that d.Apply has been called at least once.
//
// d may fail and it will prevent s from running.
// if d.Apply is run again later, it will not automatically trigger s. To do so, see TriggerOn
func (s *StateRunner) DependOn(d *StateRunner) func() {
	f := wrapErrf(ErrDependancyFailed, d.ApplyOnce)
	remove := s.AddBeforeCheckHook("dependancy: "+d.state.Name(), f)
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
	remove := t.AddAfterSuccessHook("triggering: "+s.state.Name(), f)
	go func() {
		<-s.ctx.Done()
		remove()
	}()
	return remove
}

// BlockOn prevents s from running while t is running
func (s *StateRunner) BlockOn(t *StateRunner) func() {
	remove := s.AddBeforeCheckHook("blockOn: "+t.state.Name(), func(ctx context.Context) error {
		t.Result(ctx)
		return nil
	})
	go func() {
		<-t.ctx.Done()
		remove()
	}()
	return remove
}
