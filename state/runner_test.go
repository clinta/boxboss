package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"go.uber.org/goleak"
)

type catchLog struct {
	level zerolog.Level
	msg   string
	count int
}

func (h *catchLog) Run(_ *zerolog.Event, l zerolog.Level, msg string) {
	if l == h.level && msg == h.msg {
		h.count += 1
	}
}

func newCatchLog(level zerolog.Level, msg string) *catchLog {
	h := &catchLog{level, msg, 0}
	log.Logger = log.Logger.Hook(h)
	return h
}

type testState struct {
	*BasicState
	checks          []time.Time
	runs            []time.Time
	retCheckChanges bool
	retCheckErr     error
	retRunChanges   bool
	retRunErr       error
}

func newTestRunner() (context.Context, func(), *testState, *StateRunner) {
	ctx, cancel := context.WithCancel(context.Background())
	t := &testState{}
	t.BasicState = NewBasicState("testState",
		func(ctx context.Context) (bool, error) {
			t.checks = append(t.checks, time.Now())
			return t.retCheckChanges, t.retCheckErr
		},
		func(ctx context.Context) (bool, error) {
			t.runs = append(t.runs, time.Now())
			return t.retRunChanges, t.retRunErr
		},
	)
	return ctx, cancel, t, NewStateRunner(ctx, t)
}

func TestCheckFalse(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()

	assert.Zero(state.checks, "should not check before apply was called")
	assert.Nil(runner.Apply(ctx))
	assert.Equal(len(state.checks), 1, "check should have run after apply")
	assert.Zero(state.runs, "should not have run")
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.Nil(res.Err())
	assert.NotZero(res.Completed())
	cancel()
	goleak.VerifyNone(t)
}

func TestCheckTrueRunFalse(t *testing.T) {
	assert := assert.New(t)
	h := newCatchLog(zerolog.WarnLevel, checkChangesButNoRunChanges)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckChanges = true

	assert.Zero(state.checks, "should not check before apply was called")
	assert.Nil(runner.Apply(ctx))
	assert.Equal(len(state.checks), 1)
	assert.Equal(len(state.runs), 1)
	assert.Equal(h.count, 1, "did not get a warn log for check indicating changes but no changes made")
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.Nil(res.Err())
	assert.NotZero(res.Completed())
	assert.True(state.checks[0].Before(state.runs[0]), "should have checked before run")
	assert.True(res.Completed().After(state.runs[0]), "runner should have completed after run function")
	cancel()
	goleak.VerifyNone(t)
}

func TestCheckTrueRunTrue(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckChanges = true
	state.retRunChanges = true

	assert.Zero(state.checks, "should not check before apply was called")
	assert.Nil(runner.Apply(ctx))
	assert.Equal(len(state.checks), 1)
	assert.Equal(len(state.runs), 1)
	res := runner.Result(ctx)
	assert.True(res.Changed())
	assert.Nil(res.Err())
	assert.NotZero(res.Completed())
	assert.True(state.checks[0].Before(state.runs[0]), "should have checked before run")
	assert.True(res.Completed().After(state.runs[0]), "runner should have completed after run function")
	cancel()
	goleak.VerifyNone(t)
}

func TestCheckTrueRunTrueTwice(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckChanges = true
	state.retRunChanges = true

	assert.Zero(state.checks, "should not check before apply was called")
	assert.Nil(runner.Apply(ctx))
	assert.Equal(len(state.checks), 1)
	assert.Equal(len(state.runs), 1)
	res := runner.Result(ctx)
	assert.True(res.Changed())
	assert.Nil(res.Err())
	assert.NotZero(res.Completed())
	assert.True(state.checks[0].Before(state.runs[0]), "should have checked before run")
	assert.True(res.Completed().After(state.runs[0]), "runner should have completed after run function")

	assert.Nil(runner.Apply(ctx))
	assert.Equal(len(state.checks), 2)
	assert.Equal(len(state.runs), 2)
	res = runner.Result(ctx)
	assert.True(res.Changed())
	assert.Nil(res.Err())
	assert.NotZero(res.Completed())
	assert.True(state.checks[1].Before(state.runs[1]), "should have checked before run")
	assert.True(res.Completed().After(state.runs[1]), "runner should have completed after run function")

	cancel()
	goleak.VerifyNone(t)
}

