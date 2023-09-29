package state

import "context"

// StateApplier interface is the interface that a state module must implement.
type StateApplier interface {
	// Name is a name identifying the state, used in logging.
	Name() string

	// Check reports whether changes are required. If no changes are required, Apply will not be called by the StateRunner.
	//
	// The context provided to check will be done when after any post-run hooks are complete, so cleanup tasks required by a state
	// can be done using an afterFunc to the context
	Check(context.Context) (bool, error)

	// Apply makes modifications to the system bringing it into compliance. Reports whether changes were made during the process.
	//
	// A warning will be logged if Check indicated changes were required, but Apply reports no changes were made.
	Apply(context.Context) (bool, error)

	// Manage returns a StateManager for this state
	Manage() *StateManager
}

// State is a simple implementation of the StateApplier interface.
type State struct {
	name  string
	check func(context.Context) (bool, error)
	run   func(context.Context) (bool, error)
}

func (s *State) Name() string {
	return s.name
}

func (s *State) Check(ctx context.Context) (bool, error) {
	return s.check(ctx)
}

func (s *State) Apply(ctx context.Context) (bool, error) {
	return s.run(ctx)
}

func (s *State) Manage() *StateManager {
	return NewStateManager(s)
}

func NewState(name string, check func(context.Context) (bool, error), run func(context.Context) (bool, error)) *State {
	return &State{
		name:  name,
		check: check,
		run:   run,
	}
}
