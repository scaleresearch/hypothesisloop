package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// Handler wires the registry service to HTTP routes.
type Handler struct {
	svc    *Service
	logger *zap.Logger
}

// NewHandler returns a Handler for the given service.
func NewHandler(svc *Service, logger *zap.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

// Mount registers all registry routes onto r.
// The caller mounts this handler under /registry, so paths here are relative.
func (h *Handler) Mount(r chi.Router) {
	r.Post("/experiments", h.registerExperiment)
	r.Get("/experiments", h.listExperiments)
	r.Get("/experiments/{id}", h.getExperiment)
	r.Get("/experiments/{id}/lineage", h.getLineage)
	r.Get("/experiments/{id}/metrics", h.getMetrics)
	r.Post("/experiments/{id}/metrics", h.appendMetric)
	r.Patch("/experiments/{id}/status", h.updateStatus)
	r.Get("/platform-experiments/{id}/metrics-timeseries", h.getPlatformExperimentTimeseries)
	r.Post("/hypotheses", h.registerHypothesis)
	r.Get("/hypotheses", h.listHypotheses)
	r.Get("/hypotheses/{id}", h.getHypothesis)
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ---- handlers ----

// POST /registry/experiments
func (h *Handler) registerExperiment(w http.ResponseWriter, r *http.Request) {
	var exp domain.Experiment
	if err := decodeJSON(r, &exp); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := h.svc.Register(r.Context(), &exp); err != nil {
		h.logger.Error("register experiment", zap.Error(err))
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, &exp)
}

// GET /registry/experiments
func (h *Handler) listExperiments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := domain.ExperimentFilter{
		AgentID:              q.Get("agent"),
		PlatformExperimentID: q.Get("platform_experiment_id"),
		Status:               domain.ExperimentStatus(q.Get("status")),
	}
	if lim := q.Get("limit"); lim != "" {
		n, err := strconv.Atoi(lim)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		filter.Limit = n
	}
	exps, err := h.svc.List(r.Context(), filter)
	if err != nil {
		h.logger.Error("list experiments", zap.Error(err))
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, exps)
}

// GET /registry/experiments/{id}
func (h *Handler) getExperiment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	exp, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.logger.Error("get experiment", zap.String("id", id), zap.Error(err))
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, exp)
}

// GET /registry/experiments/{id}/lineage
func (h *Handler) getLineage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	chain, err := h.svc.GetLineage(r.Context(), id)
	if err != nil {
		h.logger.Error("get lineage", zap.String("id", id), zap.Error(err))
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, chain)
}

// GET /registry/experiments/{id}/metrics
func (h *Handler) getMetrics(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ts, err := h.svc.GetTimeseries(r.Context(), id)
	if err != nil {
		h.logger.Error("get timeseries", zap.String("id", id), zap.Error(err))
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

// GET /registry/platform-experiments/{id}/metrics-timeseries?metric_name=val_accuracy&lookback_hours=6
// Returns the full metric history for every job in the platform experiment — one series per
// job — so a dashboard can plot competing agents' metrics over time, not just their latest value.
func (h *Handler) getPlatformExperimentTimeseries(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	q := r.URL.Query()
	metricName := q.Get("metric_name")
	if metricName == "" {
		writeError(w, http.StatusBadRequest, "metric_name is required")
		return
	}
	lookback := 24 * time.Hour
	if lh := q.Get("lookback_hours"); lh != "" {
		if n, err := strconv.ParseFloat(lh, 64); err == nil && n > 0 {
			lookback = time.Duration(n * float64(time.Hour))
		}
	}
	series, err := h.svc.GetPlatformExperimentTimeseries(r.Context(), id, metricName, lookback)
	if err != nil {
		h.logger.Error("get platform experiment timeseries", zap.String("id", id), zap.Error(err))
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": series})
}

// POST /registry/experiments/{id}/metrics
func (h *Handler) appendMetric(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		MetricName       string  `json:"metric_name"`
		FractionComplete float64 `json:"fraction_complete"`
		MetricValue      float64 `json:"metric_value"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.MetricName == "" {
		body.MetricName = "default"
	}
	// RecordMetric already writes the sample straight to GreptimeDB (experiment_metric_value) —
	// that write is itself the observation. Nothing to additionally notify: silence detection
	// and decline detection both query GreptimeDB directly (see controller.checkSilence,
	// checkMetricDecline) rather than being told about pushes through a side channel.
	if err := h.svc.RecordMetric(r.Context(), id, body.MetricName, body.FractionComplete, body.MetricValue); err != nil {
		h.logger.Error("record metric", zap.String("id", id), zap.Error(err))
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// hypothesisResponse wraps a Hypothesis with whether registration found a pre-existing
// equivalent row, so agent clients can tell "you registered a new one" from "here's the one
// that already existed" without a second lookup.
type hypothesisResponse struct {
	*domain.Hypothesis
	AlreadyExisted bool `json:"already_existed"`
}

// POST /registry/hypotheses
// Body: {"agent_id": "...", "text": "..."}. Idempotent: registering text equivalent (modulo
// case/whitespace) to an already-registered hypothesis returns that existing row instead of
// creating a duplicate — this is the real uniqueness check agents should use to validate a
// hypothesis before submitting an experiment against it.
func (h *Handler) registerHypothesis(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AgentID string `json:"agent_id"`
		Text    string `json:"text"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.AgentID == "" {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	if body.Text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	hyp, existed, err := h.svc.RegisterHypothesis(r.Context(), body.AgentID, body.Text)
	if err != nil {
		h.logger.Error("register hypothesis", zap.Error(err))
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	writeJSON(w, status, hypothesisResponse{Hypothesis: hyp, AlreadyExisted: existed})
}

// GET /registry/hypotheses
func (h *Handler) listHypotheses(w http.ResponseWriter, r *http.Request) {
	hs, err := h.svc.ListHypotheses(r.Context())
	if err != nil {
		h.logger.Error("list hypotheses", zap.Error(err))
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, hs)
}

// hypothesisWithJobs bundles a hypothesis with the experiments (jobs) submitted against it,
// so the UI and agents can see the full picture — how many jobs already tested this
// hypothesis, and what came of them — from a single request.
type hypothesisWithJobs struct {
	*domain.Hypothesis
	Jobs []*domain.Experiment `json:"jobs"`
}

// GET /registry/hypotheses/{id}
func (h *Handler) getHypothesis(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	hyp, err := h.svc.GetHypothesis(r.Context(), id)
	if err != nil {
		h.logger.Error("get hypothesis", zap.String("id", id), zap.Error(err))
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hyp == nil {
		writeError(w, http.StatusNotFound, "hypothesis not found")
		return
	}
	jobs, err := h.svc.List(r.Context(), domain.ExperimentFilter{HypothesisID: id})
	if err != nil {
		h.logger.Error("list jobs for hypothesis", zap.String("id", id), zap.Error(err))
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, hypothesisWithJobs{Hypothesis: hyp, Jobs: jobs})
}

// PATCH /registry/experiments/{id}/status
func (h *Handler) updateStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		Status domain.ExperimentStatus `json:"status"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Status == "" {
		writeError(w, http.StatusBadRequest, "status is required")
		return
	}
	if err := h.svc.UpdateStatus(r.Context(), id, body.Status); err != nil {
		h.logger.Error("update status", zap.String("id", id), zap.Error(err))
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	exp, err := h.svc.Get(context.Background(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, exp)
}
