package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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
	auxiliary, err := a.jwc.ListManagedAuxiliaryWorkloads(ctx)
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

	// Create anything desired but not yet actual.
	for id, exp := range desiredByID {
		if actualSet[id] {
			matches, err := a.jwc.WorkloadMatchesDesired(ctx, exp)
			if err != nil {
				log("compare workload %s with desired spec: %v", id, err)
				reconcileErrors = append(reconcileErrors, fmt.Errorf("compare workload %s: %w", id, err))
				continue
			}
			if !matches {
				if err := a.jwc.DeleteWorkload(ctx, id); err != nil {
					log("delete drifted workload %s: %v", id, err)
					reconcileErrors = append(reconcileErrors, fmt.Errorf("delete drifted workload %s: %w", id, err))
					continue
				}
				log("deleted drifted workload %s; next pass recreates desired spec", id)
			}
			continue
		}
		if err := a.jwc.CreateWorkload(ctx, exp); err != nil {
			log("create workload %s: %v", id, err)
			reconcileErrors = append(reconcileErrors, fmt.Errorf("create workload %s: %w", id, err))
			continue
		}
	}

	// Delete anything actual but no longer desired.
	for id := range actualSet {
		if desiredByID[id] != nil {
			continue
		}
		if err := a.jwc.DeleteWorkload(ctx, id); err != nil {
			log("delete workload %s: %v", id, err)
			reconcileErrors = append(reconcileErrors, fmt.Errorf("delete workload %s: %w", id, err))
			continue
		}
		log("deleted workload %s (no longer desired)", id)
	}
	// A partial create/delete can leave a Service or DRA template without a Job. Discover and
	// remove those orphans from current cluster state; retain no cleanup queue between ticks.
	for _, id := range orphanAuxiliaryIDs(desiredByID, actualSet, auxiliary) {
		if err := a.jwc.DeleteWorkload(ctx, id); err != nil {
			log("delete orphan auxiliary resources %s: %v", id, err)
			reconcileErrors = append(reconcileErrors, fmt.Errorf("delete orphan auxiliary resources %s: %w", id, err))
			continue
		}
		log("deleted orphan auxiliary resources %s", id)
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

func (a *agent) fetchDesiredState(ctx context.Context) ([]*domain.Experiment, error) {
	cpuAvail, cpuTotal, err := a.jwc.GetLiveCPUCapacity(ctx)
	if err != nil {
		return nil, fmt.Errorf("get live CPU capacity: %w", err)
	}
	acceleratorAvail, acceleratorTotal, acceleratorByNode, nodeLabels, err := a.jwc.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("get live accelerator capacity: %w", err)
	}
	ramAvail, ramTotal, err := a.jwc.GetLiveRAMCapacity(ctx)
	if err != nil {
		return nil, fmt.Errorf("get live RAM capacity: %w", err)
	}
	storageAvail, storageTotal, err := a.jwc.GetLiveStorageCapacity(ctx)
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
	url := fmt.Sprintf("%s/internal/clusters/%s/reconcile", a.cpURL, a.clusterName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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
