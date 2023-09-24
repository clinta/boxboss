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
	_ = runner.AddPreCheckHook(ctx, func(ctx context.Context) error {
		return hookErr
	})
	changed, err := runner.Manage(ctx)
	assert.False(changed)
	assert.ErrorIs(err, hookErr)
	assert.ErrorIs(err, ErrPreCheckHook)
	assert.Zero(state.checks)
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}

func TestPreCheckSucceed(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	hookTime := time.Time{}

	_ = runner.AddPreCheckHook(ctx, func(ctx context.Context) error {
		hookTime = time.Now()
		return nil
	})
	changed, err := runner.Manage(ctx)
	assert.Nil(err)
	assert.False(changed)
	assert.Nil(err)
	assert.True(hookTime.Before(state.checks[0]))
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}

func TestPreCheckRemove(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	hookTime := time.Time{}

	hCtx, rmh := context.WithCancel(ctx)
	assert.Nil(runner.AddPreCheckHook(hCtx, func(ctx context.Context) error {
		hookTime = time.Now()
		return nil
	}))
	changed, err := runner.Manage(ctx)
	assert.False(changed)
	assert.Nil(err)
	assert.True(hookTime.Before(state.checks[0]))
	bft := hookTime
	assert.Zero(state.runs)

	rmh()

	_, err = runner.Manage(ctx)
	assert.Nil(err)
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
	_ = runner.AddPostCheckHook(ctx, func(ctx context.Context, _ bool) error {
		return testErr
	})
	changed, err := runner.Manage(ctx)
	assert.ErrorIs(err, testErr)
	assert.ErrorIs(err, ErrPostCheckHook)
	assert.False(changed)
	assert.Equal(len(state.checks), 1)
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}

func TestPostCheckSucceed(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	hookTime := time.Time{}

	_ = runner.AddPostCheckHook(ctx, func(ctx context.Context, _ bool) error {
		hookTime = time.Now()
		return nil
	})
	changed, err := runner.Manage(ctx)
	assert.False(changed)
	assert.Nil(err)
	assert.True(hookTime.After(state.checks[0]))
	assert.Zero(state.runs)
	cancel()
	goleak.VerifyNone(t)
}

func TestPostCheckRemove(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	beforeHookTime := time.Time{}

	hCtx, rmh := context.WithCancel(ctx)
	assert.Nil(runner.AddPostCheckHook(hCtx, func(ctx context.Context, _ bool) error {
		beforeHookTime = time.Now()
		return nil
	}))
	changed, err := runner.Manage(ctx)
	assert.False(changed)
	assert.Nil(err)
	assert.True(beforeHookTime.After(state.checks[0]))
	bft := beforeHookTime
	assert.Zero(state.runs)

	rmh()

	_, err = runner.Manage(ctx)
	assert.Nil(err)
	assert.Equal(beforeHookTime, bft)

	cancel()
	goleak.VerifyNone(t)
}

func TestPostCheckCheckFaild(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckErr = errors.New("check failed")

	_ = runner.AddPostCheckHook(ctx, func(ctx context.Context, _ bool) error {
		assert.FailNow("this should not have run")
		return nil
	})
	changed, err := runner.Manage(ctx)
	assert.ErrorIs(err, state.retCheckErr)
	assert.ErrorIs(err, ErrCheckFailed)
	assert.False(changed)
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

	_ = runner.AddPostRunHook(ctx, func(ctx context.Context, _ bool, _ error) {
		postRunHookTime = time.Now()
		close(hookDone)
	})
	changed, err := runner.Manage(ctx)
	assert.Nil(err)
	assert.False(changed)
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

	hCtx, rmh := context.WithCancel(ctx)
	assert.Nil(runner.AddPostRunHook(hCtx, func(ctx context.Context, _ bool, _ error) {
		hookTime = time.Now()
		close(hookDone)
	}))
	changed, err := runner.Manage(ctx)
	assert.True(changed)
	assert.Nil(err)
	<-hookDone
	assert.True(hookTime.After(state.checks[0]), "%s not after %s", hookTime, state.checks[0])
	assert.True(hookTime.After(state.runs[0]), "%s not after %s", hookTime, state.runs[0])
	bft := hookTime

	rmh()

	_, err = runner.Manage(ctx)
	assert.Nil(err)
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

	_ = runner.AddPostRunHook(ctx, func(ctx context.Context, changes bool, err error) {
		assert.ErrorIs(err, state.retRunErr)
		close(hookDone)
	})
	changed, err := runner.Manage(ctx)
	assert.False(changed)
	assert.ErrorIs(err, state.retRunErr)
	assert.ErrorIs(err, ErrRunFailed)
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
	//hookTime := time.Time{}

	_ = runner.AddCondition(ctx, func(ctx context.Context) (bool, error) {
		//hookTime = time.Now()
		return condition, conditionErr
	})
	changed, err := runner.Manage(ctx)
	assert.Nil(err)
	assert.False(changed)

	condition = true
	state.retCheckChanges = true
	state.retRunChanges = true
	changed, err = runner.Manage(ctx)
	assert.Nil(err)
	assert.True(changed)

	cancel()
	goleak.VerifyNone(t)
}
