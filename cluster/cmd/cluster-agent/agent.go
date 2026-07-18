package main

import (
	"context"
	"net/http"
	"sync"

	"github.com/scaleresearch/openresearch/controlplane/shared/workload"
)

type trackedJob struct {
	lastPhase workload.JobPhase
	// seq is a per-job, per-process monotonic counter for this job's own report stream —
	// incremented on every report built for this job, regardless of delivery outcome. Ordering
	// only needs to hold within one job's reports from one agent process (this is always a
	// single writer per job), so a local counter fully replaces wall-clock sequencing without
	// the clock-skew/non-monotonic-across-restart hazards of time.Now().UnixNano().
	seq int64
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
