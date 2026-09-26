package provider

import "context"

// Pauser is the capability of setting an instance aside without giving it up: a paused instance
// keeps its identity and its disk and is not billed for compute, for as long as the vendor keeps
// it.
//
// What a caller may rely on after Resume is the weaker of what vendors give: the command runs
// again under the same instance, and it is either the same process continuing or the Spec's
// command started anew on a filesystem that may be the image's again. A command that must
// survive a pause therefore keeps what it needs outside the instance, as it must to survive a
// replacement. The agent harness does: it stores its session and restores it at start.
type Pauser interface {
	// Pause sets a running instance aside and returns once the vendor has taken the request.
	// List shows StateRunning until the pause holds and StatePaused after. Pausing an instance
	// that is paused or gone is not an error. An instance the vendor cannot pause as it was
	// rented (its size, its state) is an error that says so.
	Pause(ctx context.Context, id string) error
	// Resume brings a paused instance back and returns once the vendor has taken the request.
	// List shows StateStarting or StateRunning after. Resuming an instance that runs is not an
	// error. Resuming one that is gone is: the caller asked for something that cannot happen.
	Resume(ctx context.Context, id string) error
}

// CapabilityPause is the name Capabilities gives Pauser.
const CapabilityPause = "pause"

// Capabilities names what p can do beyond the core, for a process to say at start.
func Capabilities(p Provider) []string {
	found := []string{}
	if _, ok := p.(Pauser); ok {
		found = append(found, CapabilityPause)
	}
	return found
}

// PausedAsStarting is the list as a caller that never pauses reads it: an instance somebody
// paused by other means is one that is not running yet, so it keeps its name, is not rented a
// second time, and the caller's own limit on starting bounds it.
func PausedAsStarting(listed []Instance) []Instance {
	for i := range listed {
		if listed[i].State == StatePaused {
			listed[i].State = StateStarting
		}
	}
	return listed
}
