package auth

import (
	"context"
	"net/http"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Placement is what Access reads to place a resource in an org. *db.AccountsStore and
// *db.ClustersStore satisfy it together (StoreDirectory).
type Placement interface {
	PlatformExperimentTenancy(ctx context.Context, platformExperimentID string) (*domain.Tenancy, bool, error)
	ExperimentTenancy(ctx context.Context, experimentID string) (*domain.Tenancy, bool, error)
	DonationTenancy(ctx context.Context, donationID string) (*domain.Tenancy, error)
	AgentOrg(ctx context.Context, agentID string) (orgID string, found bool, err error)
	ClusterOrg(ctx context.Context, name string) (orgID string, found bool, err error)
	GetProject(ctx context.Context, id string) (*domain.Project, error)
	PlatformExperimentIDs(ctx context.Context, scope domain.TenantScope) ([]string, error)
}

// Access answers whether the request's principal may touch a resource. The rule is the same for
// every resource: place it in a platform experiment, compare that platform experiment's org and
// project to the principal's, and for an agent or a coordinator the platform experiment itself.
// A resource outside the principal's scope is answered as not found. A resource that does not
// exist passes, so the handler answers for it exactly as it would for any caller.
type Access struct {
	placement Placement
}

func NewAccess(placement Placement) *Access {
	return &Access{placement: placement}
}

func notFound(what string) *Error {
	return &Error{status: http.StatusNotFound, Message: what + " not found"}
}

func forbidden(message string) *Error {
	return &Error{status: http.StatusForbidden, Message: message}
}

func principal(ctx context.Context) (Principal, error) {
	p, ok := FromContext(ctx)
	if !ok {
		return Principal{}, unauthenticated("the request carries no principal")
	}
	return p, nil
}

// PlatformExperiment admits the principal to one platform experiment.
func (a *Access) PlatformExperiment(ctx context.Context, platformExperimentID string) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	if p.Scope().Unrestricted() {
		return nil
	}
	tenancy, _, err := a.placement.PlatformExperimentTenancy(ctx, platformExperimentID)
	if err != nil {
		return err
	}
	if tenancy != nil && !p.Scope().Admits(*tenancy) {
		return notFound("platform experiment")
	}
	return nil
}

// Experiment admits the principal to one job. A job token reaches its own job and no other.
func (a *Access) Experiment(ctx context.Context, experimentID string) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	if p.Kind == KindJob && p.ExperimentID != experimentID {
		return forbidden("a job token reaches its own job only")
	}
	if p.Scope().Unrestricted() {
		return nil
	}
	tenancy, _, err := a.placement.ExperimentTenancy(ctx, experimentID)
	if err != nil {
		return err
	}
	if tenancy != nil && !p.Scope().Admits(*tenancy) {
		return notFound("experiment")
	}
	return nil
}

// Donation admits the principal to one donation request.
func (a *Access) Donation(ctx context.Context, donationID string) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	if p.Scope().Unrestricted() {
		return nil
	}
	tenancy, err := a.placement.DonationTenancy(ctx, donationID)
	if err != nil {
		return err
	}
	if tenancy != nil && !p.Scope().Admits(*tenancy) {
		return notFound("donation request")
	}
	return nil
}

// Self refuses an agent token that names any agent but its own.
func (a *Access) Self(ctx context.Context, agentID string) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	if p.Kind == KindAgent && p.AgentID != agentID {
		return forbidden("an agent token may name only its own agent, " + p.AgentID)
	}
	return nil
}

// Agent admits the principal to name agentID in a request: an agent may name only itself, anyone
// else any agent of an org in their scope.
func (a *Access) Agent(ctx context.Context, agentID string) error {
	if err := a.Self(ctx, agentID); err != nil {
		return err
	}
	p, _ := FromContext(ctx)
	if p.Kind == KindAgent || p.Kind == KindStaff {
		return nil
	}
	orgID, found, err := a.placement.AgentOrg(ctx, agentID)
	if err != nil {
		return err
	}
	if found && !p.Scope().AdmitsOrg(orgID) {
		return notFound("agent")
	}
	return nil
}

// Cluster admits the principal to one cluster's settings: its owning org, or staff.
func (a *Access) Cluster(ctx context.Context, name string) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	if p.Kind == KindStaff {
		return nil
	}
	orgID, found, err := a.placement.ClusterOrg(ctx, name)
	if err != nil {
		return err
	}
	if !found || !p.Scope().AdmitsOrg(orgID) {
		return notFound("cluster")
	}
	return nil
}

// Project admits the principal to create within one project.
func (a *Access) Project(ctx context.Context, projectID string) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	project, err := a.placement.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	if project == nil || !p.Scope().Admits(domain.Tenancy{ProjectID: project.ID, OrgID: project.OrgID, PlatformExperimentID: p.PlatformExperimentID}) {
		return notFound("project")
	}
	return nil
}

// Org resolves the org a new org-level resource belongs to. A principal bound to one org uses
// it; one that spans several, or all, must name it.
func (a *Access) Org(ctx context.Context, requested string) (string, error) {
	p, err := principal(ctx)
	if err != nil {
		return "", err
	}
	if requested == "" {
		if p.Kind == KindStaff || len(p.OrgIDs) != 1 {
			return "", &Error{status: http.StatusBadRequest, Message: "org_id is required: this token is not bound to a single org"}
		}
		return p.OrgIDs[0], nil
	}
	if !p.Scope().AdmitsOrg(requested) {
		return "", notFound("org")
	}
	return requested, nil
}

// ListScope is the principal's scope for a listing, narrowed to one project when asked.
func (a *Access) ListScope(ctx context.Context, projectID string) (domain.TenantScope, error) {
	p, err := principal(ctx)
	if err != nil {
		return domain.TenantScope{}, err
	}
	scope := p.Scope()
	if projectID == "" {
		return scope, nil
	}
	if scope.ProjectID != "" && scope.ProjectID != projectID {
		return domain.TenantScope{}, notFound("project")
	}
	scope.ProjectID = projectID
	return scope, nil
}

// PlatformExperimentIDs lists the platform experiments a listing may draw from, for a store that
// cannot join to them. restricted is false when the principal may see them all.
func (a *Access) PlatformExperimentIDs(ctx context.Context, projectID string) (ids []string, restricted bool, err error) {
	scope, err := a.ListScope(ctx, projectID)
	if err != nil {
		return nil, false, err
	}
	if scope.Unrestricted() {
		return nil, false, nil
	}
	ids, err = a.placement.PlatformExperimentIDs(ctx, scope)
	return ids, true, err
}

// Author is the name written beside what a person contributes: the identity provider's name for
// a signed-in user, and what the request states for every other caller.
func Author(ctx context.Context, stated string) string {
	if p, ok := FromContext(ctx); ok && p.Kind == KindUser {
		return p.Name
	}
	return stated
}
