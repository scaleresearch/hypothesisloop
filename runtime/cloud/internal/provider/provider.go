// Package provider is the one contract every instance vendor is reached through. An adapter is
// a sub-package implementing Provider; nothing outside this tree names a vendor, and a vendor's
// own instance id never leaves its adapter except as the opaque Instance.ID handed back to it.
package provider

import (
	"context"
	"time"
)

type State string

const (
	// StateStarting covers everything between Create returning and the command running:
	// offer matching, image pull, boot.
	StateStarting State = "starting"
	StateRunning  State = "running"
	// StateExited means the command has ended and the instance still exists, so its logs can
	// still be read. An instance the vendor has removed is absent from List.
	StateExited State = "exited"
)

// Spec describes one instance. Name is the identity: adapters store it with the instance and
// List returns it unchanged, so a caller that keeps nothing between passes can still tell which
// instances are its own and what each one is for.
type Spec struct {
	Name    string
	Image   string
	Command []string
	Args    []string
	Env     map[string]string

	// AcceleratorType is a resource catalog string; an adapter translates it to the vendor's
	// own naming. AcceleratorCount 0 asks for a CPU-only instance.
	AcceleratorType  string
	AcceleratorCount int
	CPUMillis        int64
	MemoryBytes      int64
	DiskBytes        int64
}

type Instance struct {
	ID        string
	Name      string
	State     State
	StartedAt time.Time
}

// Provider rents and returns instances. Offer search, bidding, region choice and every other
// vendor concept stay inside the adapter.
type Provider interface {
	// Create rents an instance for spec and returns once List shows it under spec.Name: a caller
	// that finds no instance of a name creates one, so an instance missing from List for a moment
	// after Create is rented twice. The command need not have started; that is StateStarting.
	Create(ctx context.Context, spec Spec) (id string, err error)
	// List returns every instance whose name starts with namePrefix, and no others on the account.
	List(ctx context.Context, namePrefix string) ([]Instance, error)
	// Logs returns the last tail lines the instance's command wrote to stdout and stderr, oldest
	// first. It must work for an instance in StateExited.
	Logs(ctx context.Context, id string, tail int) ([]string, error)
	// Stop sends the instance's command SIGTERM and returns without waiting for it to end. The
	// instance stays in List: running until the command exits, StateExited after. Stopping an
	// instance that was already stopped, has exited or is gone is not an error.
	Stop(ctx context.Context, id string) error
	// Destroy ends the instance and everything billed for it. Destroying an instance that is
	// already gone is not an error.
	Destroy(ctx context.Context, id string) error
}
