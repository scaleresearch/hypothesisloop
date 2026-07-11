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
// It decodes {id, metadata, job}, builds the domain.Experiment (GPUType/GPUCount are
// derived from the job spec's own GPUType/TotalGPUs — never restated by the caller), calls
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
		ID:                     req.ID,
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
		GPUType:                req.Job.GPUType,
		GPUCount:               req.Job.TotalGPUs(),
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

// AdmitExperiment handles POST /experiments/{id}/admit (operator endpoint).
// It directly admits the identified experiment, bypassing queue ordering.
func (h *Handler) AdmitExperiment(w http.ResponseWriter, r *http.Request) {
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
	if exp.Status != domain.StatusQueued {
		writeError(w, http.StatusConflict, "only QUEUED experiments can be admitted")
		return
	}

	if err := h.svc.store.UpdateExperimentStatus(r.Context(), id, domain.StatusAdmitted); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	exp.Status = domain.StatusAdmitted
	exp.UpdatedAt = time.Now().UTC()

	if err := h.svc.workload.CreateWorkload(r.Context(), exp); err != nil {
		// Roll back on workload creation failure.
		_ = h.svc.store.UpdateExperimentStatus(r.Context(), id, domain.StatusQueued)
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
