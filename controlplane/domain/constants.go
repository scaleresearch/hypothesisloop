package domain

import (
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

// MinRemainingHours is the minimum rescheduled duration after preemption (15 minutes).
const MinRemainingHours = 0.25

// ExperimentStatus represents the lifecycle state of an agent job.
type ExperimentStatus string

const (
	StatusSubmitted ExperimentStatus = "SUBMITTED"
	StatusQueued    ExperimentStatus = "QUEUED"
	StatusRunning   ExperimentStatus = "RUNNING"
	StatusCompleted ExperimentStatus = "COMPLETED"
	StatusFailed    ExperimentStatus = "FAILED"
	StatusEvicted   ExperimentStatus = "EVICTED"
	StatusRejected  ExperimentStatus = "REJECTED"
	// StatusCancelled is what a job somebody cancelled is answered as, and no row holds it: such
	// a job is stored REJECTED when it never ran and EVICTED when it did, with EvictionCancelled
	// as its reason. See AnsweredStatus.
	StatusCancelled ExperimentStatus = "CANCELLED"
)

// AnsweredStatus is the status the API answers for a job stored with this status and eviction
// reason, on a job, in a count, as a filter and in a watch event. It is the one place a cancel
// is told apart.
func AnsweredStatus(stored ExperimentStatus, evictionReason string) ExperimentStatus {
	if EvictionReason(evictionReason).Code() == EvictionCancelled {
		return StatusCancelled
	}
	return stored
}

// ValidExperimentStatus reports whether s is one of the statuses a job is answered with. Callers
// filtering on a status validate here first, so an unrecognized value is the client mistake it is
// and no driver error.
func ValidExperimentStatus(s ExperimentStatus) bool {
	switch s {
	case StatusSubmitted, StatusQueued, StatusRunning,
		StatusCompleted, StatusFailed, StatusEvicted, StatusRejected, StatusCancelled:
		return true
	default:
		return false
	}
}

const (
	// NotAdmittedAwaitingAdmission is the reason of a job the admission loop has not evaluated
	// since it was queued or requeued. Every other reason is a verdict of that loop.
	NotAdmittedAwaitingAdmission   = "awaiting_admission"
	NotAdmittedCapacityUnavailable = "capacity_unavailable"
	NotAdmittedOutranked           = "outranked"
	NotAdmittedStageCut            = "stage_cut"
	NotAdmittedJobTooLong          = "job_too_long"
	NotAdmittedWorkloadCreation    = "workload_creation_failed"
	// NotAdmittedInsufficientQuota is the reason of a queued job whose reservation its agent's
	// quota no longer covers at claim time.
	NotAdmittedInsufficientQuota = "insufficient_quota"
	// NotAdmittedNoMultiNodeCluster means the job spans more than one node and no connected
	// cluster reports being able to run multi-node work. Distinct from capacity_unavailable
	// because waiting will not fix it: this is not a busy platform, it is a platform with no
	// runtime for this shape of job.
	NotAdmittedNoMultiNodeCluster = "no_multi_node_cluster"
	// NotAdmittedNoClusterForOrg means no connected cluster may take this org's jobs: none is
	// its own and none is shared. Waiting does not fix it; registering a cluster does.
	NotAdmittedNoClusterForOrg = "no_cluster_for_org"
	// NotAdmittedClustersDisabled means the org has clusters it can use and has turned every one
	// of them off for its jobs. Waiting does not fix it; turning one on does.
	NotAdmittedClustersDisabled = "clusters_disabled"
	// NotAdmittedNoClusterForImage means the job's image choice keeps it off every cluster taking
	// its org's jobs: none runs the image it names, which holds of every cluster for a job stored
	// with none, or those that run its choice have installed less than it asks for while a cluster
	// its choice rules out has it. Waiting does not fix it; the reason says which choice runs
	// where. A submit is refused with it, and a job queued before the clusters changed carries it.
	NotAdmittedNoClusterForImage = "no_cluster_for_image"
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
	// NotAdmittedConcurrencyCap means the experiment's platform-experiment pool already holds
	// max_concurrent_accelerators (or the global default) accelerators in flight (SUBMITTED+
	// RUNNING); admitting this job would exceed that spend-rate control (autoscaler.md's
	// concurrency cap).
	NotAdmittedConcurrencyCap = "concurrency_cap"
	// NotAdmittedExceedsAcceleratorShare means an explicit cpu/memory/storage request is above
	// the share its accelerator count gets on the chosen cluster's nodes. Waiting does not help;
	// a smaller request does, or a node with more of that resource per accelerator joining.
	NotAdmittedExceedsAcceleratorShare = "exceeds_accelerator_share"
	// NotAdmittedExceedsClusterCapacity means the job asks for more than any connected cluster
	// that can run it has installed, on every accelerator type it accepts, and none of them
	// scales up. Waiting does not help; a smaller request does.
	NotAdmittedExceedsClusterCapacity = "exceeds_cluster_capacity"
)

// QueueReasonAwaitingAdmission is the stored reason of a job the admission loop has not looked at
// yet. Every stored reason is "code: sentence" (SplitQueueReason), and this is the one the store
// writes itself.
const QueueReasonAwaitingAdmission = NotAdmittedAwaitingAdmission + ": the scheduler has not looked at the job since it was queued, which it does within seconds"

// IsTerminal reports whether the status is a final lifecycle state that no further execution
// or progress reporting can follow.
func (s ExperimentStatus) IsTerminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusEvicted, StatusRejected, StatusCancelled:
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

// QueueReasonClearsByWaiting reports whether a not_admitted_reason describes a job that only
// waits its turn: for the scheduler's first look, free capacity or a scale-up.
// Every other reason stays until its agent changes the job.
func QueueReasonClearsByWaiting(reason string) bool {
	code, _, _ := strings.Cut(reason, ":")
	switch code {
	case NotAdmittedAwaitingAdmission, NotAdmittedCapacityUnavailable, NotAdmittedOutranked,
		NotAdmittedWaitingForScaleUp, NotAdmittedNoScalableCapacity, NotAdmittedConcurrencyCap:
		return true
	}
	return false
}

// SplitQueueReason splits a stored not_admitted_reason, "code" or "code: message", into the code
// a client branches on and the sentence a person or an agent reads.
func SplitQueueReason(stored string) (reason, message string) {
	reason, message, _ = strings.Cut(stored, ": ")
	return reason, message
}

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
	NodeResourceCPUMillicores = clusterapi.NodeResourceCPUMillicores
	NodeResourceMemoryBytes   = clusterapi.NodeResourceMemoryBytes
	NodeResourceStorageBytes  = clusterapi.NodeResourceStorageBytes
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
	// EvictionStuckPending marks a job that was admitted (SUBMITTED) but never
	// reported RUNNING within StuckPendingTimeoutSeconds — e.g. unschedulable due to
	// fragmentation or a bad image. See job_watcher.go.
	EvictionStuckPending EvictionReason = "stuck_pending"
	// EvictionResourceDisbalance terminates a RUNNING job whose requested CPU/memory/storage is
	// grossly out of proportion to the accelerators it holds, while that request is provably the
	// only thing keeping a queued job off idle accelerators on the same physical node. Terminal
	// rather than requeued: the job's own spec is what makes it unplaceable alongside its
	// neighbours, so returning it to the queue unchanged would evict it again on the next tick.
	// See service/scheduler/loop_disbalance.go.
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
	// EvictionAcceleratorTypeMismatch marks a job whose cluster reported it running on an accelerator type
	// other than the one reserved and billed for it. Unlike missing telemetry this cannot resolve
	// itself by waiting — the placement already happened and is wrong — so it is terminal rather
	// than retried. Only raised for an observation newer than the experiment's current
	// submitted_at, so a stale sample from a previous admission can never trigger it.
	EvictionAcceleratorTypeMismatch EvictionReason = "accelerator_type_mismatch"
	// EvictionAcceleratorTypeUnobservable marks a job whose pod the runtime reports as Running but
	// whose accelerator type never became readable, so the lifecycle could not be advanced to
	// RUNNING. Deliberately not EvictionStuckPending: the pod did start and is burning real
	// hardware, and calling that "stuck pending" would both misname the fault and imply a
	// never-ran refund. See scheduler.JobWatcher.reconcileOne.
	EvictionAcceleratorTypeUnobservable EvictionReason = "accelerator_type_unobservable"
	// EvictionPreemptedForGuaranteed marks a burst job returned to QUEUED to free capacity for a
	// guaranteed one. Not terminal: the job runs again.
	EvictionPreemptedForGuaranteed EvictionReason = "preempted_for_guaranteed"
	// EvictionScaleUpTimeout marks a job whose gang never fully bound (scheduled_nodes < Nodes())
	// within its placement deadline on an autoscaler-enabled cluster — either the deadline
	// passed, or the runtime reported a terminal scheduling_reason (e.g. "NotTriggerScaleUp: max
	// node group size reached") before that. An infrastructure fault, requeued for free on the
	// same allowance as every other, with the cluster kept in the job's tried_clusters.
	EvictionScaleUpTimeout EvictionReason = "scale_up_timeout"

	// EvictionCodeUnreachable is a job whose command never ran because its code could not be
	// fetched: the platform's git credential or the repository did not answer. The runtime
	// reports it as PhaseReasonCodeUnreachable. An infrastructure fault like every other:
	// requeued for free while the allowance lasts, then EVICTED and refunded.
	EvictionCodeUnreachable EvictionReason = "code_unreachable"

	// EvictionNoOffer is a job admitted to a cluster that rents machines and found none to rent
	// within the time its operator allows (PhaseReasonNoOffer): the offer admission counted was
	// gone when the runtime asked for it. An infrastructure fault: the job goes back to the queue
	// and waits there until the cluster reports a machine on offer again.
	EvictionNoOffer EvictionReason = "no_offer"

	// EvictionClusterWithdrawn is a job requeued because the cluster it was placed on stopped
	// taking it: the platform ended the sharing, or the cluster's org removed it. The job did
	// nothing wrong and spends no retry.
	EvictionClusterWithdrawn EvictionReason = "cluster_withdrawn"
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
}

