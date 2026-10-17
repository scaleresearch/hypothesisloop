package clusterapi

import "time"

// The per-node dimensions a cluster reports capacity for, alongside its per-node accelerator
// counts. A mistyped key is no error, it is a silently absent dimension, so both sides name them
// here.
const (
	NodeResourceCPUMillicores = "cpu_millicores"
	NodeResourceMemoryBytes   = "memory_bytes"
	NodeResourceStorageBytes  = "storage_bytes"
)

// Node labels under NodeLabelReservedPrefix are the platform's.
const (
	NodeLabelReservedPrefix = "hypothesisloop.io/"
	// NodeSlotLabel marks a node that holds one job at a time, and names it. A runtime that
	// rents a machine per job reports each machine it may rent as such a node, and a job
	// admitted to one carries the label in its resolved node selector.
	NodeSlotLabel = NodeLabelReservedPrefix + "slot"
	// NodeSlotAcceleratorsLabel is how many accelerators a slot has, taken or free.
	NodeSlotAcceleratorsLabel = NodeLabelReservedPrefix + "slot-accelerators"
	// NodeSlotNoOfferLabel is on a slot no instance holds and no machine can be rented for right
	// now. Its value is the runtime's reason, in words a person reads.
	NodeSlotNoOfferLabel = NodeLabelReservedPrefix + "slot-no-offer"
	// NodeArchitectureLabel is on every node a runtime reports: the architecture of its CPU as Go
	// and image manifests name it (amd64, arm64). A job runs only on a node of its image's.
	NodeArchitectureLabel = "kubernetes.io/arch"
)

// ReasonClusterNotRegistered is the reason on the 401 a runtime token gets once its cluster is
// no longer registered. A runtime reads it as its cluster having been removed: it stops the
// workloads it holds and exits. Any other 401 is a token somebody has to fix.
const ReasonClusterNotRegistered = "cluster_not_registered"

// AgentVersionHeader carries, on every request a cluster's agent makes, the version it runs: the
// image tag it was installed with. The control plane keeps the one of the cluster's last contact
// and shows a cluster whose version is not its own.
const AgentVersionHeader = "X-Hypothesisloop-Agent-Version"

// ReconcileRequest is what a runtime tells the control plane with every reconcile
// (POST /internal/clusters/{name}/reconcile): its live capacity and what it can do.
type ReconcileRequest struct {
	CPUAvailableCores          float64                     `json:"cpu_available_cores"`
	CPUTotalCores              float64                     `json:"cpu_total_cores"`
	AcceleratorAvailableByType map[string]int64            `json:"accelerator_available_by_type"`
	AcceleratorTotalByType     map[string]int64            `json:"accelerator_total_by_type"`
	AcceleratorAvailableByNode map[string]map[string]int64 `json:"accelerator_available_by_node"`
	// NodeResourcesByNode is free CPU, memory and storage per node, keyed by NodeResource*. A
	// job runs on one node and must fit that node in every dimension.
	NodeResourcesByNode map[string]map[string]int64 `json:"node_resources_by_node"`
	// NodeResourcesTotalByNode is each node's installed amount of the same, the stable
	// denominator free capacity cannot be.
	NodeResourcesTotalByNode map[string]map[string]int64  `json:"node_resources_total_by_node"`
	NodeLabelsByNode         map[string]map[string]string `json:"node_labels_by_node"`
	// MultiNodeCapable is whether this runtime can execute a job spanning more than one node.
	// Absent means single-node only.
	MultiNodeCapable bool `json:"multi_node_capable"`
	// AutoscalerEnabled is the operator's claim that this cluster sits behind a native
	// autoscaler that reacts to pending pods. Absent means it does not.
	AutoscalerEnabled bool `json:"autoscaler_enabled"`
	// ClusterID is a stable fingerprint the runtime derives live and never persists itself: the
	// kube-system namespace UID on k8s, /etc/machine-id on bare metal. It identifies the cluster
	// across a rename of its name.
	ClusterID             string `json:"cluster_id"`
	RAMAvailableBytes     int64  `json:"ram_available_bytes"`
	RAMTotalBytes         int64  `json:"ram_total_bytes"`
	StorageAvailableBytes int64  `json:"storage_available_bytes"`
	StorageTotalBytes     int64  `json:"storage_total_bytes"`
}

