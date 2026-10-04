// Package agentloop is the backend-agnostic half of cluster-agent and bare-agent: fetch desired
// state from the control plane, diff it against whatever agentexec.Executor reports as actual,
// converge, and push status back.
package agentloop

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// DefaultLogTailLines is how many trailing lines FetchLogTail is asked for when the agent
// binary doesn't override it (see OptionalIntEnv("LOG_TAIL_LINES", ...) in each cmd/'s main.go).
const DefaultLogTailLines = 100

// finalStatusCaptureBudget bounds the whole last-chance observation-and-push phase for workloads
// about to be deleted — the entire phase, not each workload, so a slow or unreachable control
// plane costs the pass this much once rather than once per workload. Deleting them is the pass's
// actual job; capturing why they ended is worth a few seconds of it and no more.
const finalStatusCaptureBudget = 10 * time.Second

// Agent drives one cluster/node's reconcile and status-report loops against an Executor.
type Agent struct {
	ClusterName string
	APIURL      string
	// Token is this cluster's runtime token, sent on every call to the control plane.
	Token             string
	Executor          agentexec.Executor
	HTTPClient        *http.Client
	ReconcileInterval time.Duration
	StatusInterval    time.Duration
	// LogTailLines caps how many trailing lines FetchLogTail is asked for per status push.
	// Configurable per agent binary; DefaultLogTailLines if unset (zero).
	LogTailLines int
	// AutoscalerEnabled is the operator's own claim that this cluster sits behind a native
	// autoscaler (cluster-autoscaler / Karpenter) reacting to Pending pods — AUTOSCALER_ENABLED
	// env var, default false (fail-closed: a false positive costs a job a wasted scale-up
	// deadline, so this is never heuristically detected). See autoscaler.md.
	AutoscalerEnabled bool
	Log               func(format string, args ...any)

	// unrunnable is what the last reconcile pass found it can never create, by experiment, with
	// the attempt and the runtime's words. It is that pass's observation and nothing more: every
	// pass replaces it whole, and a status push reports it beside what exists.
	unrunnableMu sync.Mutex
	unrunnable   map[string]refusedCreate

	// removed is set once the control plane said this cluster is no longer registered. From
	// then on there is nobody to report to.
	removed atomic.Bool
}

// ErrClusterRemoved is what Run returns once the control plane no longer knows this cluster
// (its owner removed it, or the runtime detached it) and every workload it held is gone.
var ErrClusterRemoved = errors.New("this cluster was removed from the control plane")

type refusedCreate struct {
	attempt int
	reason  string
	why     string
}

// Run starts the reconcile and status-report loops and blocks until ctx is cancelled, which
// returns nil, or until the cluster was removed and its workloads are gone, which returns
// ErrClusterRemoved. ctx itself is left alone on that return: an executor's own operations run
// under it and finish what they started.
func (a *Agent) Run(ctx context.Context) error {
	statusCtx, stopStatus := context.WithCancel(ctx)
	defer stopStatus()
	var wg sync.WaitGroup
	wg.Add(2)
	var err error
	go func() { defer wg.Done(); a.statusReportLoop(statusCtx) }()
	go func() { defer wg.Done(); defer stopStatus(); err = a.reconcileLoop(ctx) }()
	wg.Wait()
	return err
}

// executorForPass is the executor one reconcile pass or one status push runs against: the
// agent's own, or the one a PassScoped executor hands out for the pass.
func (a *Agent) executorForPass(ctx context.Context) (agentexec.Executor, error) {
	scoped, ok := a.Executor.(agentexec.PassScoped)
	if !ok {
		return a.Executor, nil
	}
	return scoped.ForPass(ctx)
}

