package domain

import (
	"fmt"
	"regexp"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

// The job vocabulary is the cluster contract's own (clusterapi).
type (
	JobSpec      = clusterapi.JobSpec
	JobGroup     = clusterapi.JobGroup
	TopologySpec = clusterapi.TopologySpec
)

// GroupAcceleratorType is the one accelerator type a grouped job runs on, or "" if no group
// names one. ValidateGroups has already established that at most one distinct type is named.
func GroupAcceleratorType(j JobSpec) AcceleratorType {
	for _, g := range j.Groups {
		if g.AcceleratorType != "" {
			return g.AcceleratorType
		}
	}
	return ""
}

// ValidateGroups enforces everything structural about Groups. It is a no-op for an ungrouped
// job, which is what keeps every existing submission behaving exactly as before.
//
// Returns a plain error; the admission layer wraps it in its own typed rejection.
func ValidateGroups(j JobSpec) error {
	if len(j.Groups) == 0 {
		return nil
	}
	// One way to say a thing. Accepting both forms would mean inventing merge rules — does
	// num_nodes multiply the groups? does a top-level cpu apply to a group that omitted one? —
	// and every answer is a rule someone has to remember.
	if j.NumNodes != 0 {
		return fmt.Errorf("job.num_nodes cannot be combined with job.groups: a grouped job's node count is the sum of its groups' replicas")
	}
	if j.CPU != "" || j.Memory != "" || j.Storage != "" || j.AcceleratorCount != 0 {
		return fmt.Errorf("job.cpu/memory/storage/accelerator_count are per-node fields and cannot be combined with job.groups: state them on each group instead")
	}
	names := make(map[string]bool, len(j.Groups))
	acceleratorType := j.AcceleratorType
	for _, g := range j.Groups {
		if g.Name == "" {
			return fmt.Errorf("every job.groups entry needs a name")
		}
		if !groupNamePattern.MatchString(g.Name) {
			return fmt.Errorf("job.groups[%q].name must be lowercase alphanumeric with '-', starting and ending alphanumeric (it names a resource in the execution cluster)", g.Name)
		}
		if names[g.Name] {
			return fmt.Errorf("job.groups contains %q more than once", g.Name)
		}
		names[g.Name] = true
		if g.Replicas < 1 {
			return fmt.Errorf("job.groups[%q].replicas must be at least 1", g.Name)
		}
		if g.CPU == "" || g.Memory == "" || g.Storage == "" {
			return fmt.Errorf("job.groups[%q] states its cpu, memory and storage", g.Name)
		}
		if g.AcceleratorCount < 0 {
			return fmt.Errorf("job.groups[%q].accelerator_count must not be negative", g.Name)
		}
		if g.AcceleratorType == "" {
			continue
		}
		// Groups may differ in accelerator COUNT, including zero, but never in TYPE: the
		// experiment carries a single AcceleratorType that admission, accelerator type substitution and
		// billing all key on, and a per-group set would have to be threaded through every one
		// of those paths for a case nothing has asked for.
		if acceleratorType == "" {
			acceleratorType = g.AcceleratorType
			continue
		}
		if g.AcceleratorType != acceleratorType {
			return fmt.Errorf("job.groups[%q] wants accelerator type %q but the job already runs on %q — a job runs on exactly one accelerator type", g.Name, g.AcceleratorType, acceleratorType)
		}
	}
	if acceleratorType == "" && j.TotalAccelerators() > 0 {
		return fmt.Errorf("job.accelerator_type is required when any group requests accelerators")
	}
	return nil
}

const (
	// MaxSecretEnvEntries and MaxSecretEnvBytes bound a job's secret_env, the names and values
	// together, so one submission cannot grow the row every read of the job loads.
	MaxSecretEnvEntries = 64
	MaxSecretEnvBytes   = 64 << 10
)

var secretEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateSecretEnv enforces that secret is a bounded set of environment variable names none of
// which the job also sets in the open: a name in both would leave which value the workload gets
// to the order two maps are merged in.
func ValidateSecretEnv(j JobSpec, secret map[string]string) error {
	if len(secret) > MaxSecretEnvEntries {
		return fmt.Errorf("job.secret_env holds %d entries, more than the %d allowed", len(secret), MaxSecretEnvEntries)
	}
	size := 0
	for name, value := range secret {
		if !secretEnvNamePattern.MatchString(name) {
			return fmt.Errorf("job.secret_env name %q is not an environment variable name: letters, digits and '_', not starting with a digit", name)
		}
		if value == "" {
			return fmt.Errorf("job.secret_env[%q] is empty", name)
		}
		if _, open := j.Env[name]; open {
			return fmt.Errorf("%q is in both job.env and job.secret_env: state it in one", name)
		}
		for _, g := range j.Groups {
			if _, open := g.Env[name]; open {
				return fmt.Errorf("%q is in both job.groups[%q].env and job.secret_env: state it in one", name, g.Name)
			}
		}
		size += len(name) + len(value)
	}
	if size > MaxSecretEnvBytes {
		return fmt.Errorf("job.secret_env is %d bytes of names and values, more than the %d allowed", size, MaxSecretEnvBytes)
	}
	return nil
}

// groupNamePattern is the RFC 1123 DNS label rule: a group name is appended to the experiment id
// to name the native resource each backend creates for that group, so a name k8s would reject
// has to be rejected here rather than left to fail forever in a reconcile loop.
var groupNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ExperimentMeta holds the fields a submission needs to describe research intent and
// bookkeeping around a run — never how it executes (that's JobSpec).
type ExperimentMeta struct {
	AgentID              string  `json:"agent_id" yaml:"agent_id"`
	PlatformExperimentID string  `json:"platform_experiment_id" yaml:"platform_experiment_id"`
	ParentID             *string `json:"parent_id,omitempty" yaml:"parent_id,omitempty"`

	// HypothesisID is the hypothesis the job tests, registered before it via POST /hypotheses.
	// The job of a baseline agent states none.
	HypothesisID string `json:"hypothesis_id" yaml:"hypothesis_id" required:"false" doc:"The hypothesis the job tests. Required of every job but a baseline agent's, which states none."`
	// Hypothesis is populated by the server (scheduler.Service.Submit denormalizes it from the
	// registered hypothesis's own Text, looked up by HypothesisID) and is never read from the
	// request — optional and ignored if a caller sends one. Kept only so an already-persisted
	// Experiment can round-trip through this same struct on read paths.
	Hypothesis string `json:"hypothesis,omitempty" yaml:"hypothesis,omitempty"`
	Theory     string `json:"theory,omitempty" yaml:"theory,omitempty"`

	CodeRef string `json:"code_ref" yaml:"code_ref"`
	Branch  string `json:"branch" yaml:"branch" required:"false" doc:"The git branch code_ref was pushed on, under hl/<platform_experiment_id>/. Every job states one: 422 branch_not_of_experiment otherwise."`
	// ConfigHash/DataRef are optional reproducibility pointers (a hash of the resolved
	// hyperparameter config, where the input dataset lives) — pure bookkeeping, stored and
	// returned verbatim, never validated or required by admission. Leave empty if you have
	// nothing to point at.
	ConfigHash string `json:"config_hash,omitempty" yaml:"config_hash,omitempty"`
	DataRef    string `json:"data_ref,omitempty" yaml:"data_ref,omitempty"`

	// CapacityTier picks guaranteed (FIFO, can preempt burst) vs. burst (best-effort,
	// preemptable) scheduling, translated into the backend's native priority mechanism (a k8s
	// PriorityClass today). Optional — scheduler.Service.Submit defaults an empty value to
	// guaranteed.
	CapacityTier           CapacityTier `json:"capacity_tier,omitempty" yaml:"capacity_tier,omitempty"`
	EstimatedDurationHours float64      `json:"estimated_duration_hours,omitempty" yaml:"estimated_duration_hours,omitempty" required:"false" doc:"The hours the job is expected to run, which it holds of its agent's quota until it ends. Left out, it is the one the platform measured on the platform experiment's baseline job, and the submit is refused where there is none."`
}
