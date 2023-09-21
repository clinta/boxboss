package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/goleak"
)

func TestPreCheckFail(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckChanges = true
	state.retRunChanges = true

	var hookErr = errors.New("testPreCheckHookErr")
	_ = runner.AddPreCheckHook("testPreCheckHook", func(ctx context.Context) error {
		return hookErr
	})
	err := runner.Apply(ctx)
	assert.ErrorIs(err, hookErr)
	assert.ErrorIs(err, ErrPreCheckHook)
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.ErrorIs(res.Err(), hookErr)
	assert.NotZero(res.Completed())
	assert.Zero(state.checks)
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}

func TestPreCheckSucceed(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	hookTime := time.Time{}

	_ = runner.AddPreCheckHook("testPreCheckHook", func(ctx context.Context) error {
		hookTime = time.Now()
		return nil
	})
	assert.Nil(runner.Apply(ctx))
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.Nil(res.Err())
	assert.NotZero(res.Completed())
	assert.True(hookTime.Before(state.checks[0]))
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}

func TestPreCheckRemove(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	hookTime := time.Time{}

	rmBf := runner.AddPreCheckHook("testPreCheckHook", func(ctx context.Context) error {
		hookTime = time.Now()
		return nil
	})
	assert.Nil(runner.Apply(ctx))
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.Nil(res.Err())
	assert.NotZero(res.Completed())
	assert.True(hookTime.Before(state.checks[0]))
	bft := hookTime
	assert.Zero(state.runs)

	rmBf()

	assert.Nil(runner.Apply(ctx))
	assert.Equal(hookTime, bft)

	cancel()
	goleak.VerifyNone(t)
}

func TestPostCheckFail(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckChanges = true
	state.retRunChanges = true

	var testErr = errors.New("testPostCheckHookErr")
	_ = runner.AddPostCheckHook("testPostCheckHook", func(ctx context.Context, _ bool) error {
		return testErr
	})
	err := runner.Apply(ctx)
	assert.ErrorIs(err, testErr)
	assert.ErrorIs(err, ErrPostCheckHook)
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.ErrorIs(res.Err(), testErr)
	assert.NotZero(res.Completed())
	assert.Equal(len(state.checks), 1)
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}

func TestPostCheckSucceed(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	hookTime := time.Time{}

	_ = runner.AddPostCheckHook("testPostCheckHook", func(ctx context.Context, _ bool) error {
		hookTime = time.Now()
		return nil
	})
	assert.Nil(runner.Apply(ctx))
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.Nil(res.Err())
	assert.NotZero(res.Completed())
	assert.True(hookTime.After(state.checks[0]))
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}

func TestPostCheckRemove(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	beforeHookTime := time.Time{}

	rmh := runner.AddPostCheckHook("testPostCheckHook", func(ctx context.Context, _ bool) error {
		beforeHookTime = time.Now()
		return nil
	})
	assert.Nil(runner.Apply(ctx))
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.Nil(res.Err())
	assert.NotZero(res.Completed())
	assert.True(beforeHookTime.After(state.checks[0]))
	bft := beforeHookTime
	assert.Zero(state.runs)

	rmh()

	assert.Nil(runner.Apply(ctx))
	assert.Equal(beforeHookTime, bft)

	cancel()
	goleak.VerifyNone(t)
}

func TestPostCheckCheckFaild(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckErr = errors.New("check failed")

	_ = runner.AddPostCheckHook("testPostCheckHook", func(ctx context.Context, _ bool) error {
		assert.FailNow("this should not have run")
		return nil
	})
	err := runner.Apply(ctx)
	assert.ErrorIs(err, state.retCheckErr)
	assert.ErrorIs(err, ErrCheckFailed)
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.ErrorIs(res.Err(), state.retCheckErr)
	assert.NotZero(res.Completed())
	assert.Equal(len(state.checks), 1)
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}

func TestPostRunSucceed(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	postRunHookTime := time.Time{}
	hookDone := make(chan struct{})

	_ = runner.AddPostRunHook("testPostRunHook", func(ctx context.Context, _ *StateRunResult) {
		postRunHookTime = time.Now()
		close(hookDone)
	})
	assert.Nil(runner.Apply(ctx))
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.Nil(res.Err())
	assert.NotZero(res.Completed())
	<-hookDone
	assert.True(postRunHookTime.After(state.checks[0]))
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}

func TestPostRunRemove(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckChanges = true
	state.retRunChanges = true
	hookTime := time.Time{}
	hookDone := make(chan struct{})

	rmHook := runner.AddPostRunHook("testPostRunHook", func(ctx context.Context, _ *StateRunResult) {
		hookTime = time.Now()
		close(hookDone)
	})
	assert.Nil(runner.Apply(ctx))
	res := runner.Result(ctx)
	assert.True(res.Changed())
	assert.Nil(res.Err())
	assert.NotZero(res.Completed())
	<-hookDone
	assert.True(hookTime.After(state.checks[0]), "%s not after %s", hookTime, state.checks[0])
	assert.True(hookTime.After(state.runs[0]), "%s not after %s", hookTime, state.runs[0])
	bft := hookTime

	rmHook()

	assert.Nil(runner.Apply(ctx))
	assert.Equal(hookTime, bft)

	cancel()
	goleak.VerifyNone(t)
}

func TestPostRunRunFaild(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckChanges = true
	state.retRunErr = errors.New("check failed")
	hookDone := make(chan struct{})

	_ = runner.AddPostRunHook("testPostRunHook", func(ctx context.Context, res *StateRunResult) {
		assert.ErrorIs(res.Err(), state.retRunErr)
		close(hookDone)
	})
	err := runner.Apply(ctx)
	assert.ErrorIs(err, state.retRunErr)
	assert.ErrorIs(err, ErrRunFailed)
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.ErrorIs(res.Err(), state.retRunErr)
	assert.NotZero(res.Completed())
	assert.Equal(len(state.checks), 1)
	<-hookDone
	cancel()
	goleak.VerifyNone(t)
}

func TestCondition(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	condition := false
	var conditionErr error = nil
	hookTime := time.Time{}

	_ = runner.AddCondition("testCondition", func(ctx context.Context) (bool, error) {
		hookTime = time.Now()
		return condition, conditionErr
	})
	assert.ErrorIs(runner.Apply(ctx), ErrStateNotRun)
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.ErrorIs(res.Err(), ErrStateNotRun)
	assert.NotZero(res.Completed())

	condition = true
	state.retCheckChanges = true
	state.retRunChanges = true
	assert.Nil(runner.Apply(ctx))
	res = runner.Result(ctx)
	assert.Nil(res.Err())
	assert.True(res.Changed())
	assert.True(hookTime.Before(res.Completed()))

	cancel()
	goleak.VerifyNone(t)
}