// ReconcileAnswer is one reconcile's whole answer: what should exist on the cluster, and which of
// the things that should no longer exist are entitled to their checkpoint window on the way out.
type ReconcileAnswer struct {
	Jobs []*DesiredJob `json:"experiments"`
	// CheckpointWindow names the jobs whose termination was the platform's own decision: ids and
	// nothing else. How long each window is was declared on the job's own spec.
	CheckpointWindow []string `json:"checkpoint_window"`
}

// StatusPush is one complete snapshot of a cluster's jobs (POST /internal/clusters/{name}/status).
// A job missing from it reads as gone.
type StatusPush struct {
	Reports []StatusReport `json:"reports"`
}

// StatusAck is the control plane's answer to a StatusPush that it took: Status "ok".
type StatusAck struct {
	Status string `json:"status"`
}

// StatusReport is one job's observation in a StatusPush. Reason, Message and RestartCount are
// the runtime's explanation for why the job's container hasn't started or has been restarting,
// Reason one of the PhaseReason* constants.
type StatusReport struct {
	ExperimentID string `json:"experiment_id"`
	// Phase is JobPhase.String: pending, running, succeeded, failed or gone.
	Phase                   string `json:"phase"`
	AdmittedAcceleratorType string `json:"admitted_accelerator_type,omitempty"`
	AdmittedNode            string `json:"admitted_node,omitempty"`
	// AdmittedMachine is what stands behind AdmittedNode, from a runtime that knows it. It goes
	// only with a node.
	AdmittedMachine *JobMachine `json:"admitted_machine,omitempty"`
	LogTail         []string    `json:"log_tail,omitempty"`
	Reason          string      `json:"reason,omitempty"`
	Message         string      `json:"message,omitempty"`
	RestartCount    int32       `json:"restart_count,omitempty"`
	// ScheduledNodes is how many of the job's pods are bound to a node.
	ScheduledNodes int32 `json:"scheduled_nodes,omitempty"`
	// Attempt is the generation of the workload this observation came from, DesiredJob's
	// AttemptCount carried on the workload. Absent is AttemptUnknown, which is no attempt 0.
	Attempt *int `json:"attempt,omitempty"`
	// StartedAt and EndedAt are when the job's command started and ended, from a runtime whose
	// own look at the workload cannot time it. Sent together, once the command has ended.
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// JobMachine is the machine a job's workload runs on, in terms no vendor owns, each field as far
// as the runtime knows it.
type JobMachine struct {
	// InstanceID is the rented instance, which is the job's alone.
	InstanceID string `json:"instance_id,omitempty"`
	// HostID is the physical machine, the same for every instance rented on it.
	HostID           string `json:"host_id,omitempty" doc:"The physical machine, stable across rentals: every instance rented on it reports the same one. It is not the account that owns the machine."`
	AcceleratorModel string `json:"accelerator_model,omitempty"`
	CPUModel         string `json:"cpu_model,omitempty"`
	// PricePerHourUSD is what the whole instance is rented for.
	PricePerHourUSD float64 `json:"price_per_hour_usd,omitempty" minimum:"0"`
}

// The paths of a cluster's API. Each of a cluster's own is under ClustersPath + "/{name}".
const (
	ClustersPath  = "/internal/clusters"
	AccessPath    = "/access"
	ReconcilePath = "/reconcile"
	StatusPath    = "/status"
	// NodeMetricsPath is where a node's agent pushes NodeMetricsPush, on the metrics service.
	NodeMetricsPath = "/internal/node-metrics"
)
