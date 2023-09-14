package state

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
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

	// assert equality
	assert.Equal(t, 123, 123, "they should be equal")

	// assert inequality
	assert.NotEqual(t, 123, 456, "they should not be equal")

}
