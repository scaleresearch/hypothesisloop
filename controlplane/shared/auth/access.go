package auth

import (
	"context"
	"net/http"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Placement is what Access reads to place a resource in an org. *db.AccountsStore and
// *db.ClustersStore satisfy it together (StoreDirectory).
type Placement interface {
	PlatformExperimentTenancy(ctx context.Context, platformExperimentID string) (tenancy *domain.Tenancy, closedAt *time.Time, err error)
	ExperimentTenancy(ctx context.Context, experimentID string) (*domain.Tenancy, bool, error)
	DonationTenancy(ctx context.Context, donationID string) (*domain.Tenancy, error)
	AgentOrg(ctx context.Context, agentID string) (orgID string, found bool, err error)
	GetCluster(ctx context.Context, name string) (*domain.Cluster, error)
	ClusterByReportedID(ctx context.Context, reportedID string) (*domain.Cluster, error)
	GetAPIKey(ctx context.Context, id string) (*domain.APIKey, error)
	GetOrg(ctx context.Context, id string) (*domain.Org, error)
	OrgRole(ctx context.Context, orgID, userID string) (role domain.OrgRole, found bool, err error)
	GetProject(ctx context.Context, id string) (*domain.Project, error)
	PlatformExperimentIDs(ctx context.Context, scope domain.TenantScope) ([]string, error)
}

// Access answers whether the request's principal may touch a resource. The rule is the same for
// every resource: place it in a platform experiment, compare that platform experiment's org and
// project to the principal's, and for an agent or a coordinator the platform experiment itself.
// A resource outside the principal's scope and one that does not exist are answered alike, as
// not found, so the answer says nothing about another org. Only staff, who see every org, pass
// on an id with no row and get the handler's own answer for it.
type Access struct {
	placement Placement
}

func NewAccess(placement Placement) *Access {
	return &Access{placement: placement}
}

// NotFound is the refusal for a resource the caller cannot reach, whether or not it exists.
func NotFound(what string) *Error {
	return &Error{status: http.StatusNotFound, Message: what + " not found"}
}

func forbidden(message string) *Error {
	return &Error{status: http.StatusForbidden, Message: message}
}

func badRequest(message string) *Error {
	return &Error{status: http.StatusBadRequest, Message: message}
}

func principal(ctx context.Context) (Principal, error) {
	p, ok := FromContext(ctx)
	if !ok {
		return Principal{}, unauthenticated("the request carries no principal")
	}
	return p, nil
}

// validID refuses an id that could not name a row. Ids are pasted into object-store prefixes and
// queries, so one holding a separator or a wildcard must stop here, before anything is built
// from it.
func validID(what, id string) error {
	if !domain.IdentifierShaped(id) {
		return NotFound(what)
	}
	return nil
}

// PlatformExperiment admits the principal to one platform experiment.
func (a *Access) PlatformExperiment(ctx context.Context, platformExperimentID string) error {
	return a.within(ctx, "platform experiment", platformExperimentID)
}

// Within admits the principal to something that lives in a platform experiment and has no row
// of its own to place, naming it in the refusal so that it reads the same as an absent one.
func (a *Access) Within(ctx context.Context, what, platformExperimentID string) error {
	return a.within(ctx, what, platformExperimentID)
}

func (a *Access) within(ctx context.Context, what, platformExperimentID string) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	if validID("platform experiment", platformExperimentID) != nil {
		return NotFound(what)
	}
	if p.Scope().Unrestricted() {
		return nil
	}
	tenancy, _, err := a.placement.PlatformExperimentTenancy(ctx, platformExperimentID)
	if err != nil {
		return err
	}
	if tenancy == nil || !p.Scope().Admits(*tenancy) {
		return NotFound(what)
	}
	return nil
}

// Experiment admits the principal to one job. A job token reaches its own job and no other.
func (a *Access) Experiment(ctx context.Context, experimentID string) error {
	_, err := a.experiment(ctx, experimentID)
	return err
}

// OwnExperiment admits the principal to act on one job as its owner: an agent token only on a
// job of its own agent, everyone else as Experiment does.
func (a *Access) OwnExperiment(ctx context.Context, experimentID string) error {
	tenancy, err := a.experiment(ctx, experimentID)
	if err != nil {
		return err
	}
	return ownedBy(ctx, tenancy, "job")
}

func (a *Access) experiment(ctx context.Context, experimentID string) (*domain.Tenancy, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if p.Kind == KindJob && p.ExperimentID != experimentID {
		return nil, forbidden("a job token reaches its own job only")
	}
	if err := validID("experiment", experimentID); err != nil {
		return nil, err
	}
	tenancy, _, err := a.placement.ExperimentTenancy(ctx, experimentID)
	if err != nil {
		return nil, err
	}
	return admitted(p, tenancy, "experiment")
}

