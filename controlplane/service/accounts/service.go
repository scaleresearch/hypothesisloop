// Package accounts serves who exists and what they own: orgs, their members and invites,
// projects, API keys and clusters. Who may call what is decided in auth/orgs.go, and in
// policy.go for what an API key reaches, and nowhere else.
package accounts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/scope"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// Accounts is the persistence the handler needs for orgs, members, invites, projects and API keys.
// *db.Store satisfies it.
type Accounts interface {
	CreateOrg(ctx context.Context, name, ownerUserID string, maxOwned int) (*domain.Org, error)
	RenameOrg(ctx context.Context, id, name string) error
	GetOrg(ctx context.Context, id string) (*domain.Org, error)
	ListOrgs(ctx context.Context, orgIDs []string) ([]*domain.Org, error)
	ListAllOrgs(ctx context.Context) ([]*domain.Org, error)
	ListUserOrgs(ctx context.Context, userID string) ([]*domain.Org, error)
	CreateProject(ctx context.Context, orgID, name string, maxProjects int) (*domain.Project, error)
	RenameProject(ctx context.Context, id, name string) error
	GetProject(ctx context.Context, id string) (*domain.Project, error)
	ListProjects(ctx context.Context, orgIDs []string) ([]*domain.Project, error)
	CreateAPIKey(ctx context.Context, key *domain.APIKey, keyHash string) error
	ListAPIKeys(ctx context.Context, orgID string) ([]*domain.APIKey, error)
	GetAPIKey(ctx context.Context, id string) (*domain.APIKey, error)
	RevokeAPIKey(ctx context.Context, id string) error
	ListOrgMembers(ctx context.Context, orgID string, limit, offset int) ([]*domain.OrgMember, int, error)
	CountOrgOwners(ctx context.Context, orgID string) (int, error)
	SetOrgMemberRole(ctx context.Context, orgID, userID string, role domain.OrgRole) (*domain.OrgMember, error)
	RemoveOrgMember(ctx context.Context, orgID, userID string) error
	CreateOrgInvite(ctx context.Context, invite *domain.OrgInvite, tokenHash string) error
	ListPendingOrgInvites(ctx context.Context, orgID string, limit, offset int) ([]*domain.OrgInvite, int, error)
	GetOrgInvite(ctx context.Context, id string) (*domain.OrgInvite, error)
	RevokeOrgInvite(ctx context.Context, inviteID string) error
	AcceptOrgInvite(ctx context.Context, tokenHash, userID string) (*domain.OrgMember, error)
}

// Clusters is the persistence the handler needs for clusters. *db.Store satisfies it.
type Clusters interface {
	RegisterCluster(ctx context.Context, name, orgID string) (*domain.Cluster, error)
	ClustersHosting(ctx context.Context, orgID string) ([]*domain.Cluster, error)
	UpdateCluster(ctx context.Context, name string, shared *bool, change domain.ClusterSettings) (cluster *domain.Cluster, requeued []string, err error)
	SetClusterEnabled(ctx context.Context, name, orgID string, by domain.Actor, enabled bool) (*domain.Cluster, error)
	SetClusterOrder(ctx context.Context, orgID string, names []string) error
	RemoveCluster(ctx context.Context, name string) (requeued []string, err error)
	ClearReportedID(ctx context.Context, name string) (*domain.Cluster, error)
}

// Liveness reads which clusters an agent reported from lately, and when each was last heard
// from. *metricsdb.Store is the one in use.
type Liveness interface {
	LiveClusterHeartbeats(ctx context.Context, window time.Duration) (map[string]bool, error)
	LiveClusterAutoscalerCapability(ctx context.Context, window time.Duration) (map[string]bool, error)
	ClusterLastContacts(ctx context.Context, lookback time.Duration) (map[string]metricsdb.ClusterContact, error)
}

// contactLookback is how far back a cluster's last contact is looked for: one silent for longer
// is listed with none.
const contactLookback = 30 * 24 * time.Hour

