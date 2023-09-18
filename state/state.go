package state

import "context"

// State interface is the interface that a state module must implement.
type State interface {
	// Name is a name identifying the state, used in logging.
	Name() string

	// Check reports whether changes are required. If no changes are required, Run will not be called by the StateRunner.
	Check(context.Context) (bool, error)
	// Run makes modifications to the system bringing it into compliance. Reports whether changes were made during the process.
	//
	// A warning will be logged if Check indicated changes were required, but Run reports no changes were made.
	Run(context.Context) (bool, error)
}

// BasicState is a simple implementation of the state interface.
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

func NewBasicState(name string, check func(context.Context) (bool, error), run func(context.Context) (bool, error)) *BasicState {
	return &BasicState{
		name:  name,
		check: check,
		run:   run,
	}
}
