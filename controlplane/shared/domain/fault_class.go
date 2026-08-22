package domain

// FaultClass is the verdict drawn from an eviction's typed reason: whose fault it was. Most of
// the vocabulary is terminal, but not all of it -- EvictionPreemptedForGuaranteed sends a job back
// to the queue -- so this is a statement about the reason, not about the row's finality.
// The platform's entire output is a ranking of agents, so this is a correctness concern before
// it is an operations one — without it an agent can be eliminated by a bad node, since a
// hardware failure costs the same quota and reads the same as its own bug.
type FaultClass string

const (
	// FaultWorkload: the job or its spec was wrong. Behaves exactly as it always has.
	FaultWorkload FaultClass = "workload"
	// FaultInfrastructure: the environment failed the job. Refunded, requeued without spending a
	// retry attempt, and excluded from the agent's failure record — it never chose to spend
	// those hours and this was not one of its own failures.
	FaultInfrastructure FaultClass = "infrastructure"
	// FaultPolicy: the platform decided, and the job was fine. Reported separately so a
	// stage-cut job stops reading as a failed one. This class matters as much as the other two:
	// those jobs did nothing wrong either, and lumping them in with failures is the same mistake
	// in a different place.
	FaultPolicy FaultClass = "policy"
	// FaultUnclassified is never assigned to a reason — the table below is exhaustive and a test
	// enforces it. It exists only so a breakdown computed over data written by a different build
	// of the control plane can account for a reason this one has never heard of, instead of
	// dropping it and quietly failing to sum to the total.
	FaultUnclassified FaultClass = "unclassified"
)

// faultClasses assigns every eviction reason exactly one class. It is deliberately a total
// function over the reason vocabulary rather than a switch with a default: a reason with no class
// is a test failure (see TestEveryEvictionReasonHasExactlyOneFaultClass, which reads the
// vocabulary out of the source rather than a hand-maintained list), so adding a sixteenth reason
// forces its author to decide what it means. That is the only way a taxonomy like this stays
// honest — a default would silently absorb every future reason into whichever class was
// convenient, which is how "infrastructure" quietly becomes a synonym for "unclassified".
var faultClasses = map[EvictionReason]FaultClass{
	// --- workload: the job or its spec was wrong ---------------------------------------------
	// A reporting path that never worked: wrong URL, a stale metrics helper baked into the
	// image, an exception the workload swallows. All of them the submission's own doing.
	EvictionNeverReportedMetrics: FaultWorkload,
	// A live pod that reported and then stopped — a hung process inside the container.
	EvictionSilent: FaultWorkload,
	// The agent chose an estimate the current stage's max_job_hours does not allow. The cap was
	// readable before submitting.
	EvictionJobTooLong: FaultWorkload,
	// The job's own spec is what makes it unplaceable alongside its neighbours — which is
	// exactly why this eviction is terminal rather than a requeue.
	EvictionResourceDisbalance: FaultWorkload,
	// A bad image reference or a malformed container config: NeverSelfHealsPhaseReasons.
	EvictionUnschedulable: FaultWorkload,

	// --- infrastructure: the environment failed the job --------------------------------------
	EvictionClusterUnreachable:          FaultInfrastructure,
	EvictionWorkloadGone:                FaultInfrastructure,
	EvictionAcceleratorTypeUnobservable: FaultInfrastructure,
	// The cluster placed the job on hardware other than what was reserved and billed. The
	// submission asked for one thing and the environment delivered another.
	EvictionFlavorMismatch: FaultInfrastructure,
	// Admitted against capacity the scheduler had confirmed, then never reached RUNNING. The
	// job's own never-self-healing faults (bad image, bad config) are split off into
	// EvictionUnschedulable above precisely so that what is left here is the environment failing
	// to deliver placement it had already been credited with.
	EvictionStuckPending: FaultInfrastructure,

	// --- policy: the platform decided, and the job was fine -----------------------------------
	EvictionStageCut:               FaultPolicy,
	EvictionQuotaExhaustion:        FaultPolicy,
	EvictionPreemptedForGuaranteed: FaultPolicy,
	EvictionExperimentClosed:       FaultPolicy,
	EvictionCancelled:              FaultPolicy,
}

// Classify returns the fault class of an eviction reason, and whether the reason is one this
// vocabulary knows. It strips any WithDetail suffix first, so classification is always driven by
// the typed code and never by message text.
//
// found=false is a real answer, not a class: callers must decide what an unrecognised reason
// means for them rather than being handed a default that reads as a verdict. In practice the
// completeness test makes this unreachable for anything in the vocabulary; what reaches it is a
// reason from an older or newer build of the control plane.
func Classify(reason EvictionReason) (FaultClass, bool) {
	class, ok := faultClasses[reason.Code()]
	return class, ok
}

// IsInfrastructureFault reports whether an eviction reason names the environment's fault — the one
// class that changes what happens: refunded, requeued without spending a retry attempt, and
// excluded from the agent's failure record.
func IsInfrastructureFault(reason EvictionReason) bool {
	class, ok := Classify(reason)
	return ok && class == FaultInfrastructure
}

// ClassifyCounts folds a by-reason tally into a by-class one. Derived at read time from the
// counts the stats path already produces rather than stored alongside them: a class is a pure
// function of a reason, so persisting both would be two records of one fact, free to disagree the
// first time a reason is reclassified.
//
// An unrecognised reason is counted under FaultUnclassified rather than dropped. Dropping it
// would make the classes silently fail to sum to the total, which is the one way this breakdown
// could mislead someone reading it — "no infrastructure failures" and "some failures we could not
// classify" must not look the same.
func ClassifyCounts(byReason map[string]int) map[FaultClass]int {
	byClass := map[FaultClass]int{}
	for reason, count := range byReason {
		class, found := Classify(EvictionReason(reason))
		if !found {
			class = FaultUnclassified
		}
		byClass[class] += count
	}
	return byClass
}
