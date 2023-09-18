package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/goleak"
)

func TestBeforeCheckFail(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckChanges = true
	state.retRunChanges = true

	var bfErr = errors.New("testBeforeCheckHookErr")
	_ = runner.AddBeforeCheckHook("testBeforeCheckHook", func(ctx context.Context) error {
		return bfErr
	})
	err := runner.Apply(ctx)
	assert.ErrorIs(err, bfErr)
	assert.ErrorIs(err, ErrBeforeCheckHook)
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

func TestAfterCheckFail(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckChanges = true
	state.retRunChanges = true

	var testErr = errors.New("testAfterCheckHookErr")
	_ = runner.AddAfterCheckHook("testAfterCheckHook", func(ctx context.Context, _ bool, _ error) error {
		return testErr
	})
	err := runner.Apply(ctx)
	assert.ErrorIs(err, testErr)
	assert.ErrorIs(err, ErrAfterCheckHook)
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.ErrorIs(res.Err(), testErr)
	assert.NotZero(res.Completed())
	assert.Equal(len(state.checks), 1)
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}

func TestAfterCheckSucceed(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	beforeHookTime := time.Time{}

	_ = runner.AddAfterCheckHook("testAfterCheckHook", func(ctx context.Context, _ bool, _ error) error {
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

func TestAfterCheckRemove(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	beforeHookTime := time.Time{}

	rmBf := runner.AddAfterCheckHook("testAfterCheckHook", func(ctx context.Context, _ bool, _ error) error {
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

func TestAfterCheckCheckFaild(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckErr = errors.New("check failed")

	afcRan := false
	_ = runner.AddAfterCheckHook("testAfterCheckHook", func(ctx context.Context, _ bool, err error) error {
		assert.ErrorIs(err, state.retCheckErr)
		afcRan = true
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
	assert.True(afcRan)
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}
