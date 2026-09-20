// Package provider is the one contract every instance vendor is reached through. An adapter is
// a sub-package implementing Provider; nothing outside this tree names a vendor, and a vendor's
// own instance id never leaves its adapter except as the opaque Instance.ID handed back to it.
package provider

import (
	"context"
	"fmt"
	"regexp"
	"time"
)

type State string

const (
	// StateStarting covers everything between Create returning and the command running:
	// offer matching, image pull, boot.
	StateStarting State = "starting"
	StateRunning  State = "running"
	// StateExited means the command has ended, the instance still exists, and everything the
	// command wrote is readable through Logs. An adapter whose vendor delivers output late keeps
	// reporting StateRunning until the output is complete: a caller reads how the command ended
	// from the last lines, once, when it first sees StateExited. An instance the vendor has
	// removed is absent from List.
	StateExited State = "exited"
)

// MaxNameLength is the longest Spec.Name a caller may ask for.
const MaxNameLength = 128

var namePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// ValidateName is the rule every Spec.Name meets: lowercase letters, digits and dashes, at most
// MaxNameLength long. An adapter whose vendor cannot hold such a name as it is (a shorter limit,
// another alphabet) keeps its own mapping and still returns the name unchanged from List.
func ValidateName(name string) error {
	if len(name) > MaxNameLength || !namePattern.MatchString(name) {
		return fmt.Errorf("instance name %q must be 1 to %d characters of a-z, 0-9 and dash", name, MaxNameLength)
	}
	return nil
}

// Spec describes one instance. Name is the identity: adapters store it with the instance and
// List returns it unchanged, so a caller that keeps nothing between passes can still tell which
// instances are its own and what each one is for. It meets ValidateName. A vendor does not have
// to keep names unique, and a caller must expect two instances of one name.
type Spec struct {
	Name  string
	Image string
	// Command is the whole argv the instance runs, in place of the image's entrypoint and
	// arguments. Every element reaches the process as one argument, unchanged.
	Command []string
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
	// A Create that returns an error has left either nothing or an instance that List shows under
	// spec.Name, never one a caller cannot find.
	Create(ctx context.Context, spec Spec) (id string, err error)
	// List returns every instance whose name starts with namePrefix, and no others on the account.
	List(ctx context.Context, namePrefix string) ([]Instance, error)
	// Logs returns the last tail lines the instance's command wrote to stdout and stderr, oldest
	// first. It must work for an instance in StateExited.
	Logs(ctx context.Context, id string, tail int) ([]string, error)
	// Stop sends the instance's command SIGTERM and returns without waiting for it to end. The
	// instance stays in List: StateRunning until the command exits, StateExited after, with its
	// logs still readable. The command alone decides when it exits: an adapter must not follow
	// the signal with a kill of its own after some timeout, because how long the command may take
	// is kept by the wrapper inside the instance. Stop is repeated on every pass while the
	// instance runs, so stopping one that was already stopped, has exited or is gone is not an
	// error.
	Stop(ctx context.Context, id string) error
	// Destroy ends the instance and everything billed for it. Destroying an instance that is
	// already gone is not an error.
	Destroy(ctx context.Context, id string) error
}
