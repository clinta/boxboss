package state

import "context"

// State interface is the interface that a state module must implement
type State interface {
	// Name is a name identifying the state, used in logging
	Name() string

	// Check checks whether or not changes are necessary before running them
	Check(context.Context) (bool, error)
	// Run runs the states, making system modifications
	Run(context.Context) (bool, error)
}

// BasicState is a simple implementation of the state interface
type BasicState struct {
	name  string
	check func(context.Context) (bool, error)
	run   func(context.Context) (bool, error)
}

func (b *BasicState) Name() string {
	return b.name
}

func (b *BasicState) Check(ctx context.Context) (bool, error) {
	return b.check(ctx)
}

func (b *BasicState) Run(ctx context.Context) (bool, error) {
	return b.run(ctx)
}

// NewBasicState creates a new simple state
func NewBasicState(name string, check func(context.Context) (bool, error), run func(context.Context) (bool, error)) *BasicState {
	return &BasicState{
		name:  name,
		check: check,
		run:   run,
	}
}
