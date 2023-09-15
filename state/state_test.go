package state

import (
	"context"
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

func TestCheckFalse(t *testing.T) {
	assert := assert.New(t)
	checked := time.Time{}

	ctx, cancel := context.WithCancel(context.Background())
	check := func(ctx context.Context) (bool, error) {
		checked = time.Now()
		return false, nil
	}
	run := func(ctx context.Context) (bool, error) {
		assert.FailNow("check returned false, should not run")
		return false, nil
	}
	runner := NewStateRunner(ctx, NewBasicState("test", check, run))

	assert.Zero(checked, "should not check before apply was called")
	assert.Nil(runner.Apply(ctx))
	assert.NotZero(checked, "check should have run after apply")
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
	checked := time.Time{}
	ran := time.Time{}

	ctx, cancel := context.WithCancel(context.Background())
	check := func(ctx context.Context) (bool, error) {
		checked = time.Now()
		assert.Zero(ran, "run should have not already run while check is running")
		return true, nil
	}
	run := func(ctx context.Context) (bool, error) {
		ran = time.Now()
		assert.NotZero(checked, "check should have run already")
		assert.True(ran.After(checked), "should have checked before run")
		return false, nil
	}
	runner := NewStateRunner(ctx, NewBasicState("test", check, run))

	assert.Zero(checked, "should not check before apply was called")
	assert.Nil(runner.Apply(ctx))
	assert.NotZero(ran, "should have run after apply")
	assert.Equal(h.count, 1, "did not get a warn log for check indicating changes but no changes made")
	res := runner.Result(ctx)
	assert.False(res.Changed())
	assert.Nil(res.Err())
	assert.NotZero(res.Completed())
	assert.True(res.Completed().After(ran), "runner should have completed after run function")
	cancel()
	goleak.VerifyNone(t)
}

//TODO we need pre-change functions. Like maybe if check returns true, then we need to stop a service before editing a file
