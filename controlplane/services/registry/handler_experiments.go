package registry

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

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
		if errors.Is(err, ErrInvalidMetric) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		h.logger.Error("record metric", zap.String("id", id), zap.Error(err))
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
