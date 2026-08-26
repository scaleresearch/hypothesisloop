package domain

import "strings"

// MinRemainingHours is the minimum rescheduled duration after preemption (15 minutes).
const MinRemainingHours = 0.25

// ExperimentStatus represents the lifecycle state of an agent job.
type ExperimentStatus string

const (
	StatusSubmitted ExperimentStatus = "SUBMITTED"
	StatusQueued    ExperimentStatus = "QUEUED"
	StatusAdmitted  ExperimentStatus = "ADMITTED"
	StatusRunning   ExperimentStatus = "RUNNING"
	StatusCompleted ExperimentStatus = "COMPLETED"
	StatusFailed    ExperimentStatus = "FAILED"
	StatusEvicted   ExperimentStatus = "EVICTED"
	StatusRejected  ExperimentStatus = "REJECTED"
)

// ValidExperimentStatus reports whether s is one of the recognized statuses. Callers filtering on
// a status validate here first: the column is a Postgres enum, so an unrecognized value otherwise
// surfaces as a driver error and a 500 rather than the client mistake it is.
func ValidExperimentStatus(s ExperimentStatus) bool {
	switch s {
	case StatusSubmitted, StatusQueued, StatusAdmitted, StatusRunning,
		StatusCompleted, StatusFailed, StatusEvicted, StatusRejected:
		return true
	default:
		return false
	}
}

const (
	NotAdmittedCapacityUnavailable = "capacity_unavailable"
	NotAdmittedOutranked           = "outranked"
	NotAdmittedSummaryGate         = "summary_gate"
	NotAdmittedStageCut            = "stage_cut"
	NotAdmittedJobTooLong          = "job_too_long"
	NotAdmittedWorkloadCreation    = "workload_creation_failed"
	// NotAdmittedNoMultiNodeCluster means the job spans more than one node and no connected
	// cluster reports being able to run multi-node work. Distinct from capacity_unavailable
	// because waiting will not fix it: this is not a busy platform, it is a platform with no
	// runtime for this shape of job.
	NotAdmittedNoMultiNodeCluster = "no_multi_node_cluster"
	// NotAdmittedWaitingForScaleUp means this cluster's desired-free already went negative for a
	// speculative submit in this job's accelerator dimension — a scale-up is outstanding there.
	// Preemption is skipped rather than evicting a burst job to cover a shortage the incoming
	// node is already going to fill (autoscaler.md's skip-preemption rule).
	NotAdmittedWaitingForScaleUp = "waiting-for-scale-up"
	// NotAdmittedNoScalableCapacity means every autoscaler-enabled candidate has already been
	// tried (and failed over) within the tried-cluster TTL, and live preemption/disbalance found
	// nothing either — the job is not stranded, it will be reconsidered once an entry expires or
	// live capacity changes.
	NotAdmittedNoScalableCapacity = "no_scalable_capacity"
)

// IsTerminal reports whether the status is a final lifecycle state that no further execution
// or progress reporting can follow.
func (s ExperimentStatus) IsTerminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusEvicted, StatusRejected:
		return true
	default:
		return false
	}
}

// Termination is what a requested terminal transition actually resolved to. A caller asks for
// EVICTED/REJECTED with a reason; the store decides, because an infrastructure fault under its
// ceiling resolves to a free requeue instead — and that decision belongs in the one guarded
// UPDATE that also spends the requeue budget, not in each of the call sites that can raise one.
type Termination int

const (
	// TerminationSkipped: the row had already left the `from` status, so nothing was written.
	// The caller must do nothing further — whoever moved it owns the outcome.
	TerminationSkipped Termination = iota
	// TerminationWritten: the terminal status and reason are persisted. Settle, then mark settled.
	TerminationWritten
	// TerminationRequeued: an infrastructure fault with requeue budget left, so the row is back
	// in QUEUED rather than terminal. Settle (which refunds it to zero) but never mark it
	// settled — the row is not terminal and the next attempt owes a settlement of its own.
	TerminationRequeued
)

// EvictionReason classifies why a job was terminated early.
type EvictionReason string

// WithDetail appends a human-readable explanation to a reason, in the same "code: detail" shape
// the scheduler already writes into NotAdmittedReason. The code stays the first token, so anything
// matching on the reason keeps working by prefix; the detail exists because a terminated job is
// the one outcome an agent cannot investigate afterwards — its workload is gone — so whatever the
// platform knew at the moment it decided has to be recorded there and then.
func (r EvictionReason) WithDetail(detail string) EvictionReason {
	return EvictionReason(string(r) + ": " + detail)
}

// Code strips any WithDetail suffix, for callers comparing against the constants above.
func (r EvictionReason) Code() EvictionReason {
	if code, _, found := strings.Cut(string(r), ":"); found {
		return EvictionReason(code)
	}
	return r
}

