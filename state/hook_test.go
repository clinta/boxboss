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
