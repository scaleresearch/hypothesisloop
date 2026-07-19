package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/workload"
)

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
		log("deleted workload %s (no longer desired)", id)
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

	// Same idea per accelerator flavor, JSON-encoded since it's a map rather than a scalar. The control
	// plane writes these straight to the metrics store (never Postgres) — see clusteragentapi.
	if acceleratorAvail, acceleratorTotal, err := a.jwc.GetLiveAcceleratorCapacity(ctx); err != nil {
		fmt.Printf("[cluster-agent] get live accelerator capacity: %v\n", err)
	} else if len(acceleratorAvail) > 0 {
		availJSON, _ := json.Marshal(acceleratorAvail)
		totalJSON, _ := json.Marshal(acceleratorTotal)
		q := neturl.Values{}
		q.Set("accelerator_available_by_flavor", string(availJSON))
		q.Set("accelerator_total_by_flavor", string(totalJSON))
		sep := "&"
		if !strings.Contains(url, "?") {
			sep = "?"
		}
		url = url + sep + q.Encode()
	}

	// RAM/ephemeral-storage: Class B (hard-cap, no billing) dimensions — see
	// SCHEDULING_GENERALIZATION_PLAN.md's Class B step 2. Same fast-poll piggyback as CPU/Accelerator
	// above, in bytes, so the control plane can joint-fit a mixed job's memory/storage request
	// against real cluster state instead of trusting it fits unconditionally.
	if ramAvail, ramTotal, err := a.jwc.GetLiveRAMCapacity(ctx); err != nil {
		fmt.Printf("[cluster-agent] get live RAM capacity: %v\n", err)
	} else {
		sep := "&"
		if !strings.Contains(url, "?") {
			sep = "?"
		}
		url = fmt.Sprintf("%s%sram_available_bytes=%d&ram_total_bytes=%d", url, sep, ramAvail, ramTotal)
	}
	if storageAvail, storageTotal, err := a.jwc.GetLiveStorageCapacity(ctx); err != nil {
		fmt.Printf("[cluster-agent] get live storage capacity: %v\n", err)
	} else {
		sep := "&"
		if !strings.Contains(url, "?") {
			sep = "?"
		}
		url = fmt.Sprintf("%s%sstorage_available_bytes=%d&storage_total_bytes=%d", url, sep, storageAvail, storageTotal)
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
