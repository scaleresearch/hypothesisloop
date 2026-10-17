package quota

import (
	"net/http"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
)

// reasonHostingNotConfigured is why hosting is unavailable on a deployment with no sandbox
// provider in its settings.
const reasonHostingNotConfigured = "hosting_not_configured"

// Hosting is whether the platform can host an agent: GET /hosting, and what every request that
// hosts one is refused with while it cannot.
type Hosting struct {
	Available   bool                 `json:"available"`
	Unavailable *domain.StatusReason `json:"unavailable,omitempty" doc:"Why it cannot, while available is false: reason hosting_not_configured."`
}

// hostingOf is Hosting on a deployment that has a sandbox provider or not.
func hostingOf(configured bool) Hosting {
	if configured {
		return Hosting{Available: true}
	}
	return Hosting{Unavailable: &domain.StatusReason{
		Reason:         reasonHostingNotConfigured,
		Message:        "this deployment has no sandbox provider to run hosted machines on",
		Summary:        "Hosted agents are not available on this platform.",
		OperatorDetail: "The settings have no sandbox section (on a dev stack: make dev-hosted).",
	}}
}

// requireHosting refuses to host anything on a deployment that cannot: nothing would ever start it.
func requireHosting(configured bool) error {
	why := hostingOf(configured).Unavailable
	if why == nil {
		return nil
	}
	refusal := api.Refuse(http.StatusServiceUnavailable, why.Reason, why.Reason+": "+why.Message)
	refusal.Summary, refusal.OperatorDetail = why.Summary, why.OperatorDetail
	return refusal
}
