// Package agentloop is the backend-agnostic half of cluster-agent and bare-agent: fetch desired
// state from the control plane, diff it against whatever agentexec.Executor reports as actual,
// converge, and push status back.
package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// DefaultLogTailLines is how many trailing lines FetchLogTail is asked for when the agent
// binary doesn't override it (see RequiredEnv("LOG_TAIL_LINES", ...) in each cmd/'s main.go).
const DefaultLogTailLines = 100

// finalStatusCaptureBudget bounds the whole last-chance observation-and-push phase for workloads
// about to be deleted — the entire phase, not each workload, so a slow or unreachable control
// plane costs the pass this much once rather than once per workload. Deleting them is the pass's
// actual job; capturing why they ended is worth a few seconds of it and no more.
const finalStatusCaptureBudget = 10 * time.Second

// splitLongLines breaks any line over maxLineChars into multiple lines, done once here (client
// side, before the report ever leaves this process) rather than left to the control plane to
// truncate silently on ingest -- this way nothing is ever dropped, only wrapped. maxLineChars
// comes from hypothesisloop.yaml's scheduler.max_log_tail_line_chars (see Agent.MaxLogLineChars)
// — large enough to keep a real compiler error or stack frame intact, small enough that one
// pathological line (a base64 blob, a JSON dump with no newlines) can't blow up a status push.
func splitLongLines(lines []string, maxLineChars int) []string {
	if maxLineChars <= 0 {
		return lines
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		for len(line) > maxLineChars {
			out = append(out, line[:maxLineChars])
			line = line[maxLineChars:]
		}
		out = append(out, line)
	}
	return out
}

// Agent drives one cluster/node's reconcile and status-report loops against an Executor.
type Agent struct {
	ClusterName       string
	APIURL            string
	Executor          agentexec.Executor
	HTTPClient        *http.Client
	ReconcileInterval time.Duration
	StatusInterval    time.Duration
	// LogTailLines caps how many trailing lines FetchLogTail is asked for per status push.
	// Configurable per agent binary; DefaultLogTailLines if unset (zero).
	LogTailLines int
	// MaxLogLineChars is hypothesisloop.yaml's scheduler.max_log_tail_line_chars — required
	// (config validation already enforces > 0), not defaulted here.
	MaxLogLineChars int
	Log             func(format string, args ...any)
}

// Run starts the reconcile and status-report loops and blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.reconcileLoop(ctx) }()
	go func() { defer wg.Done(); a.statusReportLoop(ctx) }()
	wg.Wait()
}