// RuntimeAddresses is what a cluster's agents are installed with: where they reach the platform
// from inside their cluster, and the registry and tag their images are pulled under.
type RuntimeAddresses struct {
	APIURL     string
	MetricsURL string
	Registry   string
	ImageTag   string
	// RegistryPlainHTTP says Registry serves plain HTTP (config.RuntimesConfig.RegistryPlainHTTP).
	RegistryPlainHTTP bool
}

// Service serves who exists and what they own. Every method checks its caller with access, and
// what it returns passes through Answer before the caller is told.
type Service struct {
	accounts      Accounts
	clusters      Clusters
	liveness      Liveness
	liveWithin    time.Duration
	signingSecret string
	runtimes      RuntimeAddresses
	limits        Limits
	logger        *zap.Logger
	access        *auth.Access
	policy        policy
}

// Limits is what one account may create: the orgs a person owns and the projects an org holds.
// Staff are held to neither (auth.Access.HeldToLimits).
type Limits struct {
	MaxOrgsPerUser    int `json:"max_orgs_per_user" doc:"How many orgs a person may own when they create one. The orgs of this answer with role owner are the ones that count."`
	MaxProjectsPerOrg int `json:"max_projects_per_org" doc:"How many projects one org may hold. The projects of this answer with that org_id are the ones that count."`
}

// NewService takes liveness, which counts a cluster connected while it reported within
// liveWithin.
func NewService(accounts Accounts, clusters Clusters, liveness Liveness, liveWithin time.Duration, signingSecret string, runtimes RuntimeAddresses, limits Limits, access *auth.Access, logger *zap.Logger) *Service {
	if logger == nil {
		panic("accounts.NewService: a logger is required")
	}
	if limits.MaxOrgsPerUser <= 0 || limits.MaxProjectsPerOrg <= 0 {
		panic("accounts.NewService: both account limits are required and positive")
	}
	return &Service{accounts: accounts, clusters: clusters, liveness: liveness, liveWithin: liveWithin, signingSecret: signingSecret, runtimes: runtimes, limits: limits, logger: logger, access: access, policy: policy{access: access}}
}

// limit is bound for a caller held to the account limits, and none, zero, for staff.
func (h *Service) limit(ctx context.Context, bound int) (int, error) {
	held, err := h.access.HeldToLimits(ctx)
	if err != nil || !held {
		return 0, err
	}
	return bound, nil
}

// Me is the caller and what it reaches.
type Me struct {
	Kind auth.Kind `json:"kind" enum:"user,key,staff"`
	// UserID and Name are set for a signed-in person, KeyID and ProjectID for an API key.
	UserID    string            `json:"user_id,omitempty"`
	Name      string            `json:"name,omitempty"`
	KeyID     string            `json:"key_id,omitempty"`
	ProjectID string            `json:"project_id,omitempty" doc:"The one project an API key is bound to, absent when it reaches its whole org."`
	Orgs      []*CallerOrg      `json:"orgs"`
	Projects  []*domain.Project `json:"projects"`
	Limits    *Limits           `json:"limits,omitempty" doc:"What this caller may create, absent for staff, who are held to no limit. How much of each is used is read from orgs and projects beside it."`
}

// CallerOrg is an org as GET /me lists it, with what the caller may do to it.
type CallerOrg struct {
	*domain.Org
	Actions []domain.OrgAction `json:"actions" doc:"What the caller may do to the org now, each a write the API accepts from them: edit (rename it, and change its projects, members, invites, GitHub installations, clusters and agent types) and create_api_key (POST /orgs/{id}/api-keys, a member's bound to a project)."`
}

// CreatedAPIKey is an API key at the one moment its token exists outside the caller's hands.
type CreatedAPIKey struct {
	domain.APIKey
	Token string `json:"token" doc:"The secret. Returned by this call only: the platform stores its hash."`
}

