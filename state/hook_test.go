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

	var bfErr = errors.New("testPreCheckHookErr")
	_ = runner.AddPreCheckHook("testPreCheckHook", func(ctx context.Context) error {
		return bfErr
	})
	err := runner.Apply(ctx)
	assert.ErrorIs(err, bfErr)
	assert.ErrorIs(err, ErrPreCheckHook)
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.ErrorIs(res.Err(), bfErr)
	assert.NotZero(res.Completed())
	assert.Zero(state.checks)
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}

func TestPreCheckSucceed(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	beforeHookTime := time.Time{}

	_ = runner.AddPreCheckHook("testPreCheckHook", func(ctx context.Context) error {
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

func TestPreCheckRemove(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	beforeHookTime := time.Time{}

	rmBf := runner.AddPreCheckHook("testPreCheckHook", func(ctx context.Context) error {
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
	beforeHookTime := time.Time{}

	_ = runner.AddPostCheckHook("testPostCheckHook", func(ctx context.Context, _ bool) error {
		beforeHookTime = time.Now()
		return nil
	})
	assert.Nil(runner.Apply(ctx))
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.Nil(res.Err())
	assert.NotZero(res.Completed())
	assert.True(beforeHookTime.After(state.checks[0]))
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}

func TestPostCheckRemove(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	beforeHookTime := time.Time{}

	rmBf := runner.AddPostCheckHook("testPostCheckHook", func(ctx context.Context, _ bool) error {
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

	rmBf()

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
