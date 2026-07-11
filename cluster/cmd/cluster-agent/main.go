// openresearch-cluster-agent is the only component with credentials to a target cluster's
// Kubernetes API. It runs inside that cluster and reconciles its local Jobs against a
// desired-state view it fetches from the control plane (GET .../desired-state) — the same
// model a kubelet uses: pull desired state, diff against actual, converge, report status
// back. The control plane never dials into this cluster; every call here is outbound.
//
// No leader election: every operation is naturally idempotent (Job creation tolerates
// AlreadyExists, deletion tolerates NotFound, status pushes are sequence-numbered) and
// desired-state is a read-only, side-effect-free GET, so any number of replicas can run this
// loop concurrently and safely. Extra replicas just mean some redundant polling — never a
// correctness problem. Run more than one only for availability if you want it; one is fine.
//
// Environment variables:
//
//	CLUSTER_NAME        — this cluster's name as registered in clusters.yaml (default: default)
//	CONTROLPLANE_URL    — base URL of scheduler-service (default: http://scheduler-service:8082)
//	OPENRESEARCH_CONFIG — path to openresearch.yaml (default: settings/openresearch.yaml) — GPU
//	                      type catalog and GPUResourceName, the k8s extended resource requested
//	                      per GPU (execution-engine detail; agents never see this).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	openresearchcfg "github.com/scaleresearch/openresearch/controlplane/shared/config"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/workload"
)

const (
	reconcileInterval = 2 * time.Second
	statusInterval    = 3 * time.Second
)

