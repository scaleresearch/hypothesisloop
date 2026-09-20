// Package auth resolves who is calling. A caller presents one token in one of three forms: a
// JWT from the identity provider (a person), an hlk_ API key (a customer's client), or an hls_
// token derived from the signing secret (anything the platform launches).
package auth

import (
	"context"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

type Kind string

const (
	KindUser        Kind = "user"
	KindKey         Kind = "key"
	KindAgent       Kind = "agent"
	KindCoordinator Kind = "coordinator"
	KindJob         Kind = "job"
	KindRuntime     Kind = "runtime"
	KindLauncher    Kind = "launcher"
	KindStaff       Kind = "staff"
)

// Principal is the authenticated caller. Which fields are set depends on Kind: a user carries
// UserID, Name and the orgs it belongs to; a key its org and, when bound to one, its project; an
// agent, a coordinator and a job the platform experiment they act in, with its org and project;
// a runtime its cluster and owning org.
type Principal struct {
	Kind Kind

	UserID string
	// Name is the person's display name as the identity provider states it on this request.
	Name string
	// KeyID names the API key a key principal presented.
	KeyID string

	OrgIDs    []string
	ProjectID string

	PlatformExperimentID string
	// PlatformExperimentClosed is set on a coordinator token whose platform experiment has
	// closed: it still reads and files the summary, and does nothing else.
	PlatformExperimentClosed bool
	// RunOver is set on an agent, coordinator or job token whose run has ended while the token
	// still works: the platform experiment closed, or the agent, for a job the agent it belongs
	// to, was cut. Whatever would outlive the wind-down, such as a credential good for an hour,
	// is decided from it.
	RunOver      bool
	AgentID      string
	ExperimentID string
	ClusterName  string
	LauncherName string
}

// Scope is the part of the platform this principal may see.
func (p Principal) Scope() domain.TenantScope {
	if p.Kind == KindStaff {
		return domain.AllTenants
	}
	return domain.TenantScope{OrgIDs: p.OrgIDs, ProjectID: p.ProjectID, PlatformExperimentID: p.PlatformExperimentID}
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the principal the request was authenticated as. ok is false on a context
// no authenticated request produced.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
