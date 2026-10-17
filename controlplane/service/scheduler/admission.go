package scheduler

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/jobspec"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// codeRefPattern requires an immutable pointer — a full commit SHA, never a branch name or
// "latest". Matches "<any-git-remote-url>@<40-hex-char-sha>"; only the "@<sha>" suffix is enforced.
var codeRefPattern = regexp.MustCompile(`^\S+@[0-9a-f]{40}$`)

// Admission error reason constants.
const (
	ReasonInsufficientQuota = db.RefusedInsufficientQuota
	ReasonDuplicate         = "duplicate"
	ReasonMalformed         = "malformed"
	ReasonRateLimited       = db.RefusedRateLimited
	ReasonJobTooLong        = "job_too_long"
	ReasonDataQuotaExceeded = "data_quota_exceeded"
	// ReasonRequired refuses a job that leaves out what every job states, the field named in its
	// problems: its image, the cpu, memory and storage of each node as quantities, and an estimate
	// where none was measured.
	ReasonRequired = "required"
)

// AdmissionError is returned when an experiment cannot be admitted. Field names the field of the
// job it concerns, when it concerns one.
type AdmissionError struct {
	Reason  string
	Message string
	Field   string
}

// Refusal is the refusal the API answers e with, with status.
func (e *AdmissionError) Refusal(status int) *refusal.Refusal {
	refused := refusal.Refuse(status, e.Reason, e.Message)
	if e.Field != "" {
		refused.On(e.Field)
	}
	return refused
}

func (e *AdmissionError) Error() string {
	return fmt.Sprintf("admission rejected [%s]: %s", e.Reason, e.Message)
}

// totalOverGroups sums one per-replica dimension across every node group, each group's value
// weighted by its replica count. An ungrouped job has a single group of Nodes() identical
// replicas, so this is the "per-node value x node count" the per-job caps have always compared
// against; a grouped job's total is the sum of its groups, never a multiple of the largest.
func totalOverGroups(spec domain.JobSpec, parse func(string) (float64, error), pick func(domain.JobGroup) string) (float64, error) {
	var total float64
	for _, group := range spec.NodeGroups() {
		value, err := parse(pick(group))
		if err != nil {
			return 0, err
		}
		total += value * float64(group.Replicas)
	}
	return total, nil
}

// quantitiesStated refuses a job whose nodes do not state cpu, memory and storage as
// jobspec.CheckNode has them, checked per node because the cap checks sum over the nodes, where
// one node's negative would cancel another's real demand.
func quantitiesStated(job domain.JobSpec) error {
	check := func(owner string, group domain.JobGroup) error {
		err := jobspec.CheckNode(owner, group.CPU, group.Memory, group.Storage)
		switch {
		case err == nil:
			return nil
		case err.Unstated:
			return &AdmissionError{Reason: ReasonRequired, Field: err.Field, Message: err.Message}
		}
		return &AdmissionError{Reason: ReasonMalformed, Field: err.Field, Message: err.Message}
	}
	if len(job.Groups) == 0 {
		return check("job", domain.JobGroup{CPU: job.CPU, Memory: job.Memory, Storage: job.Storage})
	}
	for _, group := range job.Groups {
		if err := check(fmt.Sprintf("job.groups[%q]", group.Name), group); err != nil {
			return err
		}
	}
	return nil
}

// asksForCoresOnly reports whether a job names no type and no accelerator anywhere and does state
// cores: the plain way to ask for CPU, which submission reads as that many units of domain.CPUType.
func asksForCoresOnly(job domain.JobSpec) bool {
	if job.AcceleratorType != "" || domain.GroupAcceleratorType(job) != "" || job.TotalAccelerators() != 0 {
		return false
	}
	for _, g := range job.NodeGroups() {
		if g.CPU != "" {
			return true
		}
	}
	return false
}

// countSetsCores is the one rule of the type whose unit is a core (domain.CPUType): on each node
// the count and the cores are one whole number. The job states its cores and this writes the
// count, and refuses a count that differs or a fraction of a core. Everything that reads the job
// afterwards, the stored row, the footprint, the resolved spec and every runtime, sees a job
// whose cpu is its count and needs no rule of its own.
func countSetsCores(job *domain.JobSpec) error {
	shape := func(field string, count *int, cpu string) error {
		switch cores, err := jobspec.CPUCores(cpu); {
		case err != nil:
			return fmt.Errorf("%s.cpu: %w", field, err)
		case cores < 1 || cores != float64(int(cores)):
			return fmt.Errorf("%s.cpu is %q: a job on CPU holds whole cores, so ask for a whole number of at least 1", field, cpu)
		case *count == 0:
			*count = int(cores)
		case cores != float64(*count):
			return fmt.Errorf("%s.cpu is %q and %s.accelerator_count is %d: for a job on CPU the count is the node's cores, so leave the count out or make them equal", field, cpu, field, *count)
		}
		return nil
	}
	if len(job.Groups) == 0 {
		return shape("job", &job.AcceleratorCount, job.CPU)
	}
	for i := range job.Groups {
		group := &job.Groups[i]
		if err := shape(fmt.Sprintf("job.groups[%q]", group.Name), &group.AcceleratorCount, group.CPU); err != nil {
			return err
		}
	}
	return nil
}

