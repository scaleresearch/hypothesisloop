// Package clusterapi is the wire contract between the control plane and the runtimes that pull
// desired state from /internal/clusters/{name}. It imports nothing of the platform: the control
// plane maps its own types onto these at the edge, and a runtime reads only these.
package clusterapi

import (
	"fmt"
	"time"
)

// MaxIdentifierLength bounds every id the control plane hands a runtime, at the DNS label limit
// Kubernetes object names are built from.
const MaxIdentifierLength = 63

// DesiredJob is one job a runtime is told should exist on its cluster: what it builds a workload
// from, and nothing it has no use for. A runtime may be another org's, so nothing about where
// the job sits in its org travels here.
type DesiredJob struct {
	ID         string `json:"id"`
	AgentID    string `json:"agent_id"`
	ProjectID  string `json:"project_id"`
	CodeRef    string `json:"code_ref"`
	ConfigHash string `json:"config_hash"`
	DataRef    string `json:"data_ref"`
	// Job is the job as submitted, with its secret_env. ResolvedJob is it as admission resolved
	// it onto this cluster, nil when nothing was resolved: read EffectiveJob.
	Job         JobSpec  `json:"job"`
	ResolvedJob *JobSpec `json:"resolved_job,omitempty"`
	// EnvironmentKey is set on a job that runs in an environment image, which is pulled with the
	// reconcile answer's registry_pull.
	EnvironmentKey         string          `json:"environment_key,omitempty"`
	AcceleratorType        AcceleratorType `json:"accelerator_type"`
	AcceleratorCount       int             `json:"accelerator_count"`
	EstimatedDurationHours float64         `json:"estimated_duration_hours"`
	CapacityTier           CapacityTier    `json:"capacity_tier"`
	// SubmittedAt is when the job was admitted, which a runtime that waits for a machine to rent
	// measures that wait from.
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
	// Data is the job's durable-data address and key, and Token its own platform token, good
	// for reporting its metrics and nothing else.
	Data  *DataAccess `json:"data,omitempty"`
	Token string      `json:"token,omitempty"`
	// AttemptCount numbers the job's generation. A requeue reuses the job's id and advances it,
	// so every attempt is a distinct desired state and its workload a distinct one.
	AttemptCount int `json:"attempt_count"`
}

// EffectiveJob is the job as it should be run: ResolvedJob when admission resolved one, Job
// otherwise.
func (j *DesiredJob) EffectiveJob() JobSpec {
	if j.ResolvedJob != nil {
		return *j.ResolvedJob
	}
	return j.Job
}

// CapacityTier is whether a job holds guaranteed or burst capacity.
type CapacityTier string

const (
	CapacityGuaranteed CapacityTier = "guaranteed"
	CapacityBurst      CapacityTier = "burst"
)

// DataAccess is where a job writes durable data, where it may read it, and the key for both.
type DataAccess struct {
	// URI is the job's own prefix, writable. Shared is its platform experiment's, readable.
	URI    string `json:"uri"`
	Shared string `json:"shared"`

	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`

	// The job's own permanent key, scoped by the store to exactly the two grants above.
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

// JobSpec is how a job runs, in the platform's own vocabulary; each runtime compiles it into its
// own native resource, deterministically.
type JobSpec struct {
	Image     string            `json:"image,omitempty"`
	Command   []string          `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	SecretEnv map[string]string `json:"secret_env,omitempty"`

	// CPU, Memory and Storage are resource quantities ("4", "16Gi") per node. Storage is
	// ephemeral scratch space.
	CPU     string `json:"cpu,omitempty"`
	Memory  string `json:"memory,omitempty"`
	Storage string `json:"storage,omitempty"`

	// AcceleratorCount is accelerators per node, not the job's total (TotalAccelerators).
	AcceleratorCount           int               `json:"accelerator_count"`
	AcceleratorType            AcceleratorType   `json:"accelerator_type,omitempty"`
	AcceptableAcceleratorTypes []AcceleratorType `json:"acceptable_accelerator_types,omitempty"`
	NodeSelector               map[string]string `json:"node_selector,omitempty"`
	AcceleratorTolerations     []string          `json:"accelerator_tolerations,omitempty"`
	AcceleratorPodResources    map[string]string `json:"accelerator_pod_resources,omitempty"`

	// Groups is a job whose nodes are not identical. Absent, the job is NumNodes identical nodes
	// of the top-level shape (NodeGroups).
	Groups   []JobGroup `json:"groups,omitempty"`
	NumNodes int        `json:"num_nodes,omitempty"`

	MaxRetries                    *int              `json:"max_retries,omitempty"`
	TerminationGracePeriodSeconds *int64            `json:"termination_grace_period_seconds,omitempty"`
	CheckpointGraceSeconds        *int64            `json:"checkpoint_grace_seconds,omitempty"`
	Topology                      *TopologySpec     `json:"topology,omitempty"`
	ShmSize                       string            `json:"shm_size,omitempty"`
	ExtraResources                map[string]string `json:"extra_resources,omitempty"`
	HostMounts                    map[string]string `json:"host_mounts,omitempty"`
}

