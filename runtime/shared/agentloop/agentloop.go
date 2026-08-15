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

// StatusLister is implemented by executors that report a different job set for status pushes
// than for reconcile. Executors without this method fall back to Executor.ListManagedJobs.
type StatusLister interface {
	ListManagedJobsForStatus(ctx context.Context) ([]string, error)
}

// Reaper is implemented by executors that retain terminal (stopped) records between ticks and
// need an explicit prune once those records are no longer desired.
type Reaper interface {
	ReapTerminal(ctx context.Context, desired map[string]*domain.Experiment) error
}

// LogTailer is implemented by executors that can report a job's own recent stdout/stderr —
// k8sexec (pod logs via the Kubernetes API) and podexec (container logs) both do. Reported
// alongside job phase in the same status push: the control plane never reaches into a cluster
// to pull this itself, and no job process ever pushes its own logs — only the executor watching
// it from the runtime side does, same as phase.
type LogTailer interface {
	FetchLogTail(ctx context.Context, experimentID string, maxLines int) ([]string, error)
}

// PhaseDetailer is implemented by executors that can explain *why* a job's container hasn't
// started or has been restarting — k8sexec (a pod's containerStatuses[].state.waiting/terminated)
// and podexec (a container's inspected State) both do. reason is one of the control plane's
// generic domain.PhaseReason* vocabulary (empty if the executor has nothing notable to report);
// translating the runtime's own native taxonomy into that vocabulary happens here, in the
// executor, so the control plane never learns a runtime's vocabulary (important.md #7). Read
// live from the runtime on every call — no caching (important.md #4).
type PhaseDetailer interface {
	PollPhaseDetail(ctx context.Context, experimentID string) (reason, message string, restartCount int32, err error)
}

// DefaultLogTailLines is how many trailing lines FetchLogTail is asked for when the agent
// binary doesn't override it (see RequiredEnv("LOG_TAIL_LINES", ...) in each cmd/'s main.go).
const DefaultLogTailLines = 100

// splitLongLines breaks any line over maxLineChars into multiple lines, done once here (client
// side, before the report ever leaves this process) rather than left to the control plane to
// truncate silently on ingest -- this way nothing is ever dropped, only wrapped. maxLineChars
// comes from hypothesisloop.yaml's scheduler.max_log_tail_line_chars (see Agent.MaxLogLineChars)
// — large enough to keep a real compiler error or stack frame intact, small enough that one
// pathological line (a base64 blob, a JSON dump with no newlines) can't blow up a status push.
func splitLongLines(lines []string, maxLineChars int) []string {
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

	if reaper, ok := a.Executor.(Reaper); ok {
		if err := reaper.ReapTerminal(ctx, desiredByID); err != nil {
			a.Log("reap terminal workloads: %v", err)
			reconcileErrors = append(reconcileErrors, fmt.Errorf("reap terminal workloads: %w", err))
		}
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
	payload, err := json.Marshal(map[string]any{
		"cpu_available_cores": cpuAvail, "cpu_total_cores": cpuTotal,
		"accelerator_available_by_type": acceleratorAvail, "accelerator_total_by_type": acceleratorTotal,
		"accelerator_available_by_node": acceleratorByNode,
		"node_labels_by_node":           nodeLabels,
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
	var ids []string
	var err error
	if lister, ok := a.Executor.(StatusLister); ok {
		ids, err = lister.ListManagedJobsForStatus(ctx)
	} else {
		ids, err = a.Executor.ListManagedJobs(ctx)
	}
	if err != nil {
		a.Log("list managed jobs for status: %v", err)
		return
	}

	reports := make([]statusReportWire, 0, len(ids))
	for _, id := range ids {
		// One combined poll keeps phase and UID from two different reads from being combined
		// into an impossible observation during a delete/recreate race.
		phase, _, err := a.Executor.PollJobPhaseAndUID(ctx, id)
		if err != nil {
			a.Log("poll job phase %s: %v", id, err)
			return
		}
		var admittedAcceleratorType, admittedNode string
		if phase != workload.JobPhaseGone {
			t, node, consistent, err := a.Executor.ResolveAdmittedAcceleratorType(ctx, id)
			if err != nil {
				a.Log("resolve admitted accelerator type %s: %v", id, err)
				return
			}
			admittedAcceleratorType = string(t)
			admittedNode = node
			if !consistent {
				a.Log("experiment %s: scheduled ranks landed on inconsistent accelerator types", id)
				return
			}
		}

		var logTail []string
		if tailer, ok := a.Executor.(LogTailer); ok && phase != workload.JobPhaseGone {
			maxLines := a.LogTailLines
			if maxLines <= 0 {
				maxLines = DefaultLogTailLines
			}
			// Best-effort: a job that hasn't produced output yet, or a transient read error,
			// must not block reporting phase for every other job in this same batch.
			logTail, err = tailer.FetchLogTail(ctx, id, maxLines)
			if err != nil {
				a.Log("fetch log tail %s: %v", id, err)
				logTail = nil
			} else {
				logTail = splitLongLines(logTail, a.MaxLogLineChars)
			}
		}

		var reason, message string
		var restartCount int32
		if detailer, ok := a.Executor.(PhaseDetailer); ok && phase != workload.JobPhaseGone {
			// Best-effort, same as log tail above: a transient read error must not block
			// reporting phase for every other job in this same batch.
			reason, message, restartCount, err = detailer.PollPhaseDetail(ctx, id)
			if err != nil {
				a.Log("poll phase detail %s: %v", id, err)
				reason, message, restartCount = "", "", 0
			}
		}

		reports = append(reports, statusReportWire{
			ExperimentID:            id,
			Phase:                   phase.String(),
			AdmittedAcceleratorType: admittedAcceleratorType,
			AdmittedNode:            admittedNode,
			LogTail:                 logTail,
			Reason:                  reason,
			Message:                 message,
			RestartCount:            restartCount,
		})
	}

	a.pushStatus(ctx, reports)
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
