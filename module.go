package bossbox

import "context"

// Module interface is the interface that a module must implement.
type Module interface {
	// Name is a name identifying the module, used in logging. The type is logged independently. So this should uniquely identify this instance.
	// For example, a file module should use the name of the file being managed.
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

	// Manage returns a Manager for this module
	Manage() *Manager
}

// State is a simple implementation of the Module interface.
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

func (s *State) Manage() *Manager {
	return NewManager(s)
}

func NewState(name string, check func(context.Context) (bool, error), run func(context.Context) (bool, error)) *State {
	return &State{
		name:  name,
		check: check,
		run:   run,
	}
}