func (a *Agent) reconcileLoop(ctx context.Context) error {
	ticker := time.NewTicker(a.ReconcileInterval)
	defer ticker.Stop()
	for {
		err := a.reconcileOnce(ctx)
		if errors.Is(err, ErrClusterRemoved) {
			return err
		}
		if err != nil {
			a.Log("reconcile: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (a *Agent) reconcileOnce(ctx context.Context) error {
	exec, err := a.executorForPass(ctx)
	if err != nil {
		return fmt.Errorf("read actual state: %w", err)
	}
	desired, checkpointWindow, err := a.fetchDesiredState(ctx, exec)
	// A removed cluster desires nothing: its jobs went back to the queue with the removal, so
	// the pass runs on as one whose every workload is no longer desired, until none is left.
	removed := errors.Is(err, ErrClusterRemoved)
	if err != nil && !removed {
		return fmt.Errorf("fetch desired state: %w", err)
	}
	if removed {
		a.removed.Store(true)
	}
	actual, err := exec.ListManagedJobs(ctx)
	if err != nil {
		return fmt.Errorf("list local workloads: %w", err)
	}
	auxiliary, err := exec.ListManagedAuxiliaryWorkloads(ctx)
	if err != nil {
		return fmt.Errorf("list local auxiliary resources: %w", err)
	}
	if removed {
		// The status set also holds what is stopping and what stopped and is not reaped yet.
		held, err := exec.ListManagedJobsForStatus(ctx)
		if err != nil {
			return fmt.Errorf("list local workloads: %w", err)
		}
		if len(held) == 0 && len(auxiliary) == 0 {
			return ErrClusterRemoved
		}
		a.Log("this cluster was removed from the control plane: stopping the %d workloads it holds", len(held))
	}

	desiredByID := make(map[string]*domain.Experiment, len(desired))
	for _, exp := range desired {
		if exp == nil || exp.ID == "" {
			return fmt.Errorf("desired state contains experiment without identity")
		}
		if _, exists := desiredByID[exp.ID]; exists {
			return fmt.Errorf("desired state contains duplicate experiment %q", exp.ID)
		}
		desiredByID[exp.ID] = exp
	}
	actualSet := make(map[string]bool, len(actual))
	for _, id := range actual {
		if id == "" {
			return fmt.Errorf("actual state contains workload without experiment identity")
		}
		if actualSet[id] {
			return fmt.Errorf("actual state contains duplicate experiment identity %q", id)
		}
		actualSet[id] = true
	}
	var reconcileErrors []error
	refused := map[string]refusedCreate{}

	for id, exp := range desiredByID {
		if actualSet[id] {
			matches, err := exec.WorkloadMatchesDesired(ctx, exp)
			if err != nil {
				a.Log("compare workload %s with desired spec: %v", id, err)
				reconcileErrors = append(reconcileErrors, fmt.Errorf("compare workload %s: %w", id, err))
				continue
			}
			if !matches {
				// No checkpoint window: this is not a termination at all. The workload is
				// being replaced with the one desired state now asks for, and the next pass
				// recreates it — making the job save a checkpoint it is about to be restarted
				// past would only delay its own replacement.
				if err := exec.DeleteWorkload(ctx, id, false); err != nil {
					a.Log("delete drifted workload %s: %v", id, err)
					reconcileErrors = append(reconcileErrors, fmt.Errorf("delete drifted workload %s: %w", id, err))
					continue
				}
				a.Log("deleted drifted workload %s; next pass recreates desired spec", id)
			}
			continue
		}
		if err := exec.CreateWorkload(ctx, exp); err != nil {
			// A job that waits is reported with the next status push and asked for again. Its
			// wait is logged when it begins or its reason changes, not every pass.
			var waiting *agentexec.Waiting
			if errors.As(err, &waiting) {
				refused[id] = refusedCreate{attempt: exp.AttemptCount, reason: waiting.Reason, why: waiting.Why}
				if a.lastRefused(id) != refused[id] {
					a.Log("workload %s waits: %s: %s", id, waiting.Reason, waiting.Why)
				}
				continue
			}
			a.Log("create workload %s: %v", id, err)
			// A job that can never be created here is the control plane's to end. It is told
			// with the next status push, and the pass is not failed for it.
			var unrunnable *agentexec.Unrunnable
			if errors.As(err, &unrunnable) {
				refused[id] = refusedCreate{attempt: exp.AttemptCount, reason: cmp.Or(unrunnable.Reason, domain.PhaseReasonConfigError), why: unrunnable.Why}
				continue
			}
			reconcileErrors = append(reconcileErrors, fmt.Errorf("create workload %s: %w", id, err))
			continue
		}
	}
	a.unrunnableMu.Lock()
	a.unrunnable = refused
	a.unrunnableMu.Unlock()

	// Deleting a workload destroys the only copy of why it ended: once the pod is gone, its log
	// tail and its container's termination reason are unreadable forever. Observe everything once
	// more and push before deleting, or a job that died between two status pushes reaches the
	// control plane as FAILED with nothing attached — which is what turned single crashes into
	// long blind debugging sessions.
	//
	// Deliberately the ordinary full status push, not a targeted one for the workloads about to
	// go: a push IS a complete cluster snapshot (metricsdb.RecordJobStatuses rejects anything
	// else), and a job missing from the newest snapshot reads as JobPhaseGone — which the
	// controller evicts. Pushing a snapshot containing only the dying jobs would therefore tell
	// the control plane that every other job on this cluster had vanished.
	//
	// Bounded, because the deletions must happen this pass whatever the control plane is doing:
	// when the budget is spent the workloads are simply deleted uncaptured, since a workload left
	// running is worse than a lost log line.
	needsFinalCapture := false
	for id := range actualSet {
		if desiredByID[id] == nil {
			needsFinalCapture = true
			break
		}
	}
	if needsFinalCapture && !removed {
		captureCtx, cancelCapture := context.WithTimeout(ctx, finalStatusCaptureBudget)
		a.reportStatuses(captureCtx, exec)
		cancelCapture()
	}

	for id := range actualSet {
		if desiredByID[id] != nil {
			continue
		}
		// The one place a checkpoint window can apply: this workload is no longer desired, so
		// something ended it, and the control plane has already said which endings earn a
		// window. A workload absent from the grant list is deleted as it always has been.
		if err := exec.DeleteWorkload(ctx, id, checkpointWindow[id]); err != nil {
			a.Log("delete workload %s: %v", id, err)
			reconcileErrors = append(reconcileErrors, fmt.Errorf("delete workload %s: %w", id, err))
			continue
		}
		a.Log("deleted workload %s (no longer desired)", id)
	}
	for _, id := range orphanAuxiliaryIDs(desiredByID, actualSet, auxiliary) {
		if err := exec.DeleteWorkload(ctx, id, false); err != nil {
			a.Log("delete orphan auxiliary resources %s: %v", id, err)
			reconcileErrors = append(reconcileErrors, fmt.Errorf("delete orphan auxiliary resources %s: %w", id, err))
			continue
		}
		a.Log("deleted orphan auxiliary resources %s", id)
	}

	if err := exec.ReapTerminal(ctx, desiredByID); err != nil {
		a.Log("reap terminal workloads: %v", err)
		reconcileErrors = append(reconcileErrors, fmt.Errorf("reap terminal workloads: %w", err))
	}
	return errors.Join(reconcileErrors...)
}

func orphanAuxiliaryIDs(desired map[string]*domain.Experiment, actualJobs map[string]bool, auxiliary []string) []string {
	orphans := make([]string, 0, len(auxiliary))
	for _, id := range auxiliary {
		if desired[id] == nil && !actualJobs[id] {
			orphans = append(orphans, id)
		}
	}
	return orphans
}

// fetchDesiredState returns this cluster's desired workloads and the set of experiment ids whose
// termination the control plane granted a checkpoint window — the two halves of one pass's view:
// what should exist, and which of the things that should not may still exist for a moment.
func (a *Agent) fetchDesiredState(ctx context.Context, exec agentexec.Executor) ([]*domain.Experiment, map[string]bool, error) {
	cpuAvail, cpuTotal, err := exec.GetLiveCPUCapacity(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get live CPU capacity: %w", err)
	}
	acceleratorAvail, acceleratorTotal, acceleratorByNode, nodeLabels, err := exec.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get live accelerator capacity: %w", err)
	}
	ramAvail, ramTotal, err := exec.GetLiveRAMCapacity(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get live RAM capacity: %w", err)
	}
	storageAvail, storageTotal, err := exec.GetLiveStorageCapacity(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get live storage capacity: %w", err)
	}
	nodeResources, err := exec.GetLiveNodeResourceCapacity(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get live per-node resource capacity: %w", err)
	}
	nodeResourcesTotal, err := exec.GetNodeTotalCapacity(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get per-node total resource capacity: %w", err)
	}
	clusterID, err := exec.GetClusterID(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get cluster id: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"cpu_available_cores": cpuAvail, "cpu_total_cores": cpuTotal,
		"accelerator_available_by_type": acceleratorAvail, "accelerator_total_by_type": acceleratorTotal,
		"accelerator_available_by_node": acceleratorByNode,
		"node_resources_by_node":        nodeResources,
		"node_resources_total_by_node":  nodeResourcesTotal,
		"node_labels_by_node":           nodeLabels,
		"multi_node_capable":            exec.SupportsMultiNodeJobs(),
		"job_image":                     exec.OnlyImage(),
		"autoscaler_enabled":            a.AutoscalerEnabled,
		"cluster_id":                    clusterID,
		"ram_available_bytes":           ramAvail, "ram_total_bytes": ramTotal,
		"storage_available_bytes": storageAvail, "storage_total_bytes": storageTotal,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("encode reconcile snapshot: %w", err)
	}
	url := fmt.Sprintf("%s/internal/clusters/%s/reconcile", a.APIURL, a.ClusterName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, refusal(resp)
	}
	var body struct {
		Experiments []*domain.Experiment `json:"experiments"`
		// CheckpointWindow is the ids the control plane decided are entitled to their declared
		// checkpoint window on the way out. A plain grant list, not a class or a rule: this loop
		// hands each id straight to DeleteWorkload and interprets nothing.
		CheckpointWindow []string `json:"checkpoint_window"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, nil, err
	}
	granted := make(map[string]bool, len(body.CheckpointWindow))
	for _, id := range body.CheckpointWindow {
		granted[id] = true
	}
	return body.Experiments, granted, nil
}

type statusReportWire struct {
	ExperimentID            string `json:"experiment_id"`
	Phase                   string `json:"phase"`
	AdmittedAcceleratorType string `json:"admitted_accelerator_type,omitempty"`
	AdmittedNode            string `json:"admitted_node,omitempty"`
	// AdmittedMachine is what stands behind AdmittedNode, and goes only with it.
	AdmittedMachine *domain.JobMachine `json:"admitted_machine,omitempty"`
	LogTail         []string           `json:"log_tail,omitempty"`
	Reason          string             `json:"reason,omitempty"`
	Message         string             `json:"message,omitempty"`
	RestartCount    int32              `json:"restart_count,omitempty"`
	// ScheduledNodes/SchedulingReason carry the gang-readiness facts PollPhaseDetail computes —
	// see agentexec.Executor.PollPhaseDetail.
	ScheduledNodes   int32  `json:"scheduled_nodes,omitempty"`
	SchedulingReason string `json:"scheduling_reason,omitempty"`
	// Attempt is the generation of the experiment this observation came from. A pointer so an
	// executor that could not establish one (workload.AttemptUnknown) sends no field at all
	// rather than a number: the control plane reads absence as "cannot fence" and accepts the
	// report, and reads a number as a claim it will check against desired state.
	Attempt *int `json:"attempt,omitempty"`
	// StartedAt and EndedAt are JobStatus's, sent together or not at all.
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

func (a *Agent) statusReportLoop(ctx context.Context) {
	ticker := time.NewTicker(a.StatusInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !a.removed.Load() {
				a.reportChangedStatuses(ctx)
			}
		}
	}
}

func (a *Agent) reportChangedStatuses(ctx context.Context) {
	exec, err := a.executorForPass(ctx)
	if err != nil {
		a.Log("read actual state for status: %v", err)
		return
	}
	a.reportStatuses(ctx, exec)
}

// reportStatuses pushes one snapshot observed through exec. The reconcile pass hands it its own
// executor for the last push before a delete, so that push costs no second read of actual state.
func (a *Agent) reportStatuses(ctx context.Context, exec agentexec.Executor) {
	ids, err := exec.ListManagedJobsForStatus(ctx)
	if err != nil {
		a.Log("list managed jobs for status: %v", err)
		return
	}

	// All or nothing, deliberately. A push is a complete cluster snapshot — the control plane
	// rejects anything else (metricsdb.RecordJobStatuses) and reads a job missing from the newest
	// snapshot as JobPhaseGone, which the controller evicts. So omitting a job we merely failed to
	// observe would report it as vanished and get it killed, which is far worse than this tick
	// reporting nothing: a cluster with no fresh snapshot reads as "cannot tell", and the
	// controller declines to act on it (see checkSilence's !found branch).
	reports := make([]statusReportWire, 0, len(ids))
	for _, id := range ids {
		report, ok := a.statusReportFor(ctx, exec, id)
		if !ok {
			a.Log("status snapshot incomplete (%s unobservable); skipping this push rather than reporting the rest as gone", id)
			return
		}
		reports = append(reports, report)
	}
	reports = append(reports, a.refusedReports(ids)...)

	a.pushStatus(ctx, reports)
}

// refusedReports is the status of every job the last reconcile pass did not create and that has
// no workload: pending, under the reason the runtime gave, which the control plane ends the job
// for when it is one that never heals, and with the runtime's words as the message its owner
// reads.
// lastRefused is what the pass before this one said of id.
func (a *Agent) lastRefused(id string) refusedCreate {
	a.unrunnableMu.Lock()
	defer a.unrunnableMu.Unlock()
	return a.unrunnable[id]
}

func (a *Agent) refusedReports(existing []string) []statusReportWire {
	a.unrunnableMu.Lock()
	defer a.unrunnableMu.Unlock()
	var reports []statusReportWire
	for id, refused := range a.unrunnable {
		if slices.Contains(existing, id) {
			continue
		}
		attempt := refused.attempt
		reports = append(reports, statusReportWire{
			ExperimentID: id, Attempt: &attempt, Phase: workload.JobPhasePending.String(),
			Reason: refused.reason, Message: refused.why,
		})
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].ExperimentID < reports[j].ExperimentID })
	return reports
}

// statusReportFor observes one job: its phase, where it landed, its log tail and why its
// container is in the state it is in. ok is false when the job could not be observed coherently,
// which costs the whole snapshot — see reportChangedStatuses for why a partial one is dangerous.
func (a *Agent) statusReportFor(ctx context.Context, exec agentexec.Executor, id string) (statusReportWire, bool) {
	// One combined poll keeps phase, UID and attempt from separate reads from being combined
	// into an impossible observation during a delete/recreate race.
	observed, err := exec.PollJobStatus(ctx, id)
	if err != nil {
		a.Log("poll job phase %s: %v", id, err)
		return statusReportWire{}, false
	}
	phase := observed.Phase
	// Attribution is reported when it can be resolved and left empty when it cannot. It says
	// which accelerator the job landed on, not whether the job exists, so failing to read it is
	// not grounds to withhold the phase — and withholding the phase costs the whole cluster's
	// snapshot. A job between pods during a delete-and-recreate has a phase but no attribution
	// yet, which used to blank out status for every other job in the cluster until it settled.
	// The control plane bounds an unattributable job on its own (see job_watcher's
	// runningTypeUnobservable), which it can only do if it keeps hearing the phase.
	var admittedAcceleratorType, admittedNode string
	var admittedMachine *domain.JobMachine
	if phase != workload.JobPhaseGone {
		t, node, consistent, err := exec.ResolveAdmittedAcceleratorType(ctx, id)
		switch {
		case err != nil:
			a.Log("resolve admitted accelerator type %s: %v", id, err)
		case !consistent:
			a.Log("experiment %s: scheduled ranks landed on inconsistent accelerator types", id)
		default:
			admittedAcceleratorType = string(t)
			admittedNode = node
			if node != "" {
				admittedMachine = observed.Machine
			}
		}
	}

	// A job the runtime no longer holds has nothing left to read: skipped because there is no
	// source, not because reading it failed.
	var logTail []string
	var reason, message string
	var restartCount, scheduledNodes int32
	var schedulingReason string
	if phase != workload.JobPhaseGone {
		// Diagnostics accompany the phase; they are not the phase. A read error on one job must
		// not stop this batch reporting phase for every other job (important.md #19), so it is
		// logged and that job reports phase alone.
		logTail, err = exec.FetchLogTail(ctx, id, a.logTailLines())
		if err != nil {
			a.Log("fetch log tail %s: %v", id, err)
			logTail = nil
		}
		reason, message, restartCount, scheduledNodes, schedulingReason, err = exec.PollPhaseDetail(ctx, id, observed.Attempt)
		if err != nil {
			a.Log("poll phase detail %s: %v", id, err)
			reason, message, restartCount, scheduledNodes, schedulingReason = "", "", 0, 0, ""
		}
	}

	var attempt *int
	if observed.Attempt != workload.AttemptUnknown {
		attempt = &observed.Attempt
	}

	var startedAt, endedAt *time.Time
	if !observed.EndedAt.IsZero() {
		startedAt, endedAt = &observed.StartedAt, &observed.EndedAt
	}

	return statusReportWire{
		ExperimentID:            id,
		Attempt:                 attempt,
		StartedAt:               startedAt,
		EndedAt:                 endedAt,
		Phase:                   phase.String(),
		AdmittedAcceleratorType: admittedAcceleratorType,
		AdmittedNode:            admittedNode,
		AdmittedMachine:         admittedMachine,
		LogTail:                 logTail,
		Reason:                  reason,
		Message:                 message,
		RestartCount:            restartCount,
		ScheduledNodes:          scheduledNodes,
		SchedulingReason:        schedulingReason,
	}, true
}

// refusalBodyLimit bounds how much of a refusal's body is carried into the log.
const refusalBodyLimit = 512

// refusal is the control plane's own words for why it did not take a request. The loop goes on
// either way: a 401 or 403 is a token or a registration somebody has to fix, and the reason is
// what tells them which.
func refusal(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, refusalBodyLimit))
	var said struct {
		Reason string `json:"reason"`
	}
	if resp.StatusCode == http.StatusUnauthorized && json.Unmarshal(body, &said) == nil && said.Reason == domain.ReasonClusterNotRegistered {
		return ErrClusterRemoved
	}
	return fmt.Errorf("control plane answered %d: %s", resp.StatusCode, bytes.TrimSpace(body))
}

// Detach ends this cluster on the control plane, as its owner would by removing it: its jobs go
// back to the queue, named in requeued, and its runtime token is refused from then on. The
// running agent reads that refusal, stops the workloads it holds and exits.
func (a *Agent) Detach(ctx context.Context) (requeued []string, err error) {
	url := fmt.Sprintf("%s/internal/clusters/%s", a.APIURL, a.ClusterName)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, refusal(resp)
	}
	var body struct {
		Requeued []string `json:"requeued"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Requeued, nil
}

type pushStatusResponse struct {
	Status string `json:"status"`
}

func (a *Agent) pushStatus(ctx context.Context, reports []statusReportWire) {
	buf, err := json.Marshal(map[string]any{"reports": reports})
	if err != nil {
		a.Log("push status: encode request: %v", err)
		return
	}
	url := fmt.Sprintf("%s/internal/clusters/%s/status", a.APIURL, a.ClusterName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		a.Log("push status: build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		a.Log("push status: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		a.Log("push status: %v", refusal(resp))
		return
	}

	var body pushStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		a.Log("push status: decode response: %v", err)
		return
	}
	if body.Status != "ok" {
		a.Log("push status: invalid acknowledgement status %q", body.Status)
	}
}

// RequiredEnv reads an environment variable or exits the process with a labeled error — the
// startup-validation shape shared by every agent binary's main().
func RequiredEnv(binaryName, key string) string {
	value := os.Getenv(key)
	if value == "" {
		fmt.Fprintf(os.Stderr, "%s: %s environment variable is required\n", binaryName, key)
		os.Exit(1)
	}
	return value
}

// OptionalIntEnv reads an integer environment variable, returning 0 when it is unset and exiting
// the process with a labeled error when it is set but malformed.
func OptionalIntEnv(binaryName, key string) int {
	raw := os.Getenv(key)
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s=%q is not an integer\n", binaryName, key, raw)
		os.Exit(1)
	}
	return value
}

// OptionalBoolEnv reads a boolean environment variable, returning false when it is unset and
// exiting the process with a labeled error when it is set but malformed.
func OptionalBoolEnv(binaryName, key string) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return false
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s=%q is not a boolean\n", binaryName, key, raw)
		os.Exit(1)
	}
	return value
}