// ClusterView is a registered cluster as the API shows it to one org. ClusterID is the identity
// its runtime reported, absent until its agent first connects.
type ClusterView struct {
	Name      string    `json:"name"`
	OrgID     string    `json:"org_id,omitempty"`
	Shared    bool      `json:"shared" doc:"The platform places every org's jobs on a shared cluster. Staff decide it."`
	Enabled   bool      `json:"enabled" doc:"Whether the jobs of the org in the path are placed on the cluster. True until an owner of that org turns the cluster off, which no other org sees or is bound by."`
	ClusterID string    `json:"cluster_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	domain.ClusterSettings
}

// viewOf shows a cluster as orgID has it.
func viewOf(c *domain.Cluster, orgID string) ClusterView {
	return ClusterView{Name: c.Name, OrgID: c.OrgID, Shared: c.Shared, Enabled: c.EnabledFor(orgID), ClusterID: c.ReportedID, CreatedAt: c.CreatedAt, ClusterSettings: c.ClusterSettings}
}

// viewFor shows a cluster as orgID has it to one caller. A cluster the caller does not know keeps
// its owner, its identity and its settings to itself.
func viewFor(c *domain.Cluster, orgID string, caller auth.Principal) ClusterView {
	view := viewOf(c, orgID)
	if !caller.KnowsCluster(*c) {
		view.OrgID, view.ClusterID, view.ClusterSettings = "", "", domain.ClusterSettings{}
	}
	return view
}

// RegisteredCluster is a cluster with everything its agents are started with.
type RegisteredCluster struct {
	ClusterView
	RuntimeToken      string         `json:"runtime_token" doc:"The token this cluster's agents start with. Derived from the cluster's name, so it can be read again at any time by registering the name again."`
	APIURL            string         `json:"api_url" doc:"The address this cluster's agents reach the API at."`
	MetricsURL        string         `json:"metrics_url" doc:"The address this cluster's agents send metrics to."`
	Registry          string         `json:"registry" doc:"The registry this cluster's nodes pull the agents' images from."`
	ImageTag          string         `json:"image_tag" doc:"The tag of the agents' images."`
	RegistryPlainHTTP bool           `json:"registry_plain_http" doc:"The registry serves plain HTTP with no TLS, as a local development registry does. The Kubernetes command then pulls the chart over HTTP and has every node's containerd pull from the registry over HTTP."`
	Attach            AttachCommands `json:"attach_commands" doc:"The command that attaches the cluster, for each kind of runtime, built from the fields above. It asks for runtime_token."`
}

// ListedCluster is a cluster an org can use, whether the org has it on for its jobs, whether
// its agent is connected right now, and when it was last heard from.
type ListedCluster struct {
	ClusterView
	Owned             bool                   `json:"owned" doc:"The org registered the cluster and may change its settings. False for a cluster of another org that the platform shares: its org_id, cluster_id, settings, agent_version and last_error are left out."`
	Connected         bool                   `json:"connected"`
	Place             int                    `json:"place,omitempty" doc:"Where the cluster stands in the order a job of the org is tried on its clusters, 1 first. Clusters of one place are tried together, the one with the most room for the job first. The platform's shared clusters come after every cluster of the org's own. Absent while the org has the cluster turned off."`
	AutoscalerEnabled bool                   `json:"autoscaler_enabled" doc:"The cluster's connected agent reports an autoscaler that adds nodes for waiting jobs, which is what scale_up_timeout_seconds and max_speculative_accelerators act on."`
	LastContactAt     *time.Time             `json:"last_contact_at,omitempty" doc:"When the cluster's agent last reconciled with the platform, if within the last 30 days. Absent: not heard from in them, or never."`
	AgentVersion      string                 `json:"agent_version,omitempty" doc:"The version, an image tag, the cluster's agent said it runs at its last contact."`
	VersionSkew       bool                   `json:"version_skew,omitempty" doc:"The agent runs another version than the platform, whose own is the image_tag registering a cluster answers: run the attach command again to upgrade it."`
	LastError         string                 `json:"last_error,omitempty" doc:"What the platform last refused the agent's reconcile with, until it accepts one again."`
	LastErrorAt       *time.Time             `json:"last_error_at,omitempty"`
	Actions           []domain.ClusterAction `json:"actions" doc:"What the caller may do to the cluster now, each a write the API accepts from them: enable (turn it on and off for the org's jobs, and order it), edit (PATCH its settings, and forget its identity), remove, and share (PATCH shared)."`
}

// clusterActions is what caller, holding role in orgID, may do to the cluster c its jobs may use,
// by the rules auth.Access.ClusterUse and ClusterOf and UpdateCluster refuse by: an administrator
// of the org turns any of them on and off for its jobs and changes and removes the org's own, and
// staff share one.
func clusterActions(c *domain.Cluster, orgID string, caller auth.Principal, role domain.OrgRole) []domain.ClusterAction {
	actions := []domain.ClusterAction{}
	if auth.Administers(caller, role) != nil {
		return actions
	}
	actions = append(actions, domain.ClusterActionEnable)
	if c.OrgID != orgID {
		return actions
	}
	actions = append(actions, domain.ClusterActionEdit, domain.ClusterActionRemove)
	if caller.Kind == auth.KindStaff {
		actions = append(actions, domain.ClusterActionShare)
	}
	return actions
}

// listed shows a cluster to a caller, holding role in orgID, listing the clusters of orgID.
// platformVersion is the image tag the platform runs, which an agent's version is compared with.
func listed(c *domain.Cluster, orgID string, caller auth.Principal, role domain.OrgRole, connected, autoscaler bool, contact *metricsdb.ClusterContact, platformVersion string) ListedCluster {
	out := ListedCluster{ClusterView: viewFor(c, orgID, caller), Owned: c.OrgID == orgID, Connected: connected, AutoscalerEnabled: autoscaler, Actions: clusterActions(c, orgID, caller, role)}
	if contact != nil {
		at := contact.At
		out.LastContactAt = &at
	}
	if !caller.KnowsCluster(*c) {
		return out
	}
	if contact != nil {
		out.AgentVersion = contact.AgentVersion
		out.VersionSkew = contact.AgentVersion != "" && contact.AgentVersion != platformVersion
	}
	out.LastError, out.LastErrorAt = c.LastError, c.LastErrorAt
	return out
}

// listClusters answers the clusters the jobs of orgID run on, in the order a job is placed.
func (h *Service) listClusters(ctx context.Context, orgID string, caller auth.Principal) ([]ListedCluster, error) {
	role, err := h.access.Role(ctx, caller, orgID)
	if err != nil {
		return nil, err
	}
	clusters, err := h.clusters.ClustersHosting(ctx, orgID)
	if err != nil {
		return nil, err
	}
	connected, err := h.liveness.LiveClusterHeartbeats(ctx, h.liveWithin)
	if err != nil {
		return nil, err
	}
	autoscalers, err := h.liveness.LiveClusterAutoscalerCapability(ctx, h.liveWithin)
	if err != nil {
		return nil, err
	}
	contacts, err := h.liveness.ClusterLastContacts(ctx, contactLookback)
	if err != nil {
		return nil, err
	}
	out := make([]ListedCluster, len(clusters))
	// clusters come highest priority first, and placement tries one priority at a time
	// (placement.Tiers), so each new priority among the clusters in use is the next place.
	place, placed := 0, -1
	for i, c := range clusters {
		var contact *metricsdb.ClusterContact
		if seen, ok := contacts[c.Name]; ok {
			contact = &seen
		}
		out[i] = listed(c, orgID, caller, role, connected[c.Name], connected[c.Name] && autoscalers[c.Name], contact, h.runtimes.ImageTag)
		if !c.EnabledFor(orgID) {
			continue
		}
		if c.Priority != placed {
			place, placed = place+1, c.Priority
		}
		out[i].Place = place
	}
	return out, nil
}

// WithdrawnCluster is a cluster after it stopped taking jobs it held, with the jobs that went
// back to the queue for it.
type WithdrawnCluster struct {
	ClusterView
	Requeued []string `json:"requeued" doc:"The jobs that were placed on the cluster and are queued again for another one. Each keeps its retries."`
}

func validName(what, name string) (string, error) {
	name, err := domain.ValidateName(what, name)
	if err != nil {
		return "", refusal.Reject(http.StatusBadRequest, err.Error())
	}
	return name, nil
}

// Answer turns what a method returns into what the caller is told. A refusal goes out as it is.
// What the stores refuse gets its status. Anything else is the platform failing: it is logged
// and answered without its text, which can hold a query or a value of another org.
func (h *Service) Answer(op string, err error) error {
	var refused interface{ GetStatus() int }
	switch {
	case errors.As(err, &refused):
		return err
	case errors.Is(err, db.ErrOrgNotFound):
		return refusal.Reject(http.StatusNotFound, "org not found")
	case errors.Is(err, db.ErrProjectNotFound):
		return refusal.Reject(http.StatusNotFound, "project not found")
	case errors.Is(err, db.ErrAPIKeyNotFound):
		return refusal.Reject(http.StatusNotFound, "api key not found")
	case errors.Is(err, db.ErrUserNotFound):
		return refusal.Reject(http.StatusNotFound, "user not found")
	case errors.Is(err, db.ErrClusterNotFound):
		return refusal.Reject(http.StatusNotFound, "cluster not found")
	case errors.Is(err, db.ErrOrgMemberNotFound):
		return refusal.NotFound("member")
	case errors.Is(err, db.ErrOrgInviteNotFound):
		return refusal.NotFound("invite")
	case errors.Is(err, db.ErrLastOwner):
		return refusal.Conflict(ReasonLastOwner, "an org keeps at least one owner: make someone else an owner first")
	case errors.Is(err, db.ErrAlreadyOrgMember):
		return refusal.Conflict(ReasonAlreadyMember, "you are already a member of this org; the invite is still unused")
	case errors.Is(err, db.ErrOrgInviteUsed):
		return refusal.Conflict(ReasonInviteUsed, "the invite was already accepted")
	case errors.Is(err, db.ErrOrgInviteRevoked):
		return refusal.Conflict(ReasonInviteRevoked, "the invite was revoked")
	case errors.Is(err, db.ErrOrgInviteExpired):
		return refusal.Conflict(ReasonInviteExpired, "the invite has expired: ask an owner of the org for a new one")
	case errors.Is(err, db.ErrAPIKeyCreatorLacksOrg):
		return auth.Forbidden("forbidden: " + err.Error())
	case errors.Is(err, db.ErrMaxOrgsReached):
		return refusal.Conflict(ReasonMaxOrgsReached, fmt.Sprintf("a person owns at most %d orgs, and the owner of this one already does", h.limits.MaxOrgsPerUser))
	case errors.Is(err, db.ErrMaxProjectsReached):
		return refusal.Conflict(ReasonMaxProjectsReached, fmt.Sprintf("an org holds at most %d projects, and this one already does", h.limits.MaxProjectsPerOrg))
	case errors.Is(err, db.ErrProjectNameTaken), errors.Is(err, db.ErrClusterNameTaken):
		return refusal.Reject(http.StatusConflict, err.Error())
	default:
		return auth.Internal(h.logger, "accounts request failed", err, zap.String("operation", op))
	}
}

func orEmpty(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

func (h *Service) me(ctx context.Context) (*Me, error) {
	p, err := h.access.Account(ctx)
	if err != nil {
		return nil, err
	}
	me := &Me{Kind: p.Kind, UserID: p.UserID, Name: p.Name, KeyID: p.KeyID}
	if p.Kind != auth.KindStaff {
		limits := h.limits
		me.Limits = &limits
	}
	var orgs []*domain.Org
	switch p.Kind {
	case auth.KindStaff:
		if orgs, err = h.accounts.ListAllOrgs(ctx); err != nil {
			return nil, err
		}
		if p.UserID != "" {
			own, err := h.accounts.ListUserOrgs(ctx, p.UserID)
			if err != nil {
				return nil, err
			}
			roles := make(map[string]domain.OrgRole, len(own))
			for _, org := range own {
				roles[org.ID] = org.Role
			}
			for _, org := range orgs {
				org.Role = roles[org.ID]
			}
		}
	case auth.KindKey:
		me.ProjectID = p.ProjectID
		if orgs, err = h.accounts.ListOrgs(ctx, p.OrgIDs); err != nil {
			return nil, err
		}
	default:
		if orgs, err = h.accounts.ListUserOrgs(ctx, p.UserID); err != nil {
			return nil, err
		}
	}
	orgIDs := make([]string, len(orgs))
	me.Orgs = make([]*CallerOrg, len(orgs))
	for i, org := range orgs {
		orgIDs[i] = org.ID
		me.Orgs[i] = &CallerOrg{Org: org, Actions: orgActions(p, org)}
	}
	projects, err := h.accounts.ListProjects(ctx, orgIDs)
	if err != nil {
		return nil, err
	}
	me.Projects = []*domain.Project{}
	for _, project := range projects {
		if p.Scope().Admits(domain.Tenancy{OrgID: project.OrgID, ProjectID: project.ID}) {
			me.Projects = append(me.Projects, project)
		}
	}
	return me, nil
}

func (h *Service) createAPIKey(ctx context.Context, orgID, projectID, name string) (*CreatedAPIKey, error) {
	p, err := h.policy.mintKey(ctx, orgID, projectID)
	if err != nil {
		return nil, err
	}
	if name, err = validName("api key", name); err != nil {
		return nil, err
	}
	key := &domain.APIKey{OrgID: orgID, ProjectID: projectID, Actor: domain.Actor{Kind: domain.ActorUser, ID: p.UserID}, Name: name}
	if p.Kind == auth.KindKey {
		// The person answerable for a minted key is the one answerable for the key that minted it.
		minter, err := h.accounts.GetAPIKey(ctx, p.KeyID)
		if err != nil {
			return nil, err
		}
		if minter == nil {
			return nil, fmt.Errorf("the calling api key %s has no row", p.KeyID)
		}
		key.Actor.ID, key.MintedBy = minter.Actor.ID, minter.ID
	}
	id, token, err := auth.NewAPIKey()
	if err != nil {
		return nil, err
	}
	key.ID = id
	if err := h.accounts.CreateAPIKey(ctx, key, auth.HashAPIKey(token)); err != nil {
		return nil, err
	}
	return &CreatedAPIKey{APIKey: *key, Token: token}, nil
}

// ListedAPIKey is an API key with what the caller may do to it.
type ListedAPIKey struct {
	*domain.APIKey
	Actions []domain.APIKeyAction `json:"actions" doc:"What the caller may do to the key now, a write the API accepts from them: revoke (DELETE /api-keys/{id}), until the key is revoked."`
}

func (h *Service) reachableKeys(ctx context.Context, orgID string) ([]ListedAPIKey, error) {
	p, err := h.access.OrgMember(ctx, orgID)
	if err != nil {
		return nil, err
	}
	role, err := h.access.Role(ctx, p, orgID)
	if err != nil {
		return nil, err
	}
	keys, err := h.accounts.ListAPIKeys(ctx, orgID)
	if err != nil {
		return nil, err
	}
	reached := reachableKeys(p, keys)
	out := make([]ListedAPIKey, len(reached))
	for i, k := range reached {
		out[i] = ListedAPIKey{APIKey: k, Actions: keyActions(p, role, k, keys)}
	}
	return out, nil
}

// revokeAPIKey answers for a key the caller cannot reach exactly as for one that does not exist.
func (h *Service) revokeAPIKey(ctx context.Context, id string) (*domain.APIKey, error) {
	key, err := h.access.APIKey(ctx, id)
	if err != nil {
		return nil, err
	}
	err = h.policy.endKey(ctx, key, func() ([]*domain.APIKey, error) { return h.accounts.ListAPIKeys(ctx, key.OrgID) })
	if err != nil {
		return nil, err
	}
	if err := h.accounts.RevokeAPIKey(ctx, id); err != nil {
		return nil, err
	}
	revoked, err := h.accounts.GetAPIKey(ctx, id)
	if err != nil {
		return nil, err
	}
	if revoked == nil {
		return nil, auth.NotFound("api key")
	}
	return revoked, nil
}

// Me is the caller and what it reaches.
func (h *Service) Me(ctx context.Context) (*Me, error) {
	return h.me(ctx)
}

// CreateOrg creates the org name, owned by the caller or by the owner staff name.
func (h *Service) CreateOrg(ctx context.Context, requestedOwner, name string) (*domain.Org, error) {
	owner, err := h.policy.createOrg(ctx, requestedOwner)
	if err != nil {
		return nil, err
	}
	name, err = validName("org", name)
	if err != nil {
		return nil, err
	}
	maxOwned, err := h.limit(ctx, h.limits.MaxOrgsPerUser)
	if err != nil {
		return nil, err
	}
	return h.accounts.CreateOrg(ctx, name, owner, maxOwned)
}

func (h *Service) RenameOrg(ctx context.Context, id, name string) (*domain.Org, error) {
	if _, err := h.access.OrgAdmin(ctx, id); err != nil {
		return nil, err
	}
	name, err := validName("org", name)
	if err != nil {
		return nil, err
	}
	if err := h.accounts.RenameOrg(ctx, id, name); err != nil {
		return nil, err
	}
	org, err := h.accounts.GetOrg(ctx, id)
	if err != nil {
		return nil, err
	}
	if org == nil {
		return nil, refusal.Reject(http.StatusNotFound, "org not found")
	}
	return org, nil
}

// Projects are the projects of orgID the caller sees.
func (h *Service) Projects(ctx context.Context, orgID string) ([]*domain.Project, error) {
	if _, err := h.access.OrgMember(ctx, orgID); err != nil {
		return nil, err
	}
	projects, err := h.accounts.ListProjects(ctx, []string{orgID})
	if err != nil {
		return nil, err
	}
	return h.policy.visibleProjects(ctx, orgID, projects)
}

func (h *Service) CreateProject(ctx context.Context, orgID, name string) (*domain.Project, error) {
	if _, err := h.access.OrgAdmin(ctx, orgID); err != nil {
		return nil, err
	}
	name, err := validName("project", name)
	if err != nil {
		return nil, err
	}
	maxProjects, err := h.limit(ctx, h.limits.MaxProjectsPerOrg)
	if err != nil {
		return nil, err
	}
	return h.accounts.CreateProject(ctx, orgID, name, maxProjects)
}

func (h *Service) RenameProject(ctx context.Context, id, name string) (*domain.Project, error) {
	project, err := h.access.ProjectAdmin(ctx, id)
	if err != nil {
		return nil, err
	}
	name, err = validName("project", name)
	if err != nil {
		return nil, err
	}
	if err := h.accounts.RenameProject(ctx, id, name); err != nil {
		return nil, err
	}
	project.Name = name
	return project, nil
}

func (h *Service) CreateAPIKey(ctx context.Context, orgID, projectID, name string) (*CreatedAPIKey, error) {
	return h.createAPIKey(ctx, orgID, projectID, name)
}

// APIKeys are the keys of orgID the caller reaches.
func (h *Service) APIKeys(ctx context.Context, orgID string) ([]ListedAPIKey, error) {
	return h.reachableKeys(ctx, orgID)
}

func (h *Service) RevokeAPIKey(ctx context.Context, id string) (*domain.APIKey, error) {
	return h.revokeAPIKey(ctx, id)
}

// RegisterCluster registers the cluster name for orgID and answers what its agents start with.
func (h *Service) RegisterCluster(ctx context.Context, orgID, name string) (*RegisteredCluster, error) {
	if _, err := h.access.OrgAdmin(ctx, orgID); err != nil {
		return nil, err
	}
	if err := domain.ValidateIdentifier("cluster name", name); err != nil {
		return nil, refusal.Reject(http.StatusBadRequest, err.Error())
	}
	cluster, err := h.clusters.RegisterCluster(ctx, name, orgID)
	if err != nil {
		return nil, err
	}
	token, err := auth.Mint(h.signingSecret, scope.Runtime(cluster.Name))
	if err != nil {
		return nil, err
	}
	registered := RegisteredCluster{
		ClusterView: viewOf(cluster, orgID), RuntimeToken: token, APIURL: h.runtimes.APIURL, MetricsURL: h.runtimes.MetricsURL,
		Registry: h.runtimes.Registry, ImageTag: h.runtimes.ImageTag, RegistryPlainHTTP: h.runtimes.RegistryPlainHTTP,
	}
	registered.Attach = AttachCommandsFor(registered)
	return &registered, nil
}

// Clusters are the clusters the jobs of orgID run on, in the order a job is placed.
func (h *Service) Clusters(ctx context.Context, orgID string) ([]ListedCluster, error) {
	caller, err := h.policy.readOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return h.listClusters(ctx, orgID, caller)
}

// OrderClusters sets the order the jobs of orgID are placed on its own clusters.
func (h *Service) OrderClusters(ctx context.Context, orgID string, names []string) ([]ListedCluster, error) {
	caller, err := h.access.OrgAdmin(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if err := h.clusters.SetClusterOrder(ctx, orgID, names); err != nil {
		if errors.Is(err, db.ErrClusterOrder) {
			return nil, refusal.Reject(http.StatusUnprocessableEntity, err.Error())
		}
		return nil, err
	}
	return h.listClusters(ctx, orgID, caller)
}

// SetClusterEnabled turns the cluster name on or off for the jobs of orgID.
func (h *Service) SetClusterEnabled(ctx context.Context, orgID, name string, enabled bool) (*ClusterView, error) {
	caller, _, err := h.access.ClusterUse(ctx, orgID, name)
	if err != nil {
		return nil, err
	}
	cluster, err := h.clusters.SetClusterEnabled(ctx, name, orgID, caller.Actor(), enabled)
	if err != nil {
		return nil, err
	}
	view := viewFor(cluster, orgID, caller)
	return &view, nil
}

// UpdateCluster changes the cluster's settings, or whether every org shares it.
func (h *Service) UpdateCluster(ctx context.Context, orgID, name string, shared *bool, settings domain.ClusterSettings) (*WithdrawnCluster, error) {
	if shared != nil {
		if err := h.access.Staff(ctx); err != nil {
			return nil, err
		}
	}
	if _, err := h.access.ClusterOf(ctx, orgID, name); err != nil {
		return nil, err
	}
	if shared == nil && settings.Empty() {
		return nil, refusal.Reject(http.StatusUnprocessableEntity, "name what changes: shared, scale_up_timeout_seconds, max_speculative_accelerators, stuck_pending_timeout_seconds or status_silence_ceiling_seconds")
	}
	if err := domain.ValidateClusterSettings(settings); err != nil {
		return nil, refusal.Reject(http.StatusUnprocessableEntity, err.Error())
	}
	cluster, requeued, err := h.clusters.UpdateCluster(ctx, name, shared, settings)
	if err != nil {
		return nil, err
	}
	return &WithdrawnCluster{ClusterView: viewOf(cluster, orgID), Requeued: orEmpty(requeued)}, nil
}

// RemoveCluster ends the cluster name for the platform.
func (h *Service) RemoveCluster(ctx context.Context, orgID, name string) (*WithdrawnCluster, error) {
	cluster, err := h.access.ClusterOf(ctx, orgID, name)
	if err != nil {
		return nil, err
	}
	requeued, err := h.clusters.RemoveCluster(ctx, name)
	if err != nil {
		return nil, err
	}
	return &WithdrawnCluster{ClusterView: viewOf(cluster, orgID), Requeued: orEmpty(requeued)}, nil
}

// ForgetClusterIdentity forgets the identity the cluster's runtime reported.
func (h *Service) ForgetClusterIdentity(ctx context.Context, orgID, name string) (*ClusterView, error) {
	if _, err := h.access.ClusterOf(ctx, orgID, name); err != nil {
		return nil, err
	}
	cluster, err := h.clusters.ClearReportedID(ctx, name)
	if err != nil {
		return nil, err
	}
	view := viewOf(cluster, orgID)
	return &view, nil
}
