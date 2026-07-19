// Package clusteragentapi exposes the HTTP surface cluster-agents call — and only
// cluster-agents call. Every request in this package is initiated by an agent running
// inside a target cluster; the control plane never calls out. Endpoints:
//
//	GET  /internal/clusters/{name}/desired-state — "what should currently be running here?"
//	POST /internal/clusters/{name}/status        — push job phase observations
//
// There is no command/ack protocol: an experiment's own status (SUBMITTED/ADMITTED/RUNNING)
// is the single source of truth for "should a Job exist" — the agent fetches that view and
// reconciles its local Jobs to match, the same way a kubelet reconciles pods to a desired
// spec. This means desired-state is naturally idempotent and safe to poll from any number of
// concurrent cluster-agent replicas without coordination.
package clusteragentapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/db"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
	"github.com/scaleresearch/openresearch/controlplane/shared/obsmetrics"
)

// Store is the persistence interface the handler needs. Satisfied by *db.ClusterQueueStore.
type Store interface {
	ListDesiredWorkloads(ctx context.Context, clusterName string) ([]*domain.Experiment, error)
	UpsertJobReport(ctx context.Context, experimentID, clusterName, phase string, admittedAcceleratorType domain.AcceleratorType, seq int64, jobUID string) (applied bool, uidMismatch bool, err error)
	DeleteJobReportForCluster(ctx context.Context, experimentID, clusterName string) error
	BumpAttemptOnRecreate(ctx context.Context, experimentID string) (attempt int, bumped bool, err error)
	RecordHeartbeat(ctx context.Context, clusterName string, cpuAvailable, cpuTotal float64, hasCPUReport bool) error
	ListHeartbeats(ctx context.Context) ([]db.ClusterHeartbeat, error)
}

// Handler serves the cluster-agent-facing API.
type Handler struct {
	store  Store
	logger *zap.Logger
	// connectedWithin is how recent a heartbeat must be to count as "connected" — config-driven
	// (scheduler.cluster_unreachable_after_seconds), shared with queuebackend.Backend's live
	// CPU capacity staleness check so there is exactly one cluster-liveness threshold, not two
	// independently hardcoded ones.
	connectedWithin time.Duration
	// metricsDBURL is where live per-cluster accelerator capacity is written (see RecordClusterAcceleratorCapacity)
	// — capacity is metric data, so it lives in the metrics store, never duplicated into Postgres.
	metricsDBURL string
}

// NewHandler constructs a Handler. connectedWithin is how recent a cluster's heartbeat must be
// to count as reachable/connected.
func NewHandler(store Store, connectedWithin time.Duration, metricsDBURL string, logger *zap.Logger) *Handler {
	return &Handler{store: store, connectedWithin: connectedWithin, metricsDBURL: metricsDBURL, logger: logger}
}

// Routes registers all cluster-agent-facing routes onto r. Mounted at /internal/clusters.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/", h.ListClusters)
	r.Get("/{name}/desired-state", h.DesiredState)
	r.Post("/{name}/status", h.PushStatus)
}

// DesiredState handles GET /internal/clusters/{name}/desired-state — returns every
// experiment that should currently have a Job running in that cluster, full payload
// included (the agent has no database access, so everything needed to build the Job —
// accelerator type/count, image, env refs, priority tier — travels in this response). Also records a
// heartbeat for clusterName: this is the endpoint cluster-agent calls most frequently
// (its whole reconcile loop), so it doubles as the liveness signal the UI reads back via
// ListClusters — no separate ping needed.
//
// It also doubles as the live-capacity push channel: cluster-agent computes its own
// allocatable-minus-requested CPU cores every poll and attaches them as query params
// (cpu_available_cores, cpu_total_cores) — reusing this already-fast (~2s) poll instead of
// adding a second endpoint/cadence keeps capacity reporting on the same fast path as job
// status, matching every other push mechanism in this system.
func (h *Handler) DesiredState(w http.ResponseWriter, r *http.Request) {
	clusterName := chi.URLParam(r, "name")

	cpuAvail, availErr := strconv.ParseFloat(r.URL.Query().Get("cpu_available_cores"), 64)
	cpuTotal, totalErr := strconv.ParseFloat(r.URL.Query().Get("cpu_total_cores"), 64)
	hasCPUReport := availErr == nil && totalErr == nil

	if err := h.store.RecordHeartbeat(r.Context(), clusterName, cpuAvail, cpuTotal, hasCPUReport); err != nil {
		h.logger.Warn("clusteragentapi: record heartbeat", zap.String("cluster", clusterName), zap.Error(err))
	}

	var acceleratorAvail, acceleratorTotal map[string]int64
	if raw := r.URL.Query().Get("accelerator_available_by_flavor"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &acceleratorAvail)
	}
	if raw := r.URL.Query().Get("accelerator_total_by_flavor"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &acceleratorTotal)
	}
	if len(acceleratorAvail) > 0 {
		if err := metricsdb.RecordClusterAcceleratorCapacity(r.Context(), h.metricsDBURL, clusterName, acceleratorAvail, acceleratorTotal); err != nil {
			h.logger.Warn("clusteragentapi: record accelerator capacity", zap.String("cluster", clusterName), zap.Error(err))
		}
	}

	// RAM/ephemeral-storage — Class B (hard-cap, no billing) dimensions, same "metric data,
	// never duplicated into Postgres" treatment as accelerator above (see
	// SCHEDULING_GENERALIZATION_PLAN.md's Class B step 2).
	if ramAvail, availErr := strconv.ParseInt(r.URL.Query().Get("ram_available_bytes"), 10, 64); availErr == nil {
		if ramTotal, totalErr := strconv.ParseInt(r.URL.Query().Get("ram_total_bytes"), 10, 64); totalErr == nil {
			if err := metricsdb.RecordClusterRAMCapacity(r.Context(), h.metricsDBURL, clusterName, ramAvail, ramTotal); err != nil {
				h.logger.Warn("clusteragentapi: record RAM capacity", zap.String("cluster", clusterName), zap.Error(err))
			}
		}
	}
	if storageAvail, availErr := strconv.ParseInt(r.URL.Query().Get("storage_available_bytes"), 10, 64); availErr == nil {
		if storageTotal, totalErr := strconv.ParseInt(r.URL.Query().Get("storage_total_bytes"), 10, 64); totalErr == nil {
			if err := metricsdb.RecordClusterStorageCapacity(r.Context(), h.metricsDBURL, clusterName, storageAvail, storageTotal); err != nil {
				h.logger.Warn("clusteragentapi: record storage capacity", zap.String("cluster", clusterName), zap.Error(err))
			}
		}
	}

	exps, err := h.store.ListDesiredWorkloads(r.Context(), clusterName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"experiments": exps})
}

