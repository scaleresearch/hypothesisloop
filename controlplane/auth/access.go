package auth

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// Placement is what Access reads to place a resource in an org. *db.Store satisfies it.
type Placement interface {
	PlatformExperimentTenancy(ctx context.Context, platformExperimentID string) (tenancy *domain.Tenancy, closedAt *time.Time, err error)
	ExperimentTenancy(ctx context.Context, experimentID string) (*domain.Tenancy, bool, error)
	AgentOrg(ctx context.Context, agentID string) (orgID string, found bool, err error)
	GetCluster(ctx context.Context, name string) (*domain.Cluster, error)
	GetAPIKey(ctx context.Context, id string) (*domain.APIKey, error)
	GetOrg(ctx context.Context, id string) (*domain.Org, error)
	OrgRole(ctx context.Context, orgID, userID string) (role domain.OrgRole, found bool, err error)
	GetProject(ctx context.Context, id string) (*domain.Project, error)
	PlatformExperimentIDs(ctx context.Context, scope domain.TenantScope) ([]string, error)
}

// Access answers whether the request's principal may touch a resource. The rule is the same for
// every resource: place it in a platform experiment, compare that platform experiment's org and
// project to the principal's, and for an agent or a coordinator the platform experiment itself.
// A resource outside the principal's scope, one that does not exist and one in a removed
// platform experiment are answered alike, as not found, so the answer says nothing about another
// org. Only staff, who see every org, pass on an id with no row and get the handler's own answer
// for it.
//
// One thing reaches past a caller's scope: a platform experiment staff shared is read by every
// account, and only read (reaches). It is listed beside the caller's own in every project. Every write to it, and every credential of it, is answered
// to another org's caller as for any platform experiment of another org.
type Access struct {
	placement Placement
}

func NewAccess(placement Placement) *Access {
	return &Access{placement: placement}
}

// NotFound is the refusal for a resource the caller cannot reach, whether or not it exists.
func NotFound(what string) *refusal.Refusal {
	return refusal.NotFound(what)
}

func forbidden(message string) *refusal.Refusal {
	return refusal.Refuse(http.StatusForbidden, refusal.ReasonForbidden, message)
}

type readKey struct{}

// read is what the gate in front of every handler knows of a request that only reads.
type read struct {
	// platformExperimentID is the one platform experiment a listing is narrowed to, empty when
	// the request names none.
	platformExperimentID string
}

// WithRead marks ctx as a request that reads and writes nothing, naming the platform experiment
// its platform_experiment_id narrows a listing to. The gates that authenticate a request set it
// (httpapi for every documented operation, the watch handler for its stream) and nothing else
// does, so no handler decides for itself that it is a read.
func WithRead(ctx context.Context, platformExperimentID string) context.Context {
	return context.WithValue(ctx, readKey{}, read{platformExperimentID: platformExperimentID})
}

// readsShared reports whether the request may read platform experiments staff shared: it is a
// read, and its caller holds an account. A token the platform derived for an agent, a coordinator
// or a job stays within its own platform experiment.
func readsShared(ctx context.Context, p Principal) bool {
	_, reading := ctx.Value(readKey{}).(read)
	return reading && p.HoldsAccount()
}

// readsPriorRun reports whether the request reads a prior run of the caller's own platform
// experiment: an agent or a coordinator reads each as its own, and writes nothing to them.
func readsPriorRun(ctx context.Context, p Principal, platformExperimentID string) bool {
	_, reading := ctx.Value(readKey{}).(read)
	return reading && (p.Kind == KindAgent || p.Kind == KindCoordinator) && slices.Contains(p.PriorRuns, platformExperimentID)
}