// typeField is the field of the accelerator type at position i of a job's choices, the first its
// accelerator_type and the rest its acceptable_accelerator_types.
func typeField(i int) string {
	if i == 0 {
		return "job.accelerator_type"
	}
	return "job.acceptable_accelerator_types"
}

// ValidateExperiment checks required fields are populated and no resource dimension exceeds
// its per-job cap — applied before any quota debit, so one absurd submission can't consume an
// entire budget in one shot.
func ValidateExperiment(exp *domain.Experiment, caps domain.QuotaConfig) error {
	if exp == nil {
		return &AdmissionError{Reason: ReasonMalformed, Message: "experiment is nil"}
	}
	if err := domain.ValidateIdentifier("id", exp.ID); err != nil {
		return &AdmissionError{Reason: ReasonMalformed, Field: "id", Message: err.Error()}
	}
	if exp.AgentID == "" {
		return &AdmissionError{Reason: ReasonMalformed, Field: "agent_id", Message: "agent_id is required"}
	}
	if exp.HypothesisID == "" {
		return &AdmissionError{Reason: ReasonMalformed, Field: "hypothesis_id", Message: "hypothesis_id is required: every job but a baseline agent's tests a hypothesis registered with POST /hypotheses"}
	}
	// A job on the objective is the baseline's: it measures and predicts nothing.
	if exp.Theory == "" && !exp.OnObjective() {
		return &AdmissionError{Reason: ReasonMalformed, Field: "theory", Message: "theory is required"}
	}
	if exp.CodeRef == "" {
		return &AdmissionError{Reason: ReasonMalformed, Field: "code_ref", Message: "code_ref is required"}
	}
	if !codeRefPattern.MatchString(exp.CodeRef) {
		return &AdmissionError{Reason: ReasonMalformed, Field: "code_ref", Message: "code_ref must be \"<git-remote-url>@<commit-sha>\" — a full 40-character commit SHA, not a branch name or tag"}
	}
	// Node labels under the platform's prefix are written by admission, the slot a job is
	// admitted to among them. A submitter who could name one would place the job themselves.
	for label := range exp.Job.NodeSelector {
		if strings.HasPrefix(label, domain.NodeLabelReservedPrefix) {
			return &AdmissionError{Reason: ReasonMalformed, Field: "job.node_selector", Message: fmt.Sprintf("job.node_selector may not name %q: labels under %s are set by the platform", label, domain.NodeLabelReservedPrefix)}
		}
	}
	// A host path names the node's own filesystem, its container runtime socket and the
	// runtime's token among it, so a job that chose one would hold the node and every job on it.
	if len(exp.Job.HostMounts) > 0 {
		return &AdmissionError{Reason: ReasonMalformed, Field: "job.host_mounts", Message: "job.host_mounts is not accepted: a job reads its data from its data URI"}
	}
	if err := domain.ValidateCommand(exp.Job); err != nil {
		return &AdmissionError{Reason: ReasonRequired, Field: "job.command", Message: err.Error()}
	}
	if err := domain.ValidateEnv(exp.Job); err != nil {
		return &AdmissionError{Reason: ReasonMalformed, Field: "job.env", Message: err.Error()}
	}
	if err := quantitiesStated(exp.Job); err != nil {
		return err
	}
	if asksForCoresOnly(exp.Job) {
		exp.Job.AcceleratorType = domain.CPUType
	}
	if requested := exp.Job.AcceleratorType; requested.Kind() == domain.TypeKindCPU || (requested == "" && domain.GroupAcceleratorType(exp.Job).Kind() == domain.TypeKindCPU) {
		if err := countSetsCores(&exp.Job); err != nil {
			return &AdmissionError{Reason: ReasonMalformed, Field: "job", Message: err.Error()}
		}
		exp.AcceleratorCount = exp.Job.TotalAccelerators()
	}
	// A grouped job states its shapes per group and is rejected outright if it also states them
	// at the top level, so the two blocks below are alternatives, never a merge.
	if err := domain.ValidateGroups(exp.Job); err != nil {
		return &AdmissionError{Reason: ReasonMalformed, Field: "job.groups", Message: err.Error()}
	}
	// A grouped job may name its single accelerator type on the groups rather than at the top
	// level. Normalize it onto the job here, at the one place a submission is validated, so
	// everything downstream — footprint, admission, substitution, billing, both backends — keeps
	// reading exactly one field for the type instead of learning about groups.
	if exp.Job.AcceleratorType == "" {
		exp.Job.AcceleratorType = domain.GroupAcceleratorType(exp.Job)
	}
	if err := domain.ValidateSecretEnv(exp.Job, exp.SecretEnv); err != nil {
		return &AdmissionError{Reason: ReasonMalformed, Field: "secret_env", Message: err.Error()}
	}
	// The two fields a job may leave out for a stated default, each written here and nowhere else
	// and answered on the job: job.max_retries is 0, and capacity_tier is guaranteed.
	if exp.Job.MaxRetries == nil {
		exp.Job.MaxRetries = new(int)
	}
	if *exp.Job.MaxRetries < 0 {
		return &AdmissionError{Reason: ReasonMalformed, Field: "job.max_retries", Message: "job.max_retries must not be negative"}
	}
	switch exp.CapacityTier {
	case "":
		exp.CapacityTier = domain.CapacityGuaranteed
	case domain.CapacityGuaranteed, domain.CapacityBurst:
	default:
		return &AdmissionError{Reason: ReasonMalformed, Field: "capacity_tier", Message: fmt.Sprintf("capacity_tier must be %q, %q, or omitted, got %q", domain.CapacityGuaranteed, domain.CapacityBurst, exp.CapacityTier)}
	}

	// Every job holds units of a priced type. Budget, quota, fairness and the concurrency cap all
	// count those units, so a job that held none would run outside all of them. An accelerator job
	// must name a type, or nothing downstream has a type to schedule against.
	switch {
	case exp.AcceleratorCount < 0:
		return &AdmissionError{Reason: ReasonMalformed, Field: "job.accelerator_count", Message: "job.accelerator_count must not be negative"}
	case exp.AcceleratorCount == 0:
		return &AdmissionError{Reason: ReasonMalformed, Field: "job", Message: "the job asks for nothing the catalog prices: state job.cpu, a whole number of cores, for a job on CPU, or job.accelerator_type with job.accelerator_count of at least 1 for a job on accelerators"}
	case exp.Job.AcceleratorType == "":
		return &AdmissionError{Reason: ReasonMalformed, Field: "job.accelerator_type", Message: "job.accelerator_type is required when job.accelerator_count > 0"}
	}
	if exp.AcceleratorCount > 0 {
		// Every type must be a well-formed driver-published key=value AND priced in the
		// operator's catalog. The catalog attaches a rate to these strings; it never renames
		// them, so an unpriced type is an operator gap, not a translation failure.
		seen := map[domain.AcceleratorType]bool{}
		for i, t := range append([]domain.AcceleratorType{exp.Job.AcceleratorType}, exp.Job.AcceptableAcceleratorTypes...) {
			if err := t.Validate(); err != nil {
				return &AdmissionError{Reason: ReasonMalformed, Field: typeField(i), Message: err.Error()}
			}
			// Position 0 is job.accelerator_type, the first choice. It may legitimately reappear
			// among the alternatives — an agent listing every accelerator type it runs on naturally includes
			// its preferred one, and rejecting that makes the obvious payload unsubmittable. Only a
			// genuine repeat *within* acceptable_accelerator_types is malformed, so dedup from i=1.
			// placement.CandidateTypes collapses the harmless overlap before placement.
			if i > 0 {
				if seen[t] {
					return &AdmissionError{Reason: ReasonMalformed, Field: "job.acceptable_accelerator_types", Message: fmt.Sprintf("accelerator type %q is listed more than once", t)}
				}
				seen[t] = true
			}
			if _, ok := domain.LookupAcceleratorCost(t); !ok {
				return &AdmissionError{Reason: ReasonMalformed, Field: typeField(i), Message: fmt.Sprintf("accelerator type %q has no configured acch_rate (position %d)", t, i)}
			}
			// A count is cores on CPU and devices on an accelerator, so one count cannot stand
			// for both.
			if requested := exp.Job.AcceleratorType; t.Kind() != requested.Kind() {
				return &AdmissionError{Reason: ReasonMalformed, Field: "job.acceptable_accelerator_types", Message: fmt.Sprintf("acceptable accelerator type %q cannot stand in for job.accelerator_type %q: a job runs on CPU cores or on accelerators, never either", t, requested)}
			}
		}
		exp.AcceleratorType = exp.Job.AcceleratorType
	}
	// One that is left out is the submit's to give (Experiment.TakeEstimate, takeLargestEstimate).
	if exp.EstimatedDurationHours < 0 {
		return &AdmissionError{Reason: ReasonMalformed, Field: "estimated_duration_hours", Message: "estimated_duration_hours must not be negative"}
	}

	// The count of a job on cores is its cores, which max_cpu_cores_per_job bounds below.
	if exp.AcceleratorType.Kind() != domain.TypeKindCPU && exp.AcceleratorCount > caps.MaxAcceleratorCountPerJob {
		return &AdmissionError{Reason: ReasonMalformed, Field: "job.accelerator_count", Message: fmt.Sprintf("job.accelerator_count %d is over the most accelerators one job may ask for, %d", exp.AcceleratorCount, caps.MaxAcceleratorCountPerJob)}
	}
	nodes := exp.Job.Nodes()
	cpu, err := totalOverGroups(exp.Job, jobspec.CPUCores, func(g domain.JobGroup) string { return g.CPU })
	if err != nil {
		return &AdmissionError{Reason: ReasonMalformed, Field: "job.cpu", Message: "job.cpu: " + err.Error()}
	}
	if cpu > caps.MaxCPUCoresPerJob {
		return &AdmissionError{Reason: ReasonMalformed, Field: "job.cpu", Message: fmt.Sprintf("job.cpu %g cores (x%d nodes) is over the most cores one job may ask for, %g", cpu/float64(nodes), nodes, caps.MaxCPUCoresPerJob)}
	}
	memory, err := totalOverGroups(exp.Job, jobspec.GB, func(g domain.JobGroup) string { return g.Memory })
	if err != nil {
		return &AdmissionError{Reason: ReasonMalformed, Field: "job.memory", Message: "job.memory: " + err.Error()}
	}
	if memory > caps.MaxRAMGBPerJob {
		return &AdmissionError{Reason: ReasonMalformed, Field: "job.memory", Message: fmt.Sprintf("job.memory %.2fGB (x%d nodes) exceeds per-job max %.2fGB", memory/float64(nodes), nodes, caps.MaxRAMGBPerJob)}
	}
	storage, err := totalOverGroups(exp.Job, jobspec.GB, func(g domain.JobGroup) string { return g.Storage })
	if err != nil {
		return &AdmissionError{Reason: ReasonMalformed, Field: "job.storage", Message: "job.storage: " + err.Error()}
	}
	if storage > caps.MaxStorageGBPerJob {
		return &AdmissionError{Reason: ReasonMalformed, Field: "job.storage", Message: fmt.Sprintf("job.storage %.2fGB (x%d nodes) exceeds per-job max %.2fGB", storage/float64(nodes), nodes, caps.MaxStorageGBPerJob)}
	}
	// ExtraResources has no billing dimension or per-job cap, but a malformed quantity string
	// should still be rejected here rather than surfacing later as an opaque Job-creation failure.
	for name, qty := range exp.Job.ExtraResources {
		if _, err := jobspec.ParseQuantity(qty); err != nil {
			return &AdmissionError{Reason: ReasonMalformed, Field: "job.extra_resources", Message: fmt.Sprintf("job.extra_resources[%q]: %s", name, err.Error())}
		}
	}
	if exp.Job.ShmSize != "" {
		q, err := jobspec.ParseQuantity(exp.Job.ShmSize)
		if err != nil {
			return &AdmissionError{Reason: ReasonMalformed, Field: "job.shm_size", Message: fmt.Sprintf("job.shm_size %q: %s", exp.Job.ShmSize, err.Error())}
		}
		if q.Sign() <= 0 {
			return &AdmissionError{Reason: ReasonMalformed, Field: "job.shm_size", Message: fmt.Sprintf("job.shm_size must be positive (%q)", exp.Job.ShmSize)}
		}
	}
	for name, qty := range exp.Job.AcceleratorPodResources {
		if _, err := jobspec.ParseQuantity(qty); err != nil {
			return &AdmissionError{Reason: ReasonMalformed, Field: "job.accelerator_pod_resources", Message: fmt.Sprintf("job.accelerator_pod_resources[%q]: %s", name, err.Error())}
		}
	}
	return nil
}