// SignalContext returns a context cancelled on SIGTERM/SIGINT, logging via logFn when that
// happens — the shutdown-handling shape shared by every agent binary's main().
func SignalContext(logFn func(format string, args ...any)) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		logFn("shutting down")
		cancel()
	}()
	return ctx, cancel
}

// logTailLines is how many trailing log lines a status report carries.
func (a *Agent) logTailLines() int {
	if a.LogTailLines <= 0 {
		return DefaultLogTailLines
	}
	return a.LogTailLines
}

// CheckAccess makes the call every pass starts with and acts on nothing it answers, so an
// installer learns that the control plane refuses this cluster's token before it starts an
// agent that would only log the refusal.
func (a *Agent) CheckAccess(ctx context.Context) error {
	_, _, err := a.fetchDesiredState(ctx, a.Executor)
	return err
}

// RunDetach is the detach command of an agent binary: it ends the cluster named by the
// environment the agent itself runs with and says what became of its jobs. A cluster already
// removed is the state asked for.
func RunDetach(ctx context.Context, binaryName string, log func(format string, args ...any)) *Agent {
	a := &Agent{
		ClusterName: RequiredEnv(binaryName, "CLUSTER_NAME"),
		APIURL:      RequiredEnv(binaryName, "API_URL"),
		Token:       RequiredEnv(binaryName, "RUNTIME_TOKEN"),
		HTTPClient:  &http.Client{Timeout: 35 * time.Second},
		Log:         log,
	}
	requeued, err := a.Detach(ctx)
	switch {
	case errors.Is(err, ErrClusterRemoved):
		log("cluster %s is already removed from %s", a.ClusterName, a.APIURL)
	case err != nil:
		fmt.Fprintf(os.Stderr, "%s: detach %s: %v\n", binaryName, a.ClusterName, err)
		os.Exit(1)
	default:
		log("detached cluster %s from %s: %d of its jobs went back to the queue", a.ClusterName, a.APIURL, len(requeued))
	}
	return a
}