// The PhaseReason* vocabulary is the cluster contract's (clusterapi).
const (
	PhaseReasonImagePullFailed  = clusterapi.PhaseReasonImagePullFailed
	PhaseReasonConfigError      = clusterapi.PhaseReasonConfigError
	PhaseReasonInvalidImage     = clusterapi.PhaseReasonInvalidImage
	PhaseReasonContainerFailed  = clusterapi.PhaseReasonContainerFailed
	PhaseReasonOOMKilled        = clusterapi.PhaseReasonOOMKilled
	PhaseReasonCodeUnreachable  = clusterapi.PhaseReasonCodeUnreachable
	PhaseReasonNoOffer          = clusterapi.PhaseReasonNoOffer
	PhaseReasonWaitingForOffer  = clusterapi.PhaseReasonWaitingForOffer
	PhaseReasonInstanceStarting = clusterapi.PhaseReasonInstanceStarting
	PhaseReasonInstanceUnknown  = clusterapi.PhaseReasonInstanceUnknown
	PhaseReasonCreateFailed     = clusterapi.PhaseReasonCreateFailed
	ExitCodeCodeUnreachable     = clusterapi.ExitCodeCodeUnreachable
)

// failureCauseDetailRunes bounds the runtime's message in a failure cause: the cause is stored on
// the job and travels in every update about it.
const failureCauseDetailRunes = 400

