package scheduler

import (
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

// codeRefPattern requires an immutable pointer — a full commit SHA, never a branch name or
// "latest". Matches "<any-git-remote-url>@<40-hex-char-sha>"; only the "@<sha>" suffix is enforced.
var codeRefPattern = regexp.MustCompile(`^\S+@[0-9a-f]{40}$`)

// experimentIDPattern mirrors Kubernetes' RFC 1123 DNS subdomain rule. The experiment ID is
// used verbatim by cluster-agent to name the Job/Pod/ResourceClaimTemplate it creates
// (e.g. "exp-<id>-accelerator"), so any ID k8s would reject must be rejected here — at
// submission time, with a message the submitter can act on — rather than accepted and left to
// fail silently in an endless cluster-agent reconcile loop against a name it can never create.
var experimentIDPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Admission error reason constants.
const (
	ReasonInsufficientCredits = "insufficient_credits"
	ReasonDuplicate           = "duplicate"
	ReasonMalformed           = "malformed"
	ReasonSummaryRequired     = "summary_required"
	ReasonRateLimited         = "rate_limited"
	ReasonJobTooLong          = "job_too_long"
	ReasonDataQuotaExceeded   = "data_quota_exceeded"
)

// AdmissionError is returned when an experiment cannot be admitted.
type AdmissionError struct {
	Reason  string
	Message string
}

func (e *AdmissionError) Error() string {
	return fmt.Sprintf("admission rejected [%s]: %s", e.Reason, e.Message)
}

// ValidateExperiment checks required fields are populated and no resource dimension exceeds
// its per-job cap — applied before any quota debit, so one absurd submission can't consume an
// entire budget in one shot.
func ValidateExperiment(exp *domain.Experiment, caps domain.QuotaConfig) error {
	if exp == nil {
		return &AdmissionError{Reason: ReasonMalformed, Message: "experiment is nil"}
	}
	if exp.ID == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "id is required"}
	}
	if !experimentIDPattern.MatchString(exp.ID) {
		return &AdmissionError{Reason: ReasonMalformed, Message: "id must be a valid Kubernetes RFC 1123 DNS subdomain: lowercase alphanumeric characters and '-' only, starting and ending with an alphanumeric character (the ID is used verbatim in cluster resource names)"}
	}
	if exp.AgentID == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "agent_id is required"}
	}
	if exp.ProjectID == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "project_id is required"}
	}
	if exp.HypothesisID == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "hypothesis_id is required"}
	}
	if exp.Theory == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "theory is required"}
	}
	if exp.Objective == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "objective is required"}
	}
	if exp.CodeRef == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "code_ref is required"}
	}
	if !codeRefPattern.MatchString(exp.CodeRef) {
		return &AdmissionError{Reason: ReasonMalformed, Message: "code_ref must be \"<git-remote-url>@<commit-sha>\" — a full 40-character commit SHA, not a branch name or tag"}
	}
	if exp.Job.Image == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "job.image is required"}
	}
	// Resource requests must be explicit at submission time: the footprint used for cluster
	// selection/admission must be known up front, not resolved later from a per-cluster
	// JobDefaults ConfigMap the control plane never sees. Reject rather than assume a default.
	if exp.Job.CPU == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "job.cpu is required (explicit resource requests must be set at submission, not left to a cluster-side default)"}
	}
	if exp.Job.Memory == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "job.memory is required (explicit resource requests must be set at submission, not left to a cluster-side default)"}
	}
	if exp.Job.Storage == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "job.storage is required (explicit resource requests must be set at submission, not left to a cluster-side default)"}
	}
	if exp.Job.MaxRetries == nil || *exp.Job.MaxRetries < 0 {
		return &AdmissionError{Reason: ReasonMalformed, Message: "job.max_retries is required and must be non-negative"}
	}

	// AcceleratorCount == 0 is a legitimate CPU-only job as long as it requests positive CPU —
	// zero accelerators and no CPU requests nothing at all. An accelerator job must name a type,
	// or nothing downstream has a type to schedule against.
	switch {
	case exp.AcceleratorCount < 0:
		return &AdmissionError{Reason: ReasonMalformed, Message: "job.accelerator_count must not be negative"}
	case exp.AcceleratorCount == 0:
		cores, err := workload.ParseCPUCores(exp.Job.CPU)
		if err != nil {
			return &AdmissionError{Reason: ReasonMalformed, Message: "job.cpu: " + err.Error()}
		}
		if cores <= 0 {
			return &AdmissionError{Reason: ReasonMalformed, Message: "job.accelerator_count is zero and job.cpu requests no positive CPU"}
		}
	case exp.Job.AcceleratorType == "":
		return &AdmissionError{Reason: ReasonMalformed, Message: "job.accelerator_type is required when job.accelerator_count > 0"}
	}
	if exp.AcceleratorCount > 0 {
		// Every type must be a well-formed driver-published key=value AND priced in the
		// operator's catalog. The catalog attaches a rate to these strings; it never renames
		// them, so an unpriced type is an operator gap, not a translation failure.
		seen := map[domain.AcceleratorType]bool{}
		for i, t := range append([]domain.AcceleratorType{exp.Job.AcceleratorType}, exp.Job.AcceptableAcceleratorTypes...) {
			if err := t.Validate(); err != nil {
				return &AdmissionError{Reason: ReasonMalformed, Message: err.Error()}
			}
			// Position 0 is job.accelerator_type, the first choice. It may legitimately reappear
			// among the alternatives — an agent listing every flavor it runs on naturally includes
			// its preferred one, and rejecting that makes the obvious payload unsubmittable. Only a
			// genuine repeat *within* acceptable_accelerator_types is malformed, so dedup from i=1.
			// candidateAcceleratorTypes collapses the harmless overlap before placement.
			if i > 0 {
				if seen[t] {
					return &AdmissionError{Reason: ReasonMalformed, Message: fmt.Sprintf("accelerator type %q is listed more than once", t)}
				}
				seen[t] = true
			}
			if _, ok := t.LookupCost(); !ok {
				return &AdmissionError{Reason: ReasonMalformed, Message: fmt.Sprintf("accelerator type %q has no configured acch_rate (position %d)", t, i)}
			}
		}
		exp.AcceleratorType = exp.Job.AcceleratorType
	}
	if exp.EstimatedDurationHours <= 0 {
		return &AdmissionError{Reason: ReasonMalformed, Message: "estimated_duration_hours must be positive"}
	}

	if caps.MaxAcceleratorCountPerJob > 0 && exp.AcceleratorCount > caps.MaxAcceleratorCountPerJob {
		return &AdmissionError{Reason: ReasonMalformed, Message: fmt.Sprintf("job.accelerator_count %d exceeds per-job max %d", exp.AcceleratorCount, caps.MaxAcceleratorCountPerJob)}
	}
	nodes := float64(exp.Job.Nodes())
	if caps.MaxCPUCoresPerJob > 0 {
		cores, err := workload.ParseCPUCores(exp.Job.CPU)
		if err != nil {
			return &AdmissionError{Reason: ReasonMalformed, Message: "job.cpu: " + err.Error()}
		}
		if total := cores * nodes; total > caps.MaxCPUCoresPerJob {
			return &AdmissionError{Reason: ReasonMalformed, Message: fmt.Sprintf("job.cpu %.2f cores (x%d nodes) exceeds per-job max %.2f", cores, exp.Job.Nodes(), caps.MaxCPUCoresPerJob)}
		}
	}
	if caps.MaxRAMGBPerJob > 0 {
		gb, err := workload.ParseMemoryGB(exp.Job.Memory)
		if err != nil {
			return &AdmissionError{Reason: ReasonMalformed, Message: "job.memory: " + err.Error()}
		}
		if total := gb * nodes; total > caps.MaxRAMGBPerJob {
			return &AdmissionError{Reason: ReasonMalformed, Message: fmt.Sprintf("job.memory %.2fGB (x%d nodes) exceeds per-job max %.2fGB", gb, exp.Job.Nodes(), caps.MaxRAMGBPerJob)}
		}
	}
	if caps.MaxStorageGBPerJob > 0 {
		gb, err := workload.ParseStorageGB(exp.Job.Storage)
		if err != nil {
			return &AdmissionError{Reason: ReasonMalformed, Message: "job.storage: " + err.Error()}
		}
		if total := gb * nodes; total > caps.MaxStorageGBPerJob {
			return &AdmissionError{Reason: ReasonMalformed, Message: fmt.Sprintf("job.storage %.2fGB (x%d nodes) exceeds per-job max %.2fGB", gb, exp.Job.Nodes(), caps.MaxStorageGBPerJob)}
		}
	}
	// ExtraResources has no billing dimension or per-job cap, but a malformed quantity string
	// should still be rejected here rather than surfacing later as an opaque Job-creation failure.
	for name, qty := range exp.Job.ExtraResources {
		if _, err := resource.ParseQuantity(qty); err != nil {
			return &AdmissionError{Reason: ReasonMalformed, Message: fmt.Sprintf("job.extra_resources[%q]: %s", name, err.Error())}
		}
	}
	for name, qty := range exp.Job.AcceleratorPodResources {
		if _, err := resource.ParseQuantity(qty); err != nil {
			return &AdmissionError{Reason: ReasonMalformed, Message: fmt.Sprintf("job.accelerator_pod_resources[%q]: %s", name, err.Error())}
		}
	}
	return nil
}
