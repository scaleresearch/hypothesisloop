// Package workloadkeys is the catalog of label/annotation key strings both k8sexec and podexec
// stamp onto the workloads they create, so the two backends never drift on the wire vocabulary
// the reconcile/status loop (runtime/shared/agentloop) and reapers key off of. Each backend
// decides for itself whether a given key goes on a label or an annotation (k8sexec puts a few on
// annotations instead of labels — see k8sexec.AcceleratorTypeAnnotation — because Kubernetes
// label values reject characters domain.AcceleratorType strings can contain).
package workloadkeys

const (
	ManagedBy        = "hypothesisloop.io/managed-by"
	ExperimentID     = "hypothesisloop.io/experiment-id"
	AgentID          = "hypothesisloop.io/agent-id"
	CapacityTier     = "hypothesisloop.io/capacity-tier"
	AcceleratorType  = "hypothesisloop.io/accelerator-type"
	AcceleratorCount = "hypothesisloop.io/accelerator-count"
	DesiredSpecHash  = "hypothesisloop.io/desired-spec-hash"
	CPUCoresMilli    = "hypothesisloop.io/cpu-cores-milli"
	MemoryBytes      = "hypothesisloop.io/memory-bytes"
	StorageBytes     = "hypothesisloop.io/storage-bytes"
	GraceSeconds     = "hypothesisloop.io/grace-seconds"
	// CheckpointGraceSeconds is the window a policy-class termination gives this workload to
	// write a checkpoint, already capped by configuration. Stamped on at creation because the
	// runtime deleting a workload has only its identity to go on, and the job's declaration is
	// long gone from desired state by then.
	CheckpointGraceSeconds = "hypothesisloop.io/checkpoint-grace-seconds"
	// Attempt is the generation of the experiment this workload was created for
	// (domain.Experiment.AttemptCount, also injected as HYPOTHESISLOOP_ATTEMPT). Reported back
	// with the workload's phase so the control plane can tell an observation of the attempt it
	// is waiting on from one of the attempt it just replaced — a retry that lands back on the
	// cluster it failed on leaves the old workload terminating and still in the status snapshot.
	Attempt = "hypothesisloop.io/attempt"
	// JobGroup names which group of a heterogeneous job a workload belongs to (see
	// domain.JobSpec.Groups). Absent on an ungrouped job, whose nodes are all the same thing.
	JobGroup = "hypothesisloop.io/job-group"

	// ManagedByValue is the constant value ManagedBy is set to, identifying this system as the
	// writer (as opposed to which key is being written).
	ManagedByValue = "hypothesisloop"
)