// reaches is the one statement of what a caller reaches: what its scope admits, for a read by an
// account a platform experiment staff shared, and for a read by an agent or a coordinator the
// prior runs of its own, with everything placed in them.
func reaches(ctx context.Context, p Principal, t domain.Tenancy) bool {
	return p.Scope().Admits(t) || (t.Shared && readsShared(ctx, p)) || readsPriorRun(ctx, p, t.PlatformExperimentID)
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

// Writes says the principal may write the platform experiment: its scope admits it, without the
// shared platform experiments and the prior runs a read also reaches.
func (a *Access) Writes(ctx context.Context, platformExperimentID string) (bool, error) {
	p, err := principal(ctx)
	if err != nil {
		return false, err
	}
	if p.Scope().Unrestricted() {
		return true, nil
	}
	tenancy, _, err := a.placement.PlatformExperimentTenancy(ctx, platformExperimentID)
	if err != nil {
		return false, err
	}
	return tenancy != nil && p.Scope().Admits(*tenancy), nil
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
	if tenancy == nil || !reaches(ctx, p, *tenancy) {
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
	return admitted(ctx, p, tenancy, "experiment")
}

// admitted answers for a placed row. Staff pass on an absent one with a nil tenancy.
func admitted(ctx context.Context, p Principal, tenancy *domain.Tenancy, what string) (*domain.Tenancy, error) {
	if p.Scope().Unrestricted() {
		return tenancy, nil
	}
	if tenancy == nil || !reaches(ctx, p, *tenancy) {
		return nil, NotFound(what)
	}
	return tenancy, nil
}

func ownedBy(ctx context.Context, tenancy *domain.Tenancy, what string) error {
	p, _ := FromContext(ctx)
	owner := ""
	if tenancy != nil {
		owner = tenancy.AgentID
	}
	if !p.Owns(owner) {
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

// ActAs is Agent for a request that spends as the agent it names: a job, quota asked for or
// given away, the key to its data. A coordinator's token is refused all of them. It is handed to
// a machine the platform rents and is there to keep a run's description, so it holds no agent's hand.
func (a *Access) ActAs(ctx context.Context, agentID string) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	if p.Kind == KindCoordinator {
		return forbidden("a coordinator's token acts as no agent: it submits no job, asks for and gives no quota, and fetches no agent's data key")
	}
	return a.Agent(ctx, agentID)
}

// A stored model session is everything its agent said and saw, so it is no part of what staff
// share with every account: a platform experiment outside the caller's own scope is not found
// here even to a read.
//
// WriteSession admits only the one the session belongs to: the agent agentID with its own
// token, or with no agentID the platform experiment's coordinator with its. Either token ends
// domain.AgentWindDown after its run did, and with it the writing.
func (a *Access) WriteSession(ctx context.Context, platformExperimentID, agentID string) error {
	p, err := a.sessionCaller(ctx, platformExperimentID)
	if err != nil {
		return err
	}
	if !ownsSession(p, platformExperimentID, agentID) {
		return forbidden("a session is written only by the agent or the coordinator it belongs to, with its own token")
	}
	return nil
}

// ReadSession admits the one the session belongs to, and every account that reaches the
// platform experiment as its own: the people and the keys of the org that owns it, and staff.
// They read it while the platform experiment runs and after it has closed. Another agent of the
// platform experiment and its coordinator read no agent's session.
func (a *Access) ReadSession(ctx context.Context, platformExperimentID, agentID string) error {
	p, err := a.sessionCaller(ctx, platformExperimentID)
	if err != nil {
		return err
	}
	if !ownsSession(p, platformExperimentID, agentID) && !p.HoldsAccount() {
		return forbidden("a session is read only by the agent or the coordinator it belongs to, with its own token, and by the org that owns the platform experiment")
	}
	return nil
}

// ReadInstructions admits the one the instructions are for, the agent agentID with its own token
// or with no agentID the platform experiment's coordinator with its, and every account that
// reaches the platform experiment as its own, as ReadSession does.
func (a *Access) ReadInstructions(ctx context.Context, platformExperimentID, agentID string) error {
	p, err := a.sessionCaller(ctx, platformExperimentID)
	if err != nil {
		return err
	}
	if !ownsSession(p, platformExperimentID, agentID) && !p.HoldsAccount() {
		return forbidden("instructions are read only by the agent or the coordinator they are for, with its own token, and by the org that owns the platform experiment")
	}
	return nil
}

func (a *Access) sessionCaller(ctx context.Context, platformExperimentID string) (Principal, error) {
	p, err := principal(ctx)
	if err != nil {
		return Principal{}, err
	}
	if err := validID("platform experiment", platformExperimentID); err != nil {
		return Principal{}, err
	}
	tenancy, _, err := a.placement.PlatformExperimentTenancy(ctx, platformExperimentID)
	if err != nil {
		return Principal{}, err
	}
	if !p.Scope().Unrestricted() && (tenancy == nil || !p.Scope().Admits(*tenancy)) {
		return Principal{}, NotFound("platform experiment")
	}
	return p, nil
}

func ownsSession(p Principal, platformExperimentID, agentID string) bool {
	if p.PlatformExperimentID != platformExperimentID {
		return false
	}
	return p.Kind == KindCoordinator && agentID == "" || p.Kind == KindAgent && agentID != "" && p.AgentID == agentID
}

// Staff admits only the platform's own operators, for an operation that acts across every org.
func (a *Access) Staff(ctx context.Context) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	if p.Kind != KindStaff {
		return forbidden("only staff may do this")
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
			return "", refusal.Invalid(refusal.ReasonInvalidRequest, "org_id is required: this token is not bound to a single org").On("org_id")
		}
		return p.OrgIDs[0], nil
	}
	if !p.Scope().AdmitsOrg(requested) {
		return "", NotFound("org")
	}
	return requested, nil
}

// ListScope is the principal's scope for a listing, narrowed to one project when asked. A
// listing is the caller's own, with two exceptions: narrowed by platform_experiment_id to a
// platform experiment staff shared, it is that platform experiment's for an account that reads,
// and narrowed to a prior run of an agent's or a coordinator's own, it is that run's.
func (a *Access) ListScope(ctx context.Context, projectID string) (domain.TenantScope, error) {
	p, err := principal(ctx)
	if err != nil {
		return domain.TenantScope{}, err
	}
	scope := p.Scope()
	if named, _ := ctx.Value(readKey{}).(read); readsPriorRun(ctx, p, named.platformExperimentID) {
		scope.PlatformExperimentID = named.platformExperimentID
	}
	if named, _ := ctx.Value(readKey{}).(read); named.platformExperimentID != "" && !scope.Unrestricted() && readsShared(ctx, p) &&
		domain.IdentifierShaped(named.platformExperimentID) {
		tenancy, _, err := a.placement.PlatformExperimentTenancy(ctx, named.platformExperimentID)
		if err != nil {
			return domain.TenantScope{}, err
		}
		if tenancy != nil && tenancy.Shared && !scope.Admits(*tenancy) {
			return domain.TenantScope{AllOrgs: true, ProjectID: projectID, PlatformExperimentID: tenancy.PlatformExperimentID}, nil
		}
	}
	if projectID == "" {
		return scope, nil
	}
	if scope.ProjectID != "" && scope.ProjectID != projectID {
		return domain.TenantScope{}, NotFound("project")
	}
	scope.ProjectID = projectID
	return scope, nil
}

// PlatformExperimentListScope is ListScope for the listing of platform experiments themselves.
// withShared says the listing also holds the ones staff shared, whatever the scope: an account
// that reads finds them beside its own in every project. A token bound to one platform
// experiment lists no other.
func (a *Access) PlatformExperimentListScope(ctx context.Context, projectID string) (scope domain.TenantScope, withShared bool, err error) {
	p, err := principal(ctx)
	if err != nil {
		return domain.TenantScope{}, false, err
	}
	scope, err = a.ListScope(ctx, projectID)
	return scope, readsShared(ctx, p), err
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

// Spender resolves whose job a submission is and which platform experiment it runs in. An
// agent's token says both, so what it leaves out is read from the token and what it states must
// agree with it. Any other caller states both, and they are checked as it reaches them.
func (a *Access) Spender(ctx context.Context, platformExperimentID, agentID string) (string, string, error) {
	p, err := principal(ctx)
	if err != nil {
		return "", "", err
	}
	if p.Kind == KindAgent {
		if platformExperimentID == "" {
			platformExperimentID = p.PlatformExperimentID
		}
		if agentID == "" {
			agentID = p.AgentID
		}
	}
	if err := a.PlatformExperiment(ctx, platformExperimentID); err != nil {
		return "", "", err
	}
	if err := a.ActAs(ctx, agentID); err != nil {
		return "", "", err
	}
	return platformExperimentID, agentID, nil
}

// Writer resolves the two facts a record of a write keeps: whose record it is, and whose
// credential made it. The owner is the agent the request names, and the actor is the caller,
// never anything the request says.
//
// An agent token is both: it names itself or nobody, and its record is its own. Any other caller
// may name an agent it reaches and so write for it, which leaves the agent as owner with the
// caller recorded beside it. With no agent named the record has no owner and is the caller's own.
func (a *Access) Writer(ctx context.Context, agentID string) (ownerAgentID string, actor domain.Actor, err error) {
	p, err := principal(ctx)
	if err != nil {
		return "", domain.Actor{}, err
	}
	actor = p.Actor()
	if actor.Kind == "" {
		return "", domain.Actor{}, forbidden("a " + string(p.Kind) + " token writes no record of its own")
	}
	if p.Kind == KindAgent && agentID == "" {
		return p.AgentID, actor, nil
	}
	if agentID == "" {
		return "", actor, nil
	}
	if err := a.Agent(ctx, agentID); err != nil {
		return "", domain.Actor{}, err
	}
	return agentID, actor, nil
}