// The fungible per-node dimensions a cluster reports free capacity for, alongside its per-node
// accelerator counts. Named here so the runtimes reporting them and admission reading them cannot
// drift apart — a mistyped key is not an error, it is a silently absent dimension.
const (
	NodeResourceCPUMillicores = "cpu_millicores"
	NodeResourceMemoryBytes   = "memory_bytes"
	NodeResourceStorageBytes  = "storage_bytes"
)

const (
	EvictionSilent EvictionReason = "silent"
	// EvictionNeverReportedMetrics is a job that never emitted a metric its platform experiment
	// declared, as opposed to one that reported and then went quiet. The distinction matters
	// because the causes are different: a hung training process versus a reporting path that
	// never worked at all (wrong URL, a stale metrics helper baked into the image, an exception
	// the workload swallows). Reported as plain silence, both look like "the job hung".
	//
	// Fires whether or not the pod is still alive: a live pod emitting nothing cannot be ranked,
	// cut or compared against anything, so it holds an accelerator and bills for it while
	// producing no evidence at all. See controller.checkSilence for the grace period.
	EvictionNeverReportedMetrics EvictionReason = "never_reported_metrics"
	EvictionQuotaExhaustion      EvictionReason = "quota_exhaustion"
	EvictionExperimentClosed     EvictionReason = "experiment_closed"
	EvictionCancelled            EvictionReason = "cancelled"
	// EvictionStageCut terminates an agent's jobs when it is cut at a stage boundary.
	// Terminal for the rest of the platform experiment — see the stage ladder.
	EvictionStageCut EvictionReason = "stage_cut"
	// EvictionJobTooLong terminates a job that has run longer than the current stage's
	// max_job_hours.
	EvictionJobTooLong EvictionReason = "job_too_long"
	// EvictionStuckPending marks a job that was admitted (SUBMITTED/ADMITTED) but never
	// reported RUNNING within StuckPendingTimeoutSeconds — e.g. unschedulable due to
	// fragmentation or a bad image. See job_watcher.go.
	EvictionStuckPending EvictionReason = "stuck_pending"
	// EvictionResourceDisbalance terminates a RUNNING job whose requested CPU/memory/storage is
	// grossly out of proportion to the accelerators it holds, while that request is provably the
	// only thing keeping a queued job off idle accelerators on the same physical node. Terminal
	// rather than requeued: the job's own spec is what makes it unplaceable alongside its
	// neighbours, so returning it to the queue unchanged would evict it again on the next tick.
	// See services/scheduler/loop_disbalance.go.
	EvictionResourceDisbalance EvictionReason = "resource_disbalance"
	// EvictionUnschedulable marks a job whose runtime reported a phase-detail reason in
	// NeverSelfHealsPhaseReasons (a bad image, a malformed container config) before the full
	// StuckPendingTimeout elapsed — evicted early with a full refund rather than left to occupy
	// a reservation until the generic timeout catches it.
	EvictionUnschedulable EvictionReason = "unschedulable"
	// EvictionWorkloadGone marks a RUNNING job whose cluster reported a complete, fresh status
	// snapshot that no longer mentions it at all (metricsdb.LatestJobPhase's JobPhaseGone) —
	// the runtime's own confirmation that no live pod/container exists for it, sustained across
	// a full silence window. Distinct from EvictionSilent: that's a live pod that stopped
	// reporting; this is no pod at all (e.g. an orphaned pod lost across a host reboot). Without
	// this, such a job stays RUNNING forever and its quota is never released — see checks.go.
	EvictionWorkloadGone EvictionReason = "workload_gone"
	// EvictionClusterUnreachable marks a job whose cluster has pushed no status snapshot at all
	// for longer than ClusterSilenceCeilingSeconds. Distinct from EvictionWorkloadGone, which
	// requires a fresh snapshot that omits the job: here there is no fresh snapshot to read, so
	// nothing can be concluded about the job itself — only about its cluster.
	//
	// Without a ceiling this state is unbounded, and a cluster that dies or partitions strands
	// every reservation on it forever. Eviction is expressed purely as desired state, so if the
	// cluster does come back its agent reconciles the job away on its next pull; the accelerator
	// is genuinely occupied until then, which is the unavoidable cost of an unreachable cluster.
	EvictionClusterUnreachable EvictionReason = "cluster_unreachable"
	// EvictionFlavorMismatch marks a job whose cluster reported it running on an accelerator type
	// other than the one reserved and billed for it. Unlike missing telemetry this cannot resolve
	// itself by waiting — the placement already happened and is wrong — so it is terminal rather
	// than retried. Only raised for an observation newer than the experiment's current
	// submitted_at, so a stale sample from a previous admission can never trigger it.
	EvictionFlavorMismatch EvictionReason = "flavor_mismatch"
	// EvictionAcceleratorTypeUnobservable marks a job whose pod the runtime reports as Running but
	// whose accelerator type never became readable, so the lifecycle could not be advanced to
	// RUNNING. Deliberately not EvictionStuckPending: the pod did start and is burning real
	// hardware, and calling that "stuck pending" would both misname the fault and imply a
	// never-ran refund. See scheduler.JobWatcher.reconcileOne.
	EvictionAcceleratorTypeUnobservable EvictionReason = "accelerator_type_unobservable"
	// EvictionPreemptedForGuaranteed marks a burst job returned to QUEUED to free capacity for a
	// guaranteed one. Not terminal — the job runs again — so it doubles as the durable record
	// that this job has already executed once, which is what forbids re-admitting it onto a
	// different accelerator flavor (see candidateAcceleratorTypes).
	EvictionPreemptedForGuaranteed EvictionReason = "preempted_for_guaranteed"
	// EvictionScaleUpTimeout marks a job whose gang never fully bound (scheduled_nodes < Nodes())
	// within its placement deadline on an autoscaler-enabled cluster — either the deadline
	// passed, or the runtime reported a terminal scheduling_reason (e.g. "NotTriggerScaleUp: max
	// node group size reached") before that. An infrastructure fault, requeued for free, but
	// recorded as a tried_clusters failover rather than spending infra_requeue_count — see
	// ResolveTermination's triedClusterID and autoscaler.md.
	EvictionScaleUpTimeout EvictionReason = "scale_up_timeout"
)

