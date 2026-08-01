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

	// ManagedByValue is the constant value ManagedBy is set to, identifying this system as the
	// writer (as opposed to which key is being written).
	ManagedByValue = "hypothesisloop"
)