// TopologySpec is where a distributed job's nodes land relative to each other.
type TopologySpec struct {
	// SpreadAcrossHosts keeps two nodes of the job off one host; nil means true for a job of
	// more than one node.
	SpreadAcrossHosts *bool `json:"spread_across_hosts,omitempty"`
	// SameZone prefers every node in one zone, best effort.
	SameZone bool `json:"same_zone,omitempty"`
}

// JobGroup is Replicas identical nodes of a job, with their own resources and process. Env is
// merged over the job's.
type JobGroup struct {
	Name             string            `json:"name"`
	Replicas         int               `json:"replicas"`
	CPU              string            `json:"cpu,omitempty"`
	Memory           string            `json:"memory,omitempty"`
	Storage          string            `json:"storage,omitempty"`
	AcceleratorCount int               `json:"accelerator_count,omitempty"`
	AcceleratorType  AcceleratorType   `json:"accelerator_type,omitempty"`
	Command          []string          `json:"command,omitempty"`
	Args             []string          `json:"args,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
}

// NodeGroups is the shapes the job consists of: its groups, or for an ungrouped job one group of
// the top-level shape repeated Nodes() times.
func (j JobSpec) NodeGroups() []JobGroup {
	if len(j.Groups) > 0 {
		return j.Groups
	}
	return []JobGroup{{
		Replicas:         j.Nodes(),
		CPU:              j.CPU,
		Memory:           j.Memory,
		Storage:          j.Storage,
		AcceleratorCount: j.AcceleratorCount,
		AcceleratorType:  j.AcceleratorType,
		Command:          j.Command,
		Args:             j.Args,
		Env:              j.Env,
	}}
}

// TotalAccelerators is the job's accelerators across all its nodes.
func (j JobSpec) TotalAccelerators() int {
	total := 0
	for _, g := range j.NodeGroups() {
		total += g.AcceleratorCount * g.Replicas
	}
	return total
}

// Nodes is how many nodes the job spans: the sum of its groups' replicas, or NumNodes floored
// at 1.
func (j JobSpec) Nodes() int {
	if len(j.Groups) > 0 {
		total := 0
		for _, g := range j.Groups {
			total += g.Replicas
		}
		return total
	}
	if j.NumNodes < 1 {
		return 1
	}
	return j.NumNodes
}

// ResolvedGrace is the grace a runtime applies, from a job admission resolved: the ordinary
// shutdown grace and the checkpoint window (0 when the job declared none). A job without a
// shutdown grace was not resolved by admission and is refused.
func (j JobSpec) ResolvedGrace() (shutdown, checkpoint int64, err error) {
	if j.TerminationGracePeriodSeconds == nil {
		return 0, 0, fmt.Errorf("job spec carries no resolved termination grace")
	}
	if j.CheckpointGraceSeconds != nil {
		checkpoint = *j.CheckpointGraceSeconds
	}
	return *j.TerminationGracePeriodSeconds, checkpoint, nil
}
