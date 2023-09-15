package state

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/goleak"
)

// Compiler checks to make sure the interface is properly implemented
type dummyState struct {
}

func (d *dummyState) Check(context.Context) (bool, error) {
	return false, nil
}

func (d *dummyState) Run(context.Context) (bool, error) {
	return false, nil
}

func (d *dummyState) Name() string {
	return "DummyState"
}

var _ State = (*dummyState)(nil)

//////

func TestSomething(t *testing.T) {
	assert := assert.New(t)
	checked := false

	ctx, cancel := context.WithCancel(context.Background())
	check := func(ctx context.Context) (bool, error) {
		checked = true
		return false, nil
	}
	run := func(ctx context.Context) (bool, error) {
		assert.FailNow("check returned false, should not run")
		return false, nil
	}
	runner := NewStateRunner(ctx, NewBasicState("test", check, run))

	assert.False(checked, "check should not run just becasue the runner was created")
	assert.Nil(runner.Apply(ctx))
	assert.True(checked, "check should have run after apply")
	cancel()
	goleak.VerifyNone(t)
}
