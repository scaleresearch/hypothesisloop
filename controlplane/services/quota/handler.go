package quota

import (
	"go.uber.org/zap"
)

// Handler wires the quota Service to HTTP endpoints. Routes are registered via
// RegisterHuma (see huma_api.go) — the transport layer is Huma, so the OpenAPI
// spec, validation and /explore digest are all derived from the same operation
// definitions rather than hand-maintained.
type Handler struct {
	svc    *Service
	logger *zap.Logger
}

// NewHandler constructs a Handler.
func NewHandler(svc *Service, logger *zap.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}