// PhaseDetail is what a runtime observed about why a job's container hasn't started (or
// restarted), reported alongside phase in a cluster-agent's status push. Reason is one of a
// small vocabulary the control plane defines (see the PhaseReason* constants below); each
// runtime translates its own native taxonomy (Kubernetes container-status reasons, a container
// engine's own error text) into that vocabulary, so the control plane never learns a runtime's
// vocabulary (important.md #6). Message is free text for a human/agent to read; Reason is what
// code acts on.
type PhaseDetail struct {
	Reason       string `json:"reason,omitempty"`
	Message      string `json:"message,omitempty"`
	RestartCount int32  `json:"restart_count,omitempty"`
	// ScheduledNodes is the count of this experiment's pods with condition PodScheduled=True, as
	// last reported by the runtime — see autoscaler.md's gang-readiness design. 0 on a runtime
	// that doesn't yet report it, or genuinely zero pods placed.
	ScheduledNodes int32 `json:"scheduled_nodes,omitempty"`
	// SchedulingReason is a best-effort, runtime-supplied explanation for a still-Pending pod
	// (an autoscaler event message like "TriggeredScaleUp" or "NotTriggerScaleUp: max node group
	// size reached").
	SchedulingReason string `json:"scheduling_reason,omitempty"`
}

const (
	// PhaseReasonImagePullFailed: the runtime could not pull the job's image (unknown
	// tag/repo, no registry credentials, image genuinely absent). Never self-heals without a
	// new image being pushed or the job resubmitted with a correct reference.
	PhaseReasonImagePullFailed = "image_pull_failed"
	// PhaseReasonConfigError: the runtime rejected the job's own container configuration
	// (a malformed command, an invalid resource request, a mount that cannot be satisfied).
	// Never self-heals without the job spec changing.
	PhaseReasonConfigError = "config_error"
	// PhaseReasonInvalidImage: the image reference itself is malformed, not merely unpullable.
	PhaseReasonInvalidImage = "invalid_image"
	// PhaseReasonContainerFailed: the workload's own process exited non-zero. Deliberately NOT
	// in NeverSelfHealsPhaseReasons — a job with retries left should use them; this reports what
	// happened, it does not decide the job's fate.
	PhaseReasonContainerFailed = "container_failed"
	// PhaseReasonOOMKilled: the container was killed for exceeding its memory limit. Distinct
	// from a plain non-zero exit because the fix is different — raise the request, not the code.
	PhaseReasonOOMKilled = "oom_killed"
)

// NeverSelfHealsPhaseReasons is the set of PhaseDetail.Reason values that will not resolve on
// their own, however long a job is left SUBMITTED/ADMITTED — a scheduler retry or the runtime's
// own reconcile loop cannot fix a bad image or a malformed config. A job reporting one of these
// is evicted EvictionUnschedulable well before StuckPendingTimeout, instead of occupying a
// reservation until the generic timeout catches it.
var NeverSelfHealsPhaseReasons = map[string]bool{
	PhaseReasonImagePullFailed: true,
	PhaseReasonConfigError:     true,
	PhaseReasonInvalidImage:    true,
}
