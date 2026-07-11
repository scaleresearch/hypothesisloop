package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// Gateway is the single HTTP entry point that mounts quota, scheduler, and
// registry handlers behind a shared set of middlewares.
type Gateway struct {
	quotaH     http.Handler
	schedulerH http.Handler
	registryH  http.Handler
}

// NewGateway creates a Gateway composing the three service handlers.
func NewGateway(quotaH, schedulerH, registryH http.Handler) *Gateway {
	return &Gateway{
		quotaH:     quotaH,
		schedulerH: schedulerH,
		registryH:  registryH,
	}
}

// Handler builds and returns the chi router with all middleware applied.
func (g *Gateway) Handler(logger *zap.Logger) http.Handler {
	r := chi.NewRouter()

	// Global middleware stack (applied in declaration order).
	r.Use(RecoveryMiddleware(logger))
	r.Use(LoggingMiddleware(logger))
	r.Use(CORSMiddleware)

	// Health probe — always respond 200 OK.
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Mount sub-handlers.  Strip the prefix so each handler only sees paths
	// relative to its own mount point.
	r.Mount("/quota", g.quotaH)
	r.Mount("/experiments", g.schedulerH)
	r.Mount("/registry", g.registryH)

	return r
}