// FailureNoReason is the cause of a failed job whose runtime did not say how its process ended.
const FailureNoReason = "failed, runtime gave no reason; see `hl job logs`"

// FailureCause is the reason a job that failed is stored and announced with: how the runtime
// says its process ended, in the "code: detail" shape of an eviction reason. A failed job always
// has one. Where the runtime's last word is not about a process that ended, the cause says so,
// with what a runtime that named no reason said beside it, its exit code where it reports one.
func (d PhaseDetail) FailureCause() string {
	message := d.detail()
	switch {
	case d.Reason == "" && message != "":
		return FailureNoReason + " (" + message + ")"
	case d.Reason != PhaseReasonContainerFailed && d.Reason != PhaseReasonOOMKilled:
		return FailureNoReason
	case message == "":
		return d.Reason
	}
	return d.Reason + ": " + message
}

// detail is the runtime's message as a reason carries it: its first line, bounded.
func (d PhaseDetail) detail() string {
	message, _, _ := strings.Cut(d.Message, "\n")
	if runes := []rune(message); len(runes) > failureCauseDetailRunes {
		message = string(runes[:failureCauseDetailRunes])
	}
	return message
}

// EvictionOfFailure is the eviction a failed process is ended with where the failure was the
// environment's and no doing of the job, and false for a failure of the job's own.
func (d PhaseDetail) EvictionOfFailure() (EvictionReason, bool) {
	if d.Reason != PhaseReasonCodeUnreachable {
		return "", false
	}
	if message := d.detail(); message != "" {
		return EvictionCodeUnreachable.WithDetail(message), true
	}
	return EvictionCodeUnreachable, true
}

// EvictionOfNeverStarting is the eviction a placement is ended with whose runtime reported a
// reason in NeverSelfHealsPhaseReasons, with what the runtime said so the update about it says
// why: "no_offer: <sentence>" where the runtime found no machine, and for a job whose own image
// or configuration cannot start "unschedulable: <the runtime's reason>: <sentence>".
func (d PhaseDetail) EvictionOfNeverStarting() EvictionReason {
	code, said := EvictionUnschedulable, d.Said()
	if d.Reason == PhaseReasonNoOffer {
		code, said = EvictionNoOffer, d.detail()
	}
	if said == "" {
		return code
	}
	return code.WithDetail(said)
}

// Said is what the runtime reported: its reason, then the first line of its message.
func (d PhaseDetail) Said() string {
	message := d.detail()
	switch {
	case message == "":
		return d.Reason
	case d.Reason == "":
		return message
	}
	return d.Reason + ": " + message
}

// NeverSelfHealsPhaseReasons is the set of PhaseDetail.Reason values that will not resolve on
// their own, however long a job is left SUBMITTED — a scheduler retry or the runtime's
// own reconcile loop cannot fix a bad image or a malformed config. A job reporting one of these
// is evicted (EvictionOfNeverStarting) well before StuckPendingTimeout, instead of occupying a
// reservation until the generic timeout catches it.
var NeverSelfHealsPhaseReasons = map[string]bool{
	PhaseReasonImagePullFailed: true,
	PhaseReasonConfigError:     true,
	PhaseReasonInvalidImage:    true,
	PhaseReasonNoOffer:         true,
}
