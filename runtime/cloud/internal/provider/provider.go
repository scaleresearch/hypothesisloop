// Package provider is the contract between what rents machines (the cloud cluster's runtime for
// jobs, the launcher for hosted agents) and a vendor's adapter.
//
// Provider is the required core, kept to what every vendor can do: create, list, read output,
// stop, destroy. What only some vendors can do is a capability: a small interface of its own
// that an adapter implements when its vendor has the thing natively, and leaves out otherwise.
// The rules a capability follows:
//
//   - It is found by a type assertion on the Provider (Pauser is `p.(Pauser)`), once, when the
//     process starts. A process says which capabilities its provider has in its start line
//     (Capabilities), and a caller that needs one it lacks refuses to start with that as the
//     reason. No adapter stubs a capability out, and no caller emulates one.
//   - It names no vendor concept, as the core does not. What the vendor calls it, which of its
//     variants is used and what it costs stay in the adapter.
//   - Whatever it changes about an instance shows in List, in terms a caller that keeps nothing
//     between passes can act on. A new condition is a new State.
//   - Its calls are repeatable, like Stop and Destroy: asking twice, or of an instance that is
//     gone, is not an error.
//   - It has a check in providertest that every adapter claiming it runs against its real
//     vendor, and fake implements it.
//   - A wrapper of a Provider passes on the capabilities of what it wraps (Bounded does).
//
// Pauser, Market and Warmer are the capabilities there are. The ones foreseen, to be shaped the same way when
// something needs them and not before:
//
//   - Snapshotter: Snapshot(ctx, id) (ref, error) and a Spec field naming a snapshot to create
//     from, for a vendor that can copy an instance's filesystem. The harness's own session
//     snapshot covers what an agent needs across a replacement, on every vendor.
//   - Execer: Exec(ctx, id, argv) (output, error), a command run beside the instance's own.
//     Nothing on the platform reaches into a running instance today, and the tests that freeze a
//     harness call the vendor directly.
package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

type State string

const (
	// StateStarting covers everything between Create returning and the command running:
	// offer matching, image pull, boot. An instance whose command has run is never starting
	// again, unless a Pauser paused it in between.
	StateStarting State = "starting"
	StateRunning  State = "running"
	// StateUnknown is an instance that exists and is not known to be running: the vendor cannot
	// reach its host, is moving or restarting it, or reports a state the adapter was not written
	// for. It is rented and billed, its command may be running, and Logs need not work for it. An
	// adapter maps every vendor state it knows to one State by name and everything else to this
	// one, and never to StateStarting, which a caller bounds with its boot timeout.
	StateUnknown State = "unknown"
	// StateExited means the command has ended, the instance still exists, and everything the
	// command wrote is readable through Logs. An adapter whose vendor delivers output late keeps
	// reporting StateRunning until the output is complete: a caller reads how the command ended
	// from the last lines, once, when it first sees StateExited. An instance the vendor has
	// removed is absent from List.
	StateExited State = "exited"
	// StatePaused is an instance a Pauser paused: it exists, keeps its name and its disk, runs
	// nothing and is not billed for compute. Logs need not work for it. Only an adapter that
	// implements Pauser reports it.
	StatePaused State = "paused"
	// StateWarm is an instance a Warmer warmed and made ready: it exists, runs nothing and is
	// billed for its disk only, until StartWarm gives it a spec. Only an adapter that implements
	// Warmer reports it.
	StateWarm State = "warm"
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
	// ImageLogin is what Image is pulled with, nil for a public image. An adapter whose vendor
	// pulls only public images refuses a spec that has one.
	ImageLogin *ImageLogin
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

	// AvoidHosts are hosts the instance is not rented on, as Instance.Host named them: ones the
	// caller found unable to start an instance. A vendor that names no host in List is never
	// given any.
	AvoidHosts []string
}

// ImageLogin is a login to the registry host Registry.
type ImageLogin struct {
	Registry, Username, Password string
}

// ErrNoOffer is a Create that rented nothing because the vendor has no machine for the spec
// right now. Nothing is left behind, and a later Create may find one.
var ErrNoOffer = errors.New("no offer matches")

type Instance struct {
	ID        string
	Name      string
	State     State
	StartedAt time.Time
	// Expired says of an exited instance that its vendor's own limit on how long it rents one
	// ended the command, sooner than a caller asked for: the command did nothing wrong, and a
	// caller that still wants it rents another. Only an adapter whose vendor has such a limit
	// sets it. A caller that does not look in time finds the instance gone instead, as the
	// vendor removes it soon after.
	Expired bool
	// What the instance runs on, each as far as the vendor says and empty or zero otherwise.
	// Host is the vendor's id of the physical machine, the same for every instance rented on
	// it. The models are the vendor's own names for them. PricePerHour is in US dollars, for
	// the whole instance.
	Host             string
	AcceleratorModel string
	CPUModel         string
	PricePerHour     float64
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
	// Get reads the instance id itself, as List would show it, and exists is false when the
	// vendor says it is gone. A vendor's list can trail its instances and leave out one that
	// runs; Get is what tells such an instance from one that is gone. An error says neither.
	Get(ctx context.Context, id string) (instance Instance, exists bool, err error)
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
