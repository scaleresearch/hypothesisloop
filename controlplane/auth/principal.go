// Package auth resolves who is calling. A caller presents one token in one of three forms: a
// JWT from the identity provider (a person), an hlk_ API key (a customer's client), or an hls_
// token derived from the signing secret (anything the platform launches).
package auth

import (
	"context"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

type Kind string

const (
	KindUser        Kind = "user"
	KindKey         Kind = "key"
	KindAgent       Kind = "agent"
	KindCoordinator Kind = "coordinator"
	KindJob         Kind = "job"
	KindRuntime     Kind = "runtime"
	// KindBuild reads, through the git proxy, the repository of one environment build while it
	// is assigned and building, and reaches nothing else.
	KindBuild Kind = "build"
	KindStaff Kind = "staff"
)

// Principal is the authenticated caller. Which fields are set depends on Kind: a user carries
// UserID, Name and the orgs it belongs to; a key its org and, when bound to one, its project; an
// agent, a coordinator and a job the platform experiment they act in, with its org and project;
// a runtime its cluster and owning org.
type Principal struct {
	Kind Kind

	UserID string
	// Name is the person's display name, the one their identity provider last stated.
	Name string
	// KeyID names the API key a key principal presented.
	KeyID string

	OrgIDs    []string
	ProjectID string

	PlatformExperimentID string
	// PriorRuns are the prior runs of an agent's or a coordinator's platform experiment
	// (domain.Tenancy.PriorRuns), which it reads as its own (readsPriorRun).
	PriorRuns []string
	// PlatformExperimentClosed is set on a coordinator token whose platform experiment has
	// closed: until its wind-down is over it reads and files the summary, and does nothing else.
	PlatformExperimentClosed bool
	// RunOver is set on an agent, coordinator or job token whose run has ended while the token
	// still works: the platform experiment closed, the agent, for a job the agent it belongs
	// to, was cut, the agent finished, or it is a planner whose answer is stored. Whatever would outlive the
	// wind-down, such as a credential good for an hour, is decided from it.
	RunOver      bool
	AgentID      string
	ExperimentID string
	ClusterName  string
	// Build is the environment build a build token reads the repository of.
	Build BuildRef
}

// BuildRef names one attempt of an environment build on the cluster it was assigned to.
type BuildRef struct {
	Cluster string
	Key     string
	Attempt int
}

// HoldsAccount reports a principal of a person or an org: a user, an API key or staff. A token the
// platform derived for an agent, a coordinator or a job holds none.
func (p Principal) HoldsAccount() bool {
	return p.Kind == KindUser || p.Kind == KindKey || p.Kind == KindStaff
}

// Scope is the part of the platform this principal may see.
func (p Principal) Scope() domain.TenantScope {
	if p.Kind == KindStaff {
		return domain.AllTenants
	}
	return domain.TenantScope{OrgIDs: p.OrgIDs, ProjectID: p.ProjectID, PlatformExperimentID: p.PlatformExperimentID}
}

// Actor is whose credential this is, as a record of what it did names it. It is the only place
// an actor is made, so no record names one a request merely claimed. Staff who signed in are the
// person they are; a job is the agent it runs for. A runtime authors no record and has none.
func (p Principal) Actor() domain.Actor {
	switch {
	case p.UserID != "":
		return domain.Actor{Kind: domain.ActorUser, ID: p.UserID, Name: p.Name}
	case p.Kind == KindKey:
		return domain.Actor{Kind: domain.ActorKey, ID: p.KeyID}
	case p.Kind == KindAgent, p.Kind == KindJob:
		return domain.Actor{Kind: domain.ActorAgent, ID: p.AgentID}
	case p.Kind == KindCoordinator:
		return domain.Actor{Kind: domain.ActorCoordinator, ID: p.PlatformExperimentID}
	case p.Kind == KindStaff:
		return domain.Actor{Kind: domain.ActorStaff}
	default:
		return domain.Actor{}
	}
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
