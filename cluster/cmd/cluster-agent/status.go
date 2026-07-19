package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/workload"
)

type statusReportWire struct {
	ExperimentID    string `json:"experiment_id"`
	Phase           string `json:"phase"`
	AdmittedAcceleratorType string `json:"admitted_accelerator_type,omitempty"`
	// AdmittedNode is the k8s node name the job's rank-0 pod actually landed on — the sole
	// job→physical-node attribution signal in the system (see ResolveAdmittedAcceleratorType's
	// doc comment). Forwarded straight into the metrics store by the control plane, never
	// stored in Postgres.
	AdmittedNode    string `json:"admitted_node,omitempty"`
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

// candidateReport pairs an outbound wire report with the typed phase it represents, so a
// confirmed delivery can be committed back into trackedJob without re-parsing phase.String().
type candidateReport struct {
	wire  statusReportWire
	id    string
	phase workload.JobPhase
	uid   string
}

func (a *agent) reportChangedStatuses(ctx context.Context, log func(string, ...any)) {
	a.mu.Lock()
	ids := make([]string, 0, len(a.tracked))
	for id := range a.tracked {
		ids = append(ids, id)
	}
	a.mu.Unlock()

	var candidates []candidateReport
	for _, id := range ids {
		// One combined poll (phase + UID) instead of two separate Get()s a phase-changed-only
		// gate below would otherwise have to sequence around — see PollJobPhaseAndUID's doc
		// comment for why comparing UID (not just phase) is required, not just cheaper: a
		// delete-then-recreate cycle (e.g. preempt-then-readmit reusing the same experiment ID)
		// can land back on the same phase string across two polls, which a phase-only
		// comparison would silently treat as "no change" — permanently losing the old Job's
		// Gone transition and, downstream, WaitForJobDeletion's confirmation of freed capacity.
		phase, uid, err := a.jwc.PollJobPhaseAndUID(ctx, id)
		if err != nil {
			log("poll job phase %s: %v", id, err)
			continue
		}
		a.mu.Lock()
		tj, ok := a.tracked[id]
		if !ok || (phase == tj.lastPhase && uid == tj.lastUID) {
			a.mu.Unlock()
			continue
		}
		tj.seq++
		seq := tj.seq
		a.mu.Unlock()

		var admittedAcceleratorType, admittedNode string
		if phase != workload.JobPhaseGone {
			// Resolve which accelerator type (and node) this job's pod(s) actually landed on —
			// meaningful only once at least one pod is scheduled onto a node. An empty result
			// (nothing scheduled yet, or a query error) simply omits the field: the control plane
			// leaves whatever admitted type it already has untouched (see
			// ClusterQueueStore.UpsertJobReport's COALESCE), falling back to the originally
			// requested type until a real observation arrives.
			t, node, consistent, err := a.jwc.ResolveAdmittedAcceleratorType(ctx, id)
			if err != nil {
				log("resolve admitted accelerator type %s: %v", id, err)
			} else {
				admittedAcceleratorType = string(t)
				admittedNode = node
				if !consistent {
					log("experiment %s: scheduled ranks landed on inconsistent accelerator types — reporting rank 0's %s", id, t)
				}
			}
		}

		// tj.seq (bumped above, under the lock) is a per-job, per-process monotonic counter —
		// ordering only needs to be correct within one job's own report stream from this one
		// agent process, which a local counter guarantees unconditionally, unlike a wall-clock
		// reading that can go non-monotonic across a clock correction.
		candidates = append(candidates, candidateReport{
			wire: statusReportWire{
				ExperimentID:    id,
				Phase:           phase.String(),
				AdmittedAcceleratorType: admittedAcceleratorType,
				AdmittedNode:    admittedNode,
				SequenceNumber:  seq,
				JobUID:          uid,
			},
			id:    id,
			phase: phase,
			uid:   uid,
		})
		// Succeeded/Failed are terminal for the experiment but the Job object itself may
		// still exist until reconcile deletes it (status leaves the desired set on the
		// control-plane side) — keep tracking until we actually observe Gone, so the
		// eventual deletion is still reported.
	}

	if len(candidates) == 0 {
		return
	}

	reports := make([]statusReportWire, len(candidates))
	for i, c := range candidates {
		reports[i] = c.wire
	}

	// Only commit lastPhase (and only drop Gone jobs from tracking) for reports the control
	// plane's response confirms were durably applied. On a transport failure, non-2xx
	// response, or a report the response lists as rejected (DB error or a stale/duplicate
	// sequence number server-side), tracked state is left untouched so the same phase
	// transition is recomputed and retried on the next tick — the tracked map itself is the
	// retry queue, with no separate outbox needed.
	accepted, ok := a.pushStatus(ctx, reports, log)
	if !ok {
		return
	}

	a.mu.Lock()
	for _, c := range candidates {
		if !accepted[c.id] {
			continue
		}
		if c.phase == workload.JobPhaseGone {
			delete(a.tracked, c.id)
			continue
		}
		if tj, ok := a.tracked[c.id]; ok {
			tj.lastPhase = c.phase
			tj.lastUID = c.uid
		}
	}
	a.mu.Unlock()
}

// pushStatusResponse is the control plane's per-batch reply: rejected lists the experiment IDs
// whose report did not durably apply (DB error, or a stale/duplicate sequence number, or an
// experiment not owned by this cluster) — everything else in the batch was applied.
type pushStatusResponse struct {
	Rejected []string `json:"rejected"`
}

// pushStatus delivers reports to the control plane. ok reports whether the request itself
// succeeded (2xx); when ok, accepted maps each experiment ID in reports to whether the control
// plane durably applied that specific report — callers must not advance local state for an
// experiment ID absent from accepted (or mapped false), so it's naturally retried next tick.
func (a *agent) pushStatus(ctx context.Context, reports []statusReportWire, log func(string, ...any)) (accepted map[string]bool, ok bool) {
	buf, _ := json.Marshal(map[string]any{"reports": reports})
	url := fmt.Sprintf("%s/internal/clusters/%s/status", a.cpURL, a.clusterName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		log("push status: build request: %v", err)
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		log("push status: %v", err)
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log("push status: unexpected status %d", resp.StatusCode)
		return nil, false
	}

	var body pushStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log("push status: decode response: %v", err)
		return nil, false
	}
	rejected := make(map[string]bool, len(body.Rejected))
	for _, id := range body.Rejected {
		rejected[id] = true
	}
	accepted = make(map[string]bool, len(reports))
	for _, r := range reports {
		accepted[r.ExperimentID] = !rejected[r.ExperimentID]
	}
	return accepted, true
}
