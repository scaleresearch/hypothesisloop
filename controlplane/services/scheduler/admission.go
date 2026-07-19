package scheduler

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/workload"
)

// Admission error reason constants.
const (
	ReasonInsufficientCredits = "insufficient_credits"
	ReasonDuplicate           = "duplicate"
	ReasonMalformed           = "malformed"
	ReasonSummaryRequired     = "summary_required"
	ReasonRateLimited         = "rate_limited"
)

// AdmissionError is returned when an experiment cannot be admitted.
type AdmissionError struct {
	Reason  string
	Message string
}

func (e *AdmissionError) Error() string {
	return fmt.Sprintf("admission rejected [%s]: %s", e.Reason, e.Message)
}

// ValidateExperiment checks that required fields are populated and, given caps, that no
// single resource dimension exceeds its per-job maximum — applied before any quota debit, so
// one absurd submission can never consume an entire guaranteed/burst budget in one shot
// regardless of how loosely the budget itself is sized.
func ValidateExperiment(exp *domain.Experiment, caps domain.QuotaConfig) error {
	if exp == nil {
		return &AdmissionError{Reason: ReasonMalformed, Message: "experiment is nil"}
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
	if exp.Job.Image == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "job.image is required"}
	}
	// Resource requests must be explicit at submission time (plan cross-cutting fix #1): the
	// footprint used for cluster selection/admission must be genuinely known when the control
	// plane picks a cluster and computes a budget estimate, not resolved later from a
	// per-cluster JobDefaults ConfigMap the control plane never sees — different clusters could
	// silently disagree on defaults, and admission would be checking a footprint that isn't the
	// one actually created. Matches important.md's "no fallbacks — one path or error": reject,
	// don't resolve a cluster-side default pre-admission.
	if exp.Job.CPU == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "job.cpu is required (explicit resource requests must be set at submission, not left to a cluster-side default)"}
	}
	if exp.Job.Memory == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "job.memory is required (explicit resource requests must be set at submission, not left to a cluster-side default)"}
	}
	if exp.Job.Storage == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "job.storage is required (explicit resource requests must be set at submission, not left to a cluster-side default)"}
	}

	// AcceleratorCount == 0 is a legitimate CPU-only job (see cpuFlavor / admissionUnit) as long as it
	// actually requests positive CPU — a job with zero accelerators and no CPU requests nothing at all,
	// which is genuinely malformed. An accelerator job (AcceleratorCount > 0) must name an accelerator type: nothing
	// downstream (flavor accounting, accelerator affinity/tolerations) has a type to schedule against
	// otherwise.
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
	case exp.AcceleratorType == "":
		return &AdmissionError{Reason: ReasonMalformed, Message: "job.accelerator_type is required when job.accelerator_count > 0"}
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
	// ExtraResources (TPUs/other accelerators) has no billing dimension or per-job cap (see
	// domain.JobSpec.ExtraResources), but a malformed quantity string should still be
	// rejected here — at submission — rather than surfacing later as an opaque Job-creation
	// failure once the scheduler loop calls workload.CreateWorkload.
	for name, qty := range exp.Job.ExtraResources {
		if _, err := resource.ParseQuantity(qty); err != nil {
			return &AdmissionError{Reason: ReasonMalformed, Message: fmt.Sprintf("job.extra_resources[%q]: %s", name, err.Error())}
		}
	}
	return nil
}