// clusterInfo is one row of GET /internal/clusters — read by the UI to show which registered
// clusters currently have a connected cluster-agent.
type clusterInfo struct {
	ClusterName string    `json:"cluster_name"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	Connected   bool      `json:"connected"`
}

// ListClusters handles GET /internal/clusters — every cluster that has ever polled, and
// whether its agent is connected right now (heartbeat within connectedWithin).
func (h *Handler) ListClusters(w http.ResponseWriter, r *http.Request) {
	heartbeats, err := h.store.ListHeartbeats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]clusterInfo, len(heartbeats))
	now := time.Now()
	for i, hb := range heartbeats {
		out[i] = clusterInfo{
			ClusterName: hb.ClusterName,
			LastSeenAt:  hb.LastSeenAt,
			Connected:   now.Sub(hb.LastSeenAt) <= h.connectedWithin,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": out})
}

// statusReport is one job-status push from a cluster-agent.
type statusReport struct {
	ExperimentID    string `json:"experiment_id"`
	Phase           string `json:"phase"` // pending | running | succeeded | failed | gone
	AdmittedAcceleratorType string `json:"admitted_accelerator_type,omitempty"`
	SequenceNumber  int64  `json:"sequence_number"`
	JobUID          string `json:"job_uid,omitempty"`
}

// PushStatus handles POST /internal/clusters/{name}/status.
// Body: {"reports": [statusReport, ...]}
func (h *Handler) PushStatus(w http.ResponseWriter, r *http.Request) {
	clusterName := chi.URLParam(r, "name")
	var body struct {
		Reports []statusReport `json:"reports"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// rejected lists reports the DB write did not durably apply to (error, stale/duplicate
	// sequence number, or an experiment not owned by this cluster) — the cluster-agent must
	// only advance its local tracked phase for reports absent from this list, retrying the
	// rest on its next push. A blanket 200 regardless of per-report outcome would let a
	// transient DB error or a stale sequence number permanently lose a status report, since
	// the agent treats any 2xx as a durable ack for the whole batch.
	rejected := []string{}
	for _, rep := range body.Reports {
		if rep.ExperimentID == "" || rep.Phase == "" {
			continue
		}
		applied, uidMismatch, err := h.store.UpsertJobReport(r.Context(), rep.ExperimentID, clusterName, rep.Phase, domain.AcceleratorType(rep.AdmittedAcceleratorType), rep.SequenceNumber, rep.JobUID)
		if err != nil {
			h.logger.Warn("clusteragentapi: upsert job report", zap.String("experiment_id", rep.ExperimentID), zap.Error(err))
			rejected = append(rejected, rep.ExperimentID)
			continue
		}
		if !applied {
			rejected = append(rejected, rep.ExperimentID)
			continue
		}
		if uidMismatch {
			// Flagged, not silently accepted or dropped: the phase update above still applied,
			// but this report's Job UID doesn't match the one first observed for this
			// experiment — a real ownership anomaly (see db.ClusterQueueStore.UpsertJobReport).
			h.logger.Warn("clusteragentapi: job UID mismatch — report may not correspond to the dispatched Job",
				zap.String("experiment_id", rep.ExperimentID),
				zap.String("cluster", clusterName),
				zap.String("reported_uid", rep.JobUID),
			)
			obsmetrics.JobUIDMismatchTotal.WithLabelValues(clusterName).Inc()
		}
		if rep.Phase == "gone" {
			if err := h.store.DeleteJobReportForCluster(r.Context(), rep.ExperimentID, clusterName); err != nil {
				h.logger.Warn("clusteragentapi: delete job report", zap.String("experiment_id", rep.ExperimentID), zap.Error(err))
			}
			// The Job vanished while this experiment may still be desired-running — bump its
			// attempt counter so that if cluster-agent recreates the Job, it's a distinct,
			// identifiable execution attempt rather than a silent same-identity re-run. No-op
			// (bumped=false) if the experiment already left the desired set, since then the gone
			// report is just expected post-terminal cleanup, not a recreation.
			attempt, bumped, err := h.store.BumpAttemptOnRecreate(r.Context(), rep.ExperimentID)
			if err != nil {
				h.logger.Warn("clusteragentapi: bump attempt", zap.String("experiment_id", rep.ExperimentID), zap.Error(err))
			} else if bumped {
				h.logger.Warn("clusteragentapi: job disappeared while still desired — recreation attempt bumped",
					zap.String("experiment_id", rep.ExperimentID),
					zap.String("cluster", clusterName),
					zap.Int("attempt", attempt),
				)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "rejected": rejected})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
