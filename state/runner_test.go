package state

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
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
	*State
	checks          []time.Time
	runs            []time.Time
	retCheckChanges bool
	retCheckErr     error
	retRunChanges   bool
	retRunErr       error
}

func newTestRunner() (context.Context, func(), *testState, *StateManager) {
	ctx, cancel := context.WithCancel(context.Background())
	t := &testState{}
	t.State = NewState("testState",
		func(ctx context.Context) (bool, error) {
			t.checks = append(t.checks, time.Now())
			return t.retCheckChanges, t.retCheckErr
		},
		func(ctx context.Context) (bool, error) {
			t.runs = append(t.runs, time.Now())
			return t.retRunChanges, t.retRunErr
		},
	)
	return ctx, cancel, t, t.Manage()
}

func TestCheckFalse(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()

	assert.Zero(state.checks, "should not check before apply was called")
	changed, err := runner.Manage(ctx)
	assert.Nil(err)
	assert.Equal(len(state.checks), 1, "check should have run after apply")
	assert.Zero(state.runs, "should not have run")
	assert.False(changed)
	cancel()
	goleak.VerifyNone(t)
}

func TestCheckTrueRunFalse(t *testing.T) {
	assert := assert.New(t)
	h := newCatchLog(zerolog.WarnLevel, checkChangesButNoRunChanges)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckChanges = true

	assert.Zero(state.checks, "should not check before apply was called")
	changed, err := runner.Manage(ctx)
	assert.Equal(len(state.checks), 1)
	assert.Equal(len(state.runs), 1)
	assert.Equal(h.count, 1, "did not get a warn log for check indicating changes but no changes made")
	assert.False(changed)
	assert.Nil(err)
	assert.True(state.checks[0].Before(state.runs[0]), "should have checked before run")
	cancel()
	goleak.VerifyNone(t)
}

func TestCheckTrueRunTrue(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckChanges = true
	state.retRunChanges = true

	assert.Zero(state.checks, "should not check before apply was called")
	changed, err := runner.Manage(ctx)
	assert.Equal(len(state.checks), 1)
	assert.Equal(len(state.runs), 1)
	assert.True(changed)
	assert.Nil(err)
	assert.True(state.checks[0].Before(state.runs[0]), "should have checked before run")
	cancel()
	goleak.VerifyNone(t)
}

func TestCheckTrueRunTrueTwice(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel, state, runner := newTestRunner()
	state.retCheckChanges = true
	state.retRunChanges = true

	assert.Zero(state.checks, "should not check before apply was called")
	changed, err := runner.Manage(ctx)
	assert.Equal(len(state.checks), 1)
	assert.Equal(len(state.runs), 1)
	assert.True(changed)
	assert.Nil(err)
	assert.True(state.checks[0].Before(state.runs[0]), "should have checked before run")

	changed, err = runner.Manage(ctx)
	assert.Equal(len(state.checks), 2)
	assert.Equal(len(state.runs), 2)
	assert.True(changed)
	assert.Nil(err)
	assert.True(state.checks[1].Before(state.runs[1]), "should have checked before run")

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
		_, err := runner.Manage(triggerCtx)
		applyErr <- err
	}()
	<-startCheck
	triggerCancel()
	assert.ErrorIs(<-applyErr, context.Canceled)
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
		_, err := runner.Manage(triggerCtx)
		applyErr <- err
	}()
	triggerCtx2 := ctx
	applyErr2 := make(chan error)
	go func() {
		_, err := runner.Manage(triggerCtx2)
		applyErr2 <- err
	}()
	<-startCheck
	triggerCancel()
	assert.ErrorIs(<-applyErr, context.Canceled)
	close(blockCheck)
	assert.Nil(<-applyErr2)
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
	triggerCtx := ctx
	applyErr := make(chan error)
	go func() {
		_, err := runner.Manage(triggerCtx)
		applyErr <- err
	}()
	triggerCtx2, triggerCancel2 := context.WithCancel(ctx)
	applyErr2 := make(chan error)
	<-startCheck
	go func() {
		_, err := runner.Manage(triggerCtx2)
		applyErr2 <- err
	}()
	triggerCancel2()
	err2 := <-applyErr2
	assert.ErrorIs(err2, context.Canceled)
	close(blockCheck)
	assert.Nil(<-applyErr)
	cancel()
	goleak.VerifyNone(t)
}