func TestBeforeCheckFail(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckChanges = true
	state.retRunChanges = true

	var bfErr = errors.New("testBeforeCheckHookErr")
	_ = runner.AddBeforeCheckHook("testBeforeCheckHook", func(ctx context.Context) error {
		return bfErr
	})
	assert.ErrorIs(runner.Apply(ctx), bfErr)
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.ErrorIs(res.Err(), bfErr)
	assert.NotZero(res.Completed())
	assert.Zero(state.checks)
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}

func TestBeforeCheckSucceed(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	beforeHookTime := time.Time{}

	_ = runner.AddBeforeCheckHook("testBeforeCheckHook", func(ctx context.Context) error {
		beforeHookTime = time.Now()
		return nil
	})
	assert.Nil(runner.Apply(ctx))
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.Nil(res.Err())
	assert.NotZero(res.Completed())
	assert.True(beforeHookTime.Before(state.checks[0]))
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}

func TestBeforeCheckRemove(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	beforeHookTime := time.Time{}

	rmBf := runner.AddBeforeCheckHook("testBeforeCheckHook", func(ctx context.Context) error {
		beforeHookTime = time.Now()
		return nil
	})
	assert.Nil(runner.Apply(ctx))
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.Nil(res.Err())
	assert.NotZero(res.Completed())
	assert.True(beforeHookTime.Before(state.checks[0]))
	bft := beforeHookTime
	assert.Zero(state.runs)

	rmBf()

	assert.Nil(runner.Apply(ctx))
	assert.Equal(beforeHookTime, bft)

	cancel()
	goleak.VerifyNone(t)
}

func TestCancelingTrigger(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	startCheck := make(chan struct{})
	blockCheck := make(chan struct{})
	state.check = func(ctx context.Context) (bool, error) {
		close(startCheck)
		select {
		case <-blockCheck:
			log.Debug().Msg("blockcheck cleared")
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()

		}
	}
	triggerCtx, triggerCancel := context.WithCancel(ctx)
	applyErr := make(chan error)
	go func() {
		applyErr <- runner.Apply(triggerCtx)
	}()
	<-startCheck
	triggerCancel()
	assert.ErrorIs(<-applyErr, context.Canceled)
	res := runner.Result(context.Background())
	assert.ErrorIs(res.Err(), context.Canceled)
	close(blockCheck)
	cancel()
	goleak.VerifyNone(t)
}

func TestCancelingOneOfTwoTriggers(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	startCheck := make(chan struct{})
	blockCheck := make(chan struct{})
	state.check = func(ctx context.Context) (bool, error) {
		close(startCheck)
		select {
		case <-blockCheck:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()

		}
	}
	triggerCtx, triggerCancel := context.WithCancel(ctx)
	//log.Debug().Msg("wat")
	applyErr := make(chan error)
	go func() {
		applyErr <- runner.Apply(triggerCtx)
	}()
	triggerCtx2, _ := context.WithCancel(ctx)
	applyErr2 := make(chan error)
	go func() {
		applyErr2 <- runner.Apply(triggerCtx2)
	}()
	<-startCheck
	triggerCancel()
	assert.ErrorIs(<-applyErr, context.Canceled)
	close(blockCheck)
	assert.Nil(<-applyErr2)
	res := runner.Result(ctx)
	assert.Nil(res.Err())
	cancel()
	goleak.VerifyNone(t)
}

func TestCancelingOneTriggersWhileAnotherIsRunning(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	startCheck := make(chan struct{})
	blockCheck := make(chan struct{})
	state.check = func(ctx context.Context) (bool, error) {
		close(startCheck)
		select {
		case <-blockCheck:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()

		}
	}
	triggerCtx, _ := context.WithCancel(ctx)
	applyErr := make(chan error)
	go func() {
		applyErr <- runner.Apply(triggerCtx)
	}()
	triggerCtx2, triggerCancel2 := context.WithCancel(ctx)
	applyErr2 := make(chan error)
	<-startCheck
	go func() {
		applyErr2 <- runner.Apply(triggerCtx2)
	}()
	triggerCancel2()
	err2 := <-applyErr2
	assert.ErrorIs(err2, context.Canceled)
	close(blockCheck)
	assert.Nil(<-applyErr)
	res := runner.Result(ctx)
	assert.Nil(res.Err())
	cancel()
	goleak.VerifyNone(t)
}

func TestApplyOnce(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	assert.Nil(runner.ApplyOnce(ctx))
	assert.Nil(runner.ApplyOnce(ctx))
	assert.Equal(len(state.checks), 1)
	cancel()
	goleak.VerifyNone(t)
}
