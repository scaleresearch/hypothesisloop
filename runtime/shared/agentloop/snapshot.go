package agentloop

import (
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// A reconcile pass converges two snapshots: what the control plane wants, and what this runtime
// actually holds. Building those two indexes is where a malformed feed has to be dealt with, and
// the rule is not the obvious one, so it lives here in two pure functions rather than inline in
// the pass — the interesting cases are combinations of bad rows, and they are worth testing
// without an HTTP server and a fake executor standing in the way.
//
// The rule: a row that cannot be acted on is skipped rather than ending the pass, because one
// unusable entry must not stop every other workload on the cluster from being reconciled
// (important.md). But skipping is only safe for work that is IDENTITY-based. Two of the pass's
// operations are ABSENCE-based and read a skipped row as a decision:
//
//   - a workload absent from the desired index is deleted as no longer desired, so a desired row
//     whose identity was lost in transit would get its still-running job destroyed;
//   - a workload absent from the actual index is created, so an actual workload that could not be
//     named would be created a second time, either doubling its capacity or colliding forever.
//
// So each index reports whether it is still complete enough to argue from absence. An incomplete
// one does not stop the pass; it withdraws exactly the one conclusion it can no longer support,
// and the next pass — seconds later — has the whole picture again.

// desiredSnapshot is the control plane's wanted set, indexed by experiment id.
type desiredSnapshot struct {
	byID map[string]*domain.Experiment
	// complete is false once any row was dropped, which is what makes "absent from byID" stop
	// meaning "no longer desired".
	complete bool
	errs     []error
}

// actualSnapshot is what this runtime currently holds, as a set of experiment ids.
type actualSnapshot struct {
	ids map[string]bool
	// complete is false once any workload could not be named, which is what makes "absent from
	// ids" stop meaning "does not exist yet".
	complete bool
	errs     []error
}

// indexDesired builds the desired index, quarantining anything ambiguous.
//
// A duplicate id is ambiguous rather than unusable: the copies may differ in placement, image or
// resources, and picking one would make the pass reconcile against whichever the feed happened to
// list first — deleting a correct workload as drifted on one pass and recreating it on the next.
// The id is therefore dropped entirely, and it STAYS dropped: quarantining by removing it from
// the map alone would reopen on a third copy, which would find the id absent and install itself
// as though it were the only one.
func indexDesired(desired []*domain.Experiment) desiredSnapshot {
	snap := desiredSnapshot{byID: make(map[string]*domain.Experiment, len(desired)), complete: true}
	quarantined := make(map[string]bool)
	for _, exp := range desired {
		if exp == nil || exp.ID == "" {
			snap.errs = append(snap.errs, fmt.Errorf("desired state contains experiment without identity"))
			snap.complete = false
			continue
		}
		if quarantined[exp.ID] {
			// Already known to be ambiguous. Reported once, on the copy that established it.
			continue
		}
		if _, exists := snap.byID[exp.ID]; exists {
			snap.errs = append(snap.errs, fmt.Errorf("desired state contains duplicate experiment %q", exp.ID))
			delete(snap.byID, exp.ID)
			quarantined[exp.ID] = true
			snap.complete = false
			continue
		}
		snap.byID[exp.ID] = exp
	}
	return snap
}

// indexActual builds the set of workloads this runtime holds.
//
// A duplicate id needs no quarantine, unlike the desired side: reconciliation is identity-based
// and a delete removes every workload carrying that identity, so the copy is already accounted
// for by the entry that is present.
func indexActual(actual []string) actualSnapshot {
	snap := actualSnapshot{ids: make(map[string]bool, len(actual)), complete: true}
	for _, id := range actual {
		if id == "" {
			snap.errs = append(snap.errs, fmt.Errorf("actual state contains workload without experiment identity"))
			snap.complete = false
			continue
		}
		if snap.ids[id] {
			snap.errs = append(snap.errs, fmt.Errorf("actual state contains duplicate experiment identity %q", id))
			continue
		}
		snap.ids[id] = true
	}
	return snap
}