func main() {
	clusterName := envOrDefault("CLUSTER_NAME", "default")
	controlPlaneURL := envOrDefault("CONTROLPLANE_URL", "http://scheduler-service:8082")
	// Passed to every Job this agent creates as OPENRESEARCH_REGISTRY_URL — must be reachable
	// from *inside* the training pod, not from the agent itself, so it can't just default to
	// registry-service's in-cluster DNS name the way CONTROLPLANE_URL does for this process;
	// there is no such Service unless this cluster also runs the control plane's own compose
	// stack reachable under that name. Must be set explicitly for local/dev clusters where the
	// control plane lives outside the cluster (e.g. http://host.docker.internal:8083).
	registryURL := os.Getenv("REGISTRY_URL")
	pcfg := openresearchcfg.MustLoad(envOrDefault("OPENRESEARCH_CONFIG", "settings/openresearch.yaml"))

	// JobWorkloadClient does all the actual Job/PriorityClass/Namespace work against this
	// cluster's own API server — it resolves in-cluster credentials automatically when given
	// no kubeconfig/context, which is exactly what a pod running inside the cluster has.
	jwc, err := workload.New(workload.Config{
		RegistryURL:     registryURL,
		GPUResourceName: pcfg.GPUResourceName,
		GPUTaintKey:     pcfg.GPUTaintKey,
		OpenResearchConfig: &workload.OpenResearchConfig{
			NodeLabelByType:    pcfg.NodeLabelByType,
			NodeLabelKeyByType: pcfg.NodeLabelKeyByType,
			ResourceNameByType: pcfg.ResourceNameByType,
			TaintKeyByType:     pcfg.TaintKeyByType,
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cluster-agent: workload client: %v\n", err)
		os.Exit(1)
	}

	log := func(format string, args ...any) {
		fmt.Printf("[cluster-agent] "+format+"\n", args...)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		log("shutting down")
		cancel()
	}()

	log("starting: cluster=%s control_plane=%s", clusterName, controlPlaneURL)

	setupCtx, setupCancel := context.WithTimeout(ctx, 30*time.Second)
	if err := jwc.SetupCluster(setupCtx); err != nil {
		log("SetupCluster: %v", err)
	}
	setupCancel()

	a := &agent{
		clusterName: clusterName,
		cpURL:       controlPlaneURL,
		jwc:         jwc,
		httpClient:  &http.Client{Timeout: 35 * time.Second},
		tracked:     map[string]*trackedJob{},
	}
	a.resyncTrackedFromCluster(ctx, log)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.reconcileLoop(ctx, log) }()
	go func() { defer wg.Done(); a.statusReportLoop(ctx, log) }()
	wg.Wait()
}

type trackedJob struct {
	lastPhase workload.JobPhase
	seq       int64
}

type agent struct {
	clusterName string
	cpURL       string
	jwc         *workload.JobWorkloadClient
	httpClient  *http.Client

	mu      sync.Mutex
	tracked map[string]*trackedJob
}

// resyncTrackedFromCluster rebuilds the in-memory tracked-experiment set from Jobs already
// present in the cluster (labeled openresearch.io/managed-by=openresearch) — self-healing
// after an agent restart, same idea as a k8s controller's relist-on-start.
func (a *agent) resyncTrackedFromCluster(ctx context.Context, log func(string, ...any)) {
	jobs, err := a.jwc.ListManagedJobs(ctx)
	if err != nil {
		log("resync list jobs: %v", err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, expID := range jobs {
		if _, ok := a.tracked[expID]; !ok {
			a.tracked[expID] = &trackedJob{lastPhase: workload.JobPhasePending}
		}
	}
	log("resync: tracking %d existing job(s)", len(a.tracked))
}

// reconcileLoop is the whole "desired state" side: fetch what should exist, list what
// actually exists, converge the difference. It never needs the control plane to push
// anything — this loop always initiates.
func (a *agent) reconcileLoop(ctx context.Context, log func(string, ...any)) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		if err := a.reconcileOnce(ctx, log); err != nil {
			log("reconcile: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *agent) reconcileOnce(ctx context.Context, log func(string, ...any)) error {
	desired, err := a.fetchDesiredState(ctx)
	if err != nil {
		return fmt.Errorf("fetch desired state: %w", err)
	}
	actual, err := a.jwc.ListManagedJobs(ctx)
	if err != nil {
		return fmt.Errorf("list local jobs: %w", err)
	}

	desiredByID := make(map[string]*domain.Experiment, len(desired))
	for _, exp := range desired {
		desiredByID[exp.ID] = exp
	}
	actualSet := make(map[string]bool, len(actual))
	for _, id := range actual {
		actualSet[id] = true
	}

	// Create anything desired but not yet actual.
	for id, exp := range desiredByID {
		if actualSet[id] {
			continue
		}
		if err := a.jwc.CreateWorkload(ctx, exp); err != nil {
			log("create workload %s: %v", id, err)
			continue
		}
		a.track(id)
	}

	// Delete anything actual but no longer desired.
	for id := range actualSet {
		if desiredByID[id] != nil {
			continue
		}
		if err := a.jwc.DeleteWorkload(ctx, id); err != nil {
			log("delete workload %s: %v", id, err)
			continue
		}
		a.track(id) // keep tracking until the status loop observes and reports Gone
	}

	// Track everything currently desired too, even if already actual (covers the case where
	// resync missed it, or a previous reconcile pass's create hasn't been observed yet).
	for id := range desiredByID {
		a.track(id)
	}
	return nil
}

func (a *agent) track(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.tracked[id]; !ok {
		a.tracked[id] = &trackedJob{lastPhase: workload.JobPhasePending}
	}
}

func (a *agent) fetchDesiredState(ctx context.Context) ([]*domain.Experiment, error) {
	url := fmt.Sprintf("%s/internal/clusters/%s/desired-state", a.cpURL, a.clusterName)

	// Attach live CPU capacity as query params on this same fast (~2s) poll — reusing the
	// existing desired-state pull instead of adding a second endpoint/cadence keeps capacity
	// reporting on the same fast path as job status and heartbeats.
	if avail, total, err := a.jwc.GetLiveCPUCapacity(ctx); err != nil {
		fmt.Printf("[cluster-agent] get live CPU capacity: %v\n", err)
	} else {
		url = fmt.Sprintf("%s?cpu_available_cores=%.3f&cpu_total_cores=%.3f", url, avail, total)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.httpClient.Do(req)
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
	ExperimentID    string `json:"experiment_id"`
	Phase           string `json:"phase"`
	AdmittedGPUType string `json:"admitted_gpu_type,omitempty"`
	SequenceNumber  int64  `json:"sequence_number"`
	JobUID          string `json:"job_uid,omitempty"`
}

// statusReportLoop polls locally-known Job phases (in-cluster API access, always available)
// and pushes changes to the control plane. This is the outbound-only replacement for the
// control plane polling PollJobPhase against a cluster it no longer has credentials for.
func (a *agent) statusReportLoop(ctx context.Context, log func(string, ...any)) {
	ticker := time.NewTicker(statusInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.reportChangedStatuses(ctx, log)
		}
	}
}

func (a *agent) reportChangedStatuses(ctx context.Context, log func(string, ...any)) {
	a.mu.Lock()
	ids := make([]string, 0, len(a.tracked))
	for id := range a.tracked {
		ids = append(ids, id)
	}
	a.mu.Unlock()

	var reports []statusReportWire
	var toRemove []string
	for _, id := range ids {
		phase, err := a.jwc.PollJobPhase(ctx, id)
		if err != nil {
			log("poll job phase %s: %v", id, err)
			continue
		}
		a.mu.Lock()
		tj, ok := a.tracked[id]
		if !ok {
			a.mu.Unlock()
			continue
		}
		if phase == tj.lastPhase {
			a.mu.Unlock()
			continue
		}
		tj.lastPhase = phase
		tj.seq++
		seq := tj.seq
		a.mu.Unlock()

		// Fetch the Job UID fresh on every reported phase change — this only runs when the
		// phase actually changed (already rate-limited), so a plain uncached query is simpler
		// than caching it and just as cheap in practice.
		var uid string
		if phase != workload.JobPhaseGone {
			if u, err := a.jwc.GetJobUID(ctx, id); err != nil {
				log("get job UID %s: %v", id, err)
			} else {
				uid = u
			}
		}

		reports = append(reports, statusReportWire{
			ExperimentID:   id,
			Phase:          phase.String(),
			SequenceNumber: seq,
			JobUID:         uid,
		})
		if phase == workload.JobPhaseGone {
			toRemove = append(toRemove, id)
		}
		// Succeeded/Failed are terminal for the experiment but the Job object itself may
		// still exist until reconcile deletes it (status leaves the desired set on the
		// control-plane side) — keep tracking until we actually observe Gone, so the
		// eventual deletion is still reported.
	}

	if len(reports) > 0 {
		a.pushStatus(ctx, reports, log)
	}

	if len(toRemove) > 0 {
		a.mu.Lock()
		for _, id := range toRemove {
			delete(a.tracked, id)
		}
		a.mu.Unlock()
	}
}

func (a *agent) pushStatus(ctx context.Context, reports []statusReportWire, log func(string, ...any)) {
	buf, _ := json.Marshal(map[string]any{"reports": reports})
	url := fmt.Sprintf("%s/internal/clusters/%s/status", a.cpURL, a.clusterName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		log("push status: %v", err)
		return
	}
	resp.Body.Close()
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
