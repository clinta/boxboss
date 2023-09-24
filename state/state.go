package state

import "context"

// StateApplier interface is the interface that a state module must implement.
type StateApplier interface {
	// Name is a name identifying the state, used in logging.
	Name() string

	// Check reports whether changes are required. If no changes are required, Apply will not be called by the StateRunner.
	Check(context.Context) (bool, error)

	// Apply makes modifications to the system bringing it into compliance. Reports whether changes were made during the process.
	//
	// A warning will be logged if Check indicated changes were required, but Apply reports no changes were made.
	Apply(context.Context) (bool, error)
}

// State is a simple implementation of the StateApplier interface.
type State struct {
	name  string
	check func(context.Context) (bool, error)
	run   func(context.Context) (bool, error)
}

func (b *State) Name() string {
	return b.name
}

func (b *State) Check(ctx context.Context) (bool, error) {
	return b.check(ctx)
}

func (b *State) Apply(ctx context.Context) (bool, error) {
	return b.run(ctx)
}

func NewState(name string, check func(context.Context) (bool, error), run func(context.Context) (bool, error)) *State {
	return &State{
		name:  name,
		check: check,
		run:   run,
	}
}