// Donation admits the principal to one donation request.
func (a *Access) Donation(ctx context.Context, donationID string) error {
	_, err := a.donation(ctx, donationID)
	return err
}

// OwnDonation admits the principal to act on a donation request as the agent that asked.
func (a *Access) OwnDonation(ctx context.Context, donationID string) error {
	tenancy, err := a.donation(ctx, donationID)
	if err != nil {
		return err
	}
	return ownedBy(ctx, tenancy, "donation request")
}

func (a *Access) donation(ctx context.Context, donationID string) (*domain.Tenancy, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := validID("donation request", donationID); err != nil {
		return nil, err
	}
	tenancy, err := a.placement.DonationTenancy(ctx, donationID)
	if err != nil {
		return nil, err
	}
	return admitted(p, tenancy, "donation request")
}

// admitted answers for a placed row. Staff pass on an absent one with a nil tenancy.
func admitted(p Principal, tenancy *domain.Tenancy, what string) (*domain.Tenancy, error) {
	if p.Scope().Unrestricted() {
		return tenancy, nil
	}
	if tenancy == nil || !p.Scope().Admits(*tenancy) {
		return nil, NotFound(what)
	}
	return tenancy, nil
}

func ownedBy(ctx context.Context, tenancy *domain.Tenancy, what string) error {
	p, _ := FromContext(ctx)
	if p.Kind == KindAgent && (tenancy == nil || tenancy.AgentID != p.AgentID) {
		return forbidden("this " + what + " belongs to another agent")
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

// Agent admits the principal to name a registered agent in a request: an agent only itself,
// and every caller but staff only an agent of an org in its scope.
func (a *Access) Agent(ctx context.Context, agentID string) error {
	if err := a.Self(ctx, agentID); err != nil {
		return err
	}
	if err := validID("agent", agentID); err != nil {
		return err
	}
	p, _ := FromContext(ctx)
	if p.Kind == KindStaff {
		return nil
	}
	orgID, found, err := a.placement.AgentOrg(ctx, agentID)
	if err != nil {
		return err
	}
	if !found || !p.Scope().AdmitsOrg(orgID) {
		return NotFound("agent")
	}
	return nil
}

// Staff admits only the platform's own operators, for an operation that acts across every org.
func (a *Access) Staff(ctx context.Context) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	if p.Kind != KindStaff {
		return forbidden("forbidden: only staff may do this")
	}
	return nil
}

// Project admits the principal to create a platform experiment within one project. A token
// bound to one platform experiment creates no other.
func (a *Access) Project(ctx context.Context, projectID string) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	if p.PlatformExperimentID != "" {
		return forbidden("a " + string(p.Kind) + " token acts within its own platform experiment and creates no other")
	}
	project, err := a.placement.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	if project == nil || !p.Scope().AdmitsOrg(project.OrgID) || (p.ProjectID != "" && p.ProjectID != project.ID) {
		return NotFound("project")
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
		return "", NotFound("org")
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
		return domain.TenantScope{}, NotFound("project")
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

// Origin is who a contribution to the hypothesis graph is by: an agent, or a person by name.
// UserID is the signed-in person behind Author, since the name is their own claim.
type Origin struct {
	AgentID string
	Author  string
	UserID  string
}

// Origin resolves who a hypothesis, link or comment is by from what the request states.
//
// An agent token writes as its own agent and as nobody else. Any other caller that names an
// agent writes for that agent, which must be one it reaches. With no agent named the write is a
// person's: a signed-in user is named by the identity provider, whatever the request states, and
// every other caller by the author it states.
func (a *Access) Origin(ctx context.Context, agentID, author string) (Origin, error) {
	p, err := principal(ctx)
	if err != nil {
		return Origin{}, err
	}
	if p.Kind == KindAgent {
		if agentID != p.AgentID {
			return Origin{}, forbidden("an agent token writes as its own agent: agent_id must be " + p.AgentID)
		}
		if author != "" {
			return Origin{}, forbidden("an agent token cannot write as a person: leave author empty")
		}
	}
	if agentID != "" {
		if err := a.Agent(ctx, agentID); err != nil {
			return Origin{}, err
		}
		if p.UserID != "" {
			author = ""
		}
		return Origin{AgentID: agentID, Author: author}, nil
	}
	if p.UserID == "" {
		return Origin{Author: author}, nil
	}
	if p.Name == "" {
		return Origin{}, badRequest("your sign-in carries no name to write under: set one with the identity provider, or name an agent in agent_id")
	}
	return Origin{Author: p.Name, UserID: p.UserID}, nil
}
