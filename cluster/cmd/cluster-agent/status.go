package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

type statusReportWire struct {
	ExperimentID            string `json:"experiment_id"`
	Phase                   string `json:"phase"`
	AdmittedAcceleratorType string `json:"admitted_accelerator_type,omitempty"`
	// AdmittedNode is the k8s node name the job's rank-0 pod actually landed on — the sole
	// job→physical-node attribution signal in the system (see ResolveAdmittedAcceleratorType's
	// doc comment). Forwarded straight into the metrics store by the control plane, never
	// stored in Postgres.
	AdmittedNode string `json:"admitted_node,omitempty"`
}

// statusReportLoop relists every managed Job and pushes the complete actual-state snapshot on
// every tick. It retains no discovery, transition, or retry history between ticks. This is the
// outbound-only replacement for the control plane polling a cluster it has no credentials for.
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
	ids, err := a.jwc.ListManagedJobs(ctx)
	if err != nil {
		log("list managed jobs for status: %v", err)
		return
	}

	reports := make([]statusReportWire, 0, len(ids))
	for _, id := range ids {
		// One combined poll keeps phase and UID from two different Kubernetes reads from being
		// combined into an impossible observation during a delete/recreate race.
		phase, _, err := a.jwc.PollJobPhaseAndUID(ctx, id)
		if err != nil {
			log("poll job phase %s: %v", id, err)
			return
		}
		var admittedAcceleratorType, admittedNode string
		if phase != workload.JobPhaseGone {
			// Resolve which accelerator type (and node) this job's pod(s) actually landed on —
			// meaningful only once at least one pod is scheduled onto a node. An empty result
			// (nothing scheduled yet, or a query error) simply omits the field: the control plane
			// records nothing until a real observation arrives.
			t, node, consistent, err := a.jwc.ResolveAdmittedAcceleratorType(ctx, id)
			if err != nil {
				log("resolve admitted accelerator type %s: %v", id, err)
				return
			} else {
				admittedAcceleratorType = string(t)
				admittedNode = node
				if !consistent {
					log("experiment %s: scheduled ranks landed on inconsistent accelerator types", id)
					return
				}
			}
		}

		// Report every observation, even when phase and UID are unchanged.
		// The phase metric is also the actual-state heartbeat consumed by the stale
		// desired-state sweep.
		reports = append(reports, statusReportWire{
			ExperimentID:            id,
			Phase:                   phase.String(),
			AdmittedAcceleratorType: admittedAcceleratorType,
			AdmittedNode:            admittedNode,
		})
	}

	a.pushStatus(ctx, reports, log)
}

type pushStatusResponse struct {
	Status string `json:"status"`
}

// pushStatus delivers one snapshot to the control plane. Failures need no retained retry state:
// the next tick relists Kubernetes and sends the then-current snapshot again.
func (a *agent) pushStatus(ctx context.Context, reports []statusReportWire, log func(string, ...any)) {
	buf, err := json.Marshal(map[string]any{"reports": reports})
	if err != nil {
		log("push status: encode request: %v", err)
		return
	}
	url := fmt.Sprintf("%s/internal/clusters/%s/status", a.cpURL, a.clusterName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		log("push status: build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		log("push status: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log("push status: unexpected status %d", resp.StatusCode)
		return
	}

	var body pushStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log("push status: decode response: %v", err)
		return
	}
	if body.Status != "ok" {
		log("push status: invalid acknowledgement status %q", body.Status)
	}
}
