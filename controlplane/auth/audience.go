package auth

// Audience classifies who is permitted to call a registered operation, so a per-audience
// /explore digest can never leak an operation that audience must not use. Required on every
// registration — there is no default, so an operation cannot be added without classifying it.
type Audience string

const (
	// AudienceAgent: research agents and hl, their client — it must contain nothing an agent
	// shouldn't call. Admits everyone AudienceCoordinator does, and an agent token.
	AudienceAgent Audience = "agent"
	// AudienceJob: AudienceAgent plus a job token. The one thing a job may say to the platform
	// is a metric, so exactly one operation carries this audience.
	AudienceJob Audience = "job"
	// AudienceCoordinator: the coordinator/operator/dashboard — experiment lifecycle admin
	// (create/start/close/summary) and other operator-only actions.
	// Admits a signed-in user, an API key, a coordinator token and staff.
	AudienceCoordinator Audience = "coordinator"
	// AudienceInternal: cluster-agent binaries only (the reconcile/status-push contract) —
	// never fetched by a research agent or the coordinator. Admits a runtime token on the
	// cluster its {name} path parameter names, and staff.
	AudienceInternal Audience = "internal"
	// AudienceStaff: the platform's own operators and nobody else. The audience is the whole
	// rule of such an operation: a decision the platform makes across orgs.
	AudienceStaff Audience = "staff"
)

// Admits reports whether the audience lets this principal in. clusterName is the operation's
// {name} path parameter, empty on an operation that has none: the cluster of an AudienceInternal
// operation.
func (a Audience) Admits(p Principal, clusterName string) bool {
	switch p.Kind {
	case KindStaff:
		return true
	case KindUser, KindKey, KindCoordinator:
		return a == AudienceAgent || a == AudienceJob || a == AudienceCoordinator
	case KindAgent:
		return a == AudienceAgent || a == AudienceJob
	case KindJob:
		return a == AudienceJob
	case KindRuntime:
		return a == AudienceInternal && clusterName != "" && clusterName == p.ClusterName
	default:
		return false
	}
}