func (a *Agent) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(a.ReconcileInterval)
	defer ticker.Stop()
	for {
		if err := a.reconcileOnce(ctx); err != nil {
			a.Log("reconcile: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *Agent) reconcileOnce(ctx context.Context) error {
	desired, err := a.fetchDesiredState(ctx)
	if err != nil {
		return fmt.Errorf("fetch desired state: %w", err)
	}
	actual, err := a.Executor.ListManagedJobs(ctx)
	if err != nil {
		return fmt.Errorf("list local workloads: %w", err)
	}
	auxiliary, err := a.Executor.ListManagedAuxiliaryWorkloads(ctx)
	if err != nil {
		return fmt.Errorf("list local auxiliary resources: %w", err)
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

	for id, exp := range desiredByID {
		if actualSet[id] {
			matches, err := a.Executor.WorkloadMatchesDesired(ctx, exp)
			if err != nil {
				a.Log("compare workload %s with desired spec: %v", id, err)
				reconcileErrors = append(reconcileErrors, fmt.Errorf("compare workload %s: %w", id, err))
				continue
			}
			if !matches {
				if err := a.Executor.DeleteWorkload(ctx, id); err != nil {
					a.Log("delete drifted workload %s: %v", id, err)
					reconcileErrors = append(reconcileErrors, fmt.Errorf("delete drifted workload %s: %w", id, err))
					continue
				}
				a.Log("deleted drifted workload %s; next pass recreates desired spec", id)
			}
			continue
		}
		if err := a.Executor.CreateWorkload(ctx, exp); err != nil {
			a.Log("create workload %s: %v", id, err)
			reconcileErrors = append(reconcileErrors, fmt.Errorf("create workload %s: %w", id, err))
			continue
		}
	}

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
	if needsFinalCapture {
		captureCtx, cancelCapture := context.WithTimeout(ctx, finalStatusCaptureBudget)
		a.reportChangedStatuses(captureCtx)
		cancelCapture()
	}

	for id := range actualSet {
		if desiredByID[id] != nil {
			continue
		}
		if err := a.Executor.DeleteWorkload(ctx, id); err != nil {
			a.Log("delete workload %s: %v", id, err)
			reconcileErrors = append(reconcileErrors, fmt.Errorf("delete workload %s: %w", id, err))
			continue
		}
		a.Log("deleted workload %s (no longer desired)", id)
	}
	for _, id := range orphanAuxiliaryIDs(desiredByID, actualSet, auxiliary) {
		if err := a.Executor.DeleteWorkload(ctx, id); err != nil {
			a.Log("delete orphan auxiliary resources %s: %v", id, err)
			reconcileErrors = append(reconcileErrors, fmt.Errorf("delete orphan auxiliary resources %s: %w", id, err))
			continue
		}
		a.Log("deleted orphan auxiliary resources %s", id)
	}

	if err := a.Executor.ReapTerminal(ctx, desiredByID); err != nil {
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

func (a *Agent) fetchDesiredState(ctx context.Context) ([]*domain.Experiment, error) {
	cpuAvail, cpuTotal, err := a.Executor.GetLiveCPUCapacity(ctx)
	if err != nil {
		return nil, fmt.Errorf("get live CPU capacity: %w", err)
	}
	acceleratorAvail, acceleratorTotal, acceleratorByNode, nodeLabels, err := a.Executor.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("get live accelerator capacity: %w", err)
	}
	ramAvail, ramTotal, err := a.Executor.GetLiveRAMCapacity(ctx)
	if err != nil {
		return nil, fmt.Errorf("get live RAM capacity: %w", err)
	}
	storageAvail, storageTotal, err := a.Executor.GetLiveStorageCapacity(ctx)
	if err != nil {
		return nil, fmt.Errorf("get live storage capacity: %w", err)
	}
	nodeResources, err := a.Executor.GetLiveNodeResourceCapacity(ctx)
	if err != nil {
		return nil, fmt.Errorf("get live per-node resource capacity: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"cpu_available_cores": cpuAvail, "cpu_total_cores": cpuTotal,
		"accelerator_available_by_type": acceleratorAvail, "accelerator_total_by_type": acceleratorTotal,
		"accelerator_available_by_node": acceleratorByNode,
		"node_resources_by_node":        nodeResources,
		"node_labels_by_node":           nodeLabels,
		"multi_node_capable":            a.Executor.SupportsMultiNodeJobs(),
		"ram_available_bytes":           ramAvail, "ram_total_bytes": ramTotal,
		"storage_available_bytes": storageAvail, "storage_total_bytes": storageTotal,
	})
	if err != nil {
		return nil, fmt.Errorf("encode reconcile snapshot: %w", err)
	}
	url := fmt.Sprintf("%s/internal/clusters/%s/reconcile", a.APIURL, a.ClusterName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var body struct {
		Experiments []*domain.Experiment `json:"experiments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Experiments, nil
}

type statusReportWire struct {
	ExperimentID            string   `json:"experiment_id"`
	Phase                   string   `json:"phase"`
	AdmittedAcceleratorType string   `json:"admitted_accelerator_type,omitempty"`
	AdmittedNode            string   `json:"admitted_node,omitempty"`
	LogTail                 []string `json:"log_tail,omitempty"`
	Reason                  string   `json:"reason,omitempty"`
	Message                 string   `json:"message,omitempty"`
	RestartCount            int32    `json:"restart_count,omitempty"`
}

func (a *Agent) statusReportLoop(ctx context.Context) {
	ticker := time.NewTicker(a.StatusInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.reportChangedStatuses(ctx)
		}
	}
}

func (a *Agent) reportChangedStatuses(ctx context.Context) {
	ids, err := a.Executor.ListManagedJobsForStatus(ctx)
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
		report, ok := a.statusReportFor(ctx, id)
		if !ok {
			a.Log("status snapshot incomplete (%s unobservable); skipping this push rather than reporting the rest as gone", id)
			return
		}
		reports = append(reports, report)
	}

	a.pushStatus(ctx, reports)
}

// statusReportFor observes one job: its phase, where it landed, its log tail and why its
// container is in the state it is in. ok is false when the job could not be observed coherently,
// which costs the whole snapshot — see reportChangedStatuses for why a partial one is dangerous.
func (a *Agent) statusReportFor(ctx context.Context, id string) (statusReportWire, bool) {
	// One combined poll keeps phase and UID from two different reads from being combined
	// into an impossible observation during a delete/recreate race.
	phase, _, err := a.Executor.PollJobPhaseAndUID(ctx, id)
	if err != nil {
		a.Log("poll job phase %s: %v", id, err)
		return statusReportWire{}, false
	}
	// Attribution is reported when it can be resolved and left empty when it cannot. It says
	// which accelerator the job landed on, not whether the job exists, so failing to read it is
	// not grounds to withhold the phase — and withholding the phase costs the whole cluster's
	// snapshot. A job between pods during a delete-and-recreate has a phase but no attribution
	// yet, which used to blank out status for every other job in the cluster until it settled.
	// The control plane bounds an unattributable job on its own (see job_watcher's
	// runningTypeUnobservable), which it can only do if it keeps hearing the phase.
	var admittedAcceleratorType, admittedNode string
	if phase != workload.JobPhaseGone {
		t, node, consistent, err := a.Executor.ResolveAdmittedAcceleratorType(ctx, id)
		switch {
		case err != nil:
			a.Log("resolve admitted accelerator type %s: %v", id, err)
		case !consistent:
			a.Log("experiment %s: scheduled ranks landed on inconsistent accelerator types", id)
		default:
			admittedAcceleratorType = string(t)
			admittedNode = node
		}
	}

	// A job the runtime no longer holds has nothing left to read: skipped because there is no
	// source, not because reading it failed.
	var logTail []string
	var reason, message string
	var restartCount int32
	if phase != workload.JobPhaseGone {
		// Diagnostics accompany the phase; they are not the phase. A read error on one job must
		// not stop this batch reporting phase for every other job (important.md #19), so it is
		// logged and that job reports phase alone.
		logTail, err = a.Executor.FetchLogTail(ctx, id, a.logTailLines())
		if err != nil {
			a.Log("fetch log tail %s: %v", id, err)
			logTail = nil
		} else {
			logTail = splitLongLines(logTail, a.MaxLogLineChars)
		}
		reason, message, restartCount, err = a.Executor.PollPhaseDetail(ctx, id)
		if err != nil {
			a.Log("poll phase detail %s: %v", id, err)
			reason, message, restartCount = "", "", 0
		}
	}

	return statusReportWire{
		ExperimentID:            id,
		Phase:                   phase.String(),
		AdmittedAcceleratorType: admittedAcceleratorType,
		AdmittedNode:            admittedNode,
		LogTail:                 logTail,
		Reason:                  reason,
		Message:                 message,
		RestartCount:            restartCount,
	}, true
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
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		a.Log("push status: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		a.Log("push status: unexpected status %d", resp.StatusCode)
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
