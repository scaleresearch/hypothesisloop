package scheduler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/go-chi/chi/v5"
)

// Handler wires the Scheduler Service to HTTP endpoints.
type Handler struct {
	svc *Service
}

// NewHandler creates a Handler for the given Service.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Routes registers all scheduler HTTP routes onto r.
// The caller mounts this handler under /experiments in the gateway, so paths
// here are relative to that prefix.
func (h *Handler) Routes(r chi.Router) {
	r.Post("/", h.SubmitExperiment)
	r.Get("/", h.ListExperiments)
	r.Get("/{id}", h.GetExperiment)
	r.Post("/{id}/admit", h.AdmitExperiment)
	r.Post("/{id}/cancel", h.CancelExperiment)
	r.Post("/{id}/summary", h.WriteExperimentSummary)
	r.Post("/reprioritize", h.Reprioritize)
}

// submitRequest is the wire shape for POST /experiments: the platform's own JobSpec DSL
// (the sole source of truth for how the workload runs — never a raw execution-engine
// manifest) alongside the metadata sidecar for research/bookkeeping concepts. See
// settings/examples/{job,experiment}.yaml.
type submitRequest struct {
	ID       string                `json:"id"`
	Metadata domain.ExperimentMeta `json:"metadata"`
	Job      domain.JobSpec        `json:"job"`
}

