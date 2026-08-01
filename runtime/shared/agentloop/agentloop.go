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

// Agent drives one cluster/node's reconcile and status-report loops against an Executor.
type Agent struct {
	ClusterName       string
	ControlPlaneURL   string
	Executor          agentexec.Executor
	HTTPClient        *http.Client
	ReconcileInterval time.Duration
	StatusInterval    time.Duration
	Log               func(format string, args ...any)
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
	url := fmt.Sprintf("%s/internal/clusters/%s/reconcile", a.ControlPlaneURL, a.ClusterName)
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
	ExperimentID            string `json:"experiment_id"`
	Phase                   string `json:"phase"`
	AdmittedAcceleratorType string `json:"admitted_accelerator_type,omitempty"`
	AdmittedNode            string `json:"admitted_node,omitempty"`
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

		reports = append(reports, statusReportWire{
			ExperimentID:            id,
			Phase:                   phase.String(),
			AdmittedAcceleratorType: admittedAcceleratorType,
			AdmittedNode:            admittedNode,
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
	url := fmt.Sprintf("%s/internal/clusters/%s/status", a.ControlPlaneURL, a.ClusterName)
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
