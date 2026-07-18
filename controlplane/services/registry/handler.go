package registry

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
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