// SubmitExperiment handles POST /experiments.
// It decodes {id, metadata, job}, builds the domain.Experiment (AcceleratorType/AcceleratorCount are
// derived from the job spec's own AcceleratorType/TotalAccelerators — never restated by the caller), calls
// Submit, and returns the updated experiment on success or an admission error on rejection.
func (h *Handler) SubmitExperiment(w http.ResponseWriter, r *http.Request) {
	var req submitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}

	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "experiment id is required")
		return
	}

	meta := req.Metadata
	exp := domain.Experiment{
		ID:       req.ID,
		ParentID:               meta.ParentID,
		AgentID:                meta.AgentID,
		PlatformExperimentID:   meta.PlatformExperimentID,
		ProjectID:              meta.ProjectID,
		CodeRef:                meta.CodeRef,
		ConfigHash:             meta.ConfigHash,
		DataRef:                meta.DataRef,
		Job:                    req.Job,
		HypothesisID:           meta.HypothesisID,
		Hypothesis:             meta.Hypothesis,
		Objective:              meta.Objective,
		Theory:                 meta.Theory,
		AcceleratorType:                req.Job.AcceleratorType,
		AcceleratorCount:               req.Job.TotalAccelerators(),
		EstimatedDurationHours: meta.EstimatedDurationHours,
		CapacityTier:           meta.CapacityTier,
	}

	if err := h.svc.Submit(r.Context(), &exp); err != nil {
		var admErr *AdmissionError
		if errors.As(err, &admErr) {
			status := admissionHTTPStatus(admErr.Reason)
			writeJSON(w, status, map[string]string{
				"reason":  admErr.Reason,
				"message": admErr.Message,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, &exp)
}

// ListExperiments handles GET /experiments.
// Supports query parameters: agent, status.
func (h *Handler) ListExperiments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := domain.ExperimentFilter{
		AgentID: q.Get("agent"),
		Status:  domain.ExperimentStatus(q.Get("status")),
	}

	exps, err := h.svc.store.ListExperiments(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, exps)
}

// GetExperiment handles GET /experiments/{id}.
func (h *Handler) GetExperiment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	exp, err := h.svc.store.GetExperiment(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exp == nil {
		writeError(w, http.StatusNotFound, "experiment not found")
		return
	}
	writeJSON(w, http.StatusOK, exp)
}

// admitRequest is the wire shape for POST /experiments/{id}/admit: the operator must name an
// explicit target cluster — this endpoint never picks one on the caller's behalf (see
// AdmitExperiment's doc comment for why).
type admitRequest struct {
	ClusterName string `json:"cluster_name"`
}

// AdmitExperiment handles POST /experiments/{id}/admit (operator endpoint).
// It admits the identified QUEUED experiment directly onto an operator-specified cluster,
// bypassing queue ordering — but not capacity accounting: it requires an explicit valid target
// cluster and goes through the same MarkSubmitted capacity-claiming transition normal admission
// uses (see loop_preempt.go's submitJob), after confirming that cluster actually has room. A
// bare status flip with no cluster assignment would create an experiment no cluster agent's
// desired-state query could ever fetch (queries are scoped WHERE cluster_name = $1) — it would
// report success and then silently never run.
func (h *Handler) AdmitExperiment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req admitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}
	if req.ClusterName == "" {
		writeError(w, http.StatusBadRequest, "cluster_name is required")
		return
	}
	valid := false
	for _, c := range h.svc.workload.ClusterNames() {
		if c == req.ClusterName {
			valid = true
			break
		}
	}
	if !valid {
		writeError(w, http.StatusBadRequest, "cluster_name is not a configured cluster")
		return
	}

	exp, err := h.svc.store.GetExperiment(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exp == nil {
		writeError(w, http.StatusNotFound, "experiment not found")
		return
	}
	if exp.Status != domain.StatusQueued {
		writeError(w, http.StatusConflict, "only QUEUED experiments can be admitted")
		return
	}

	// Compute the exact same capacity view tick() uses, scoped to the requested cluster, so an
	// operator override still respects physical capacity instead of admitting blindly.
	// GetFlavorCapacity already reflects every SUBMITTED/ADMITTED/RUNNING pod that actually
	// exists on a node (see loop_tick.go step 2) — subtracting occupied-status experiments again
	// here would double-count them. The only additional subtraction needed is pending
	// reservations (loop_tick.go step 2b), which by construction cover only the not-yet-
	// scheduled gap and are never yet reflected in the live number.
	gAvail, _, err := h.svc.workload.GetFlavorCapacity(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	fp := exp.Footprint()
	avail, ok := gAvail[req.ClusterName]
	if !ok {
		writeError(w, http.StatusBadRequest, "cluster_name is not a configured cluster")
		return
	}
	pendingByCluster, err := h.svc.store.ListPendingReservationsByCluster(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	subtractFootprint(avail, pendingByCluster[req.ClusterName])
	if !domain.Fits(avail, fp) {
		writeError(w, http.StatusUnprocessableEntity, "insufficient capacity on requested cluster")
		return
	}

	if err := h.svc.store.MarkSubmitted(r.Context(), id, req.ClusterName); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	exp.Status = domain.StatusSubmitted
	exp.ClusterName = req.ClusterName
	exp.UpdatedAt = time.Now().UTC()
	// Durably claim this capacity the same way normal admission does (see
	// pending_capacity_reservations' schema comment). Unlike the previous best-effort version,
	// a failure here now rolls the experiment back to QUEUED instead of proceeding — the fallback
	// occupancy accounting this comment used to point to no longer exists (loop_tick.go step 2
	// was removed once GetFlavorCapacity became fully live), so a silently dropped reservation
	// would leave this job's capacity claim completely untracked, reopening the exact
	// double-admission race pending reservations exist to close.
	if err := h.svc.store.UpsertPendingReservation(r.Context(), id, req.ClusterName, fp); err != nil {
		_ = h.svc.store.UpdateExperimentStatus(r.Context(), id, domain.StatusQueued)
		writeError(w, http.StatusInternalServerError, "reservation failed: "+err.Error())
		return
	}

	if err := h.svc.workload.CreateWorkload(r.Context(), exp); err != nil {
		// Roll back on workload creation failure.
		_ = h.svc.store.UpdateExperimentStatus(r.Context(), id, domain.StatusQueued)
		_ = h.svc.store.DeletePendingReservation(r.Context(), id)
		writeError(w, http.StatusInternalServerError, "workload creation failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, exp)
}

// CancelExperiment handles POST /experiments/{id}/cancel.
// Agents and operators can cancel QUEUED or RUNNING experiments; credits are refunded.
func (h *Handler) CancelExperiment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.svc.CancelExperiment(r.Context(), id); err != nil {
		var admErr *AdmissionError
		if errors.As(err, &admErr) {
			switch admErr.Reason {
			case "not_found":
				writeError(w, http.StatusNotFound, admErr.Message)
			case "invalid_state":
				writeError(w, http.StatusConflict, admErr.Message)
			default:
				writeError(w, http.StatusUnprocessableEntity, admErr.Message)
			}
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// WriteExperimentSummary handles POST /experiments/{id}/summary.
// Agents call this after their job completes or is cancelled to record what they learned.
// Body: {"summary": "..."}
func (h *Handler) WriteExperimentSummary(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		Summary string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Summary == "" {
		writeError(w, http.StatusBadRequest, "summary is required")
		return
	}
	if err := h.svc.WriteExperimentSummary(r.Context(), id, body.Summary); err != nil {
		var admErr *AdmissionError
		if errors.As(err, &admErr) {
			switch admErr.Reason {
			case "not_found":
				writeError(w, http.StatusNotFound, admErr.Message)
			case "invalid_state":
				writeError(w, http.StatusConflict, admErr.Message)
			default:
				writeError(w, http.StatusUnprocessableEntity, admErr.Message)
			}
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Reprioritize handles POST /scheduler/reprioritize.
// It triggers an immediate re-prioritization pass over all QUEUED experiments.
func (h *Handler) Reprioritize(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.RePrioritize(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// admissionHTTPStatus maps an admission reason to the appropriate HTTP status code.
func admissionHTTPStatus(reason string) int {
	switch reason {
	case ReasonInsufficientCredits:
		return http.StatusPaymentRequired
	case ReasonDuplicate:
		return http.StatusConflict
	case ReasonMalformed:
		return http.StatusBadRequest
	case ReasonSummaryRequired:
		return http.StatusForbidden
	case ReasonRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusUnprocessableEntity
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
