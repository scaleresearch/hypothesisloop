// Package accounts serves who exists and what they own: orgs, their members and invites,
// projects, API keys and clusters. Who may call what is decided in shared/auth/orgs.go, and in
// policy.go for what an API key reaches, and nowhere else.
package accounts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Accounts is the persistence the handler needs for orgs, members, invites, projects and API keys.
// *db.AccountsStore satisfies it.
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
	SetOrgMemberRole(ctx context.Context, orgID, userID string, role domain.OrgRole) (*domain.OrgMember, error)
	RemoveOrgMember(ctx context.Context, orgID, userID string) error
	CreateOrgInvite(ctx context.Context, invite *domain.OrgInvite, tokenHash string) error
	ListPendingOrgInvites(ctx context.Context, orgID string, limit, offset int) ([]*domain.OrgInvite, int, error)
	GetOrgInvite(ctx context.Context, id string) (*domain.OrgInvite, error)
	RevokeOrgInvite(ctx context.Context, inviteID string) error
	AcceptOrgInvite(ctx context.Context, tokenHash, userID string) (*domain.OrgMember, error)
}

// Clusters is the persistence the handler needs for clusters. *db.ClustersStore satisfies it.
type Clusters interface {
	RegisterCluster(ctx context.Context, name, orgID string) (*domain.Cluster, error)
	ClustersHosting(ctx context.Context, orgID string) ([]*domain.Cluster, error)
	SetClusterShared(ctx context.Context, name string, shared bool) (cluster *domain.Cluster, requeued []string, err error)
	RemoveCluster(ctx context.Context, name string) (requeued []string, err error)
	ClearReportedID(ctx context.Context, name string) (*domain.Cluster, error)
}

// Connectivity reports which clusters have an agent connected right now, by name.
type Connectivity func(ctx context.Context) (map[string]bool, error)

// RuntimeAddresses is where a cluster's agents reach the platform from inside their cluster.
type RuntimeAddresses struct {
	APIURL     string
	MetricsURL string
}

type Handler struct {
	accounts      Accounts
	clusters      Clusters
	connected     Connectivity
	signingSecret string
	runtimes      RuntimeAddresses
	limits        Limits
	logger        *zap.Logger
	// access and policy are the document's own access check, set when the routes are registered.
	access *auth.Access
	policy policy
}

// Limits is what one account may create: the orgs a person owns and the projects an org holds.
// Staff are held to neither (auth.Access.HeldToLimits).
type Limits struct {
	MaxOrgsPerUser    int `json:"max_orgs_per_user" doc:"How many orgs a person may own when they create one. The orgs of this answer with role owner are the ones that count."`
	MaxProjectsPerOrg int `json:"max_projects_per_org" doc:"How many projects one org may hold. The projects of this answer with that org_id are the ones that count."`
}

func NewHandler(accounts Accounts, clusters Clusters, connected Connectivity, signingSecret string, runtimes RuntimeAddresses, limits Limits, logger *zap.Logger) *Handler {
	if logger == nil {
		panic("accounts.NewHandler: a logger is required")
	}
	if limits.MaxOrgsPerUser <= 0 || limits.MaxProjectsPerOrg <= 0 {
		panic("accounts.NewHandler: both account limits are required and positive")
	}
	return &Handler{accounts: accounts, clusters: clusters, connected: connected, signingSecret: signingSecret, runtimes: runtimes, limits: limits, logger: logger}
}

// limit is bound for a caller held to the account limits, and none, zero, for staff.
func (h *Handler) limit(ctx context.Context, bound int) (int, error) {
	held, err := h.access.HeldToLimits(ctx)
	if err != nil || !held {
		return 0, err
	}
	return bound, nil
}

// Me is the caller and what it reaches.
type Me struct {
	Kind auth.Kind `json:"kind" doc:"user, key or staff"`
	// UserID and Name are set for a signed-in person, KeyID and ProjectID for an API key.
	UserID    string            `json:"user_id,omitempty"`
	Name      string            `json:"name,omitempty"`
	KeyID     string            `json:"key_id,omitempty"`
	ProjectID string            `json:"project_id,omitempty" doc:"The one project an API key is bound to, absent when it reaches its whole org."`
	Orgs      []*domain.Org     `json:"orgs"`
	Projects  []*domain.Project `json:"projects"`
	Limits    *Limits           `json:"limits,omitempty" doc:"What this caller may create, absent for staff, who are held to no limit. How much of each is used is read from orgs and projects beside it."`
}

// CreatedAPIKey is an API key at the one moment its token exists outside the caller's hands.
type CreatedAPIKey struct {
	domain.APIKey
	Token string `json:"token" doc:"The secret. Returned by this call only: the platform stores its hash."`
}

// ClusterView is a registered cluster as the API shows it. ClusterID is the identity its runtime
// reported, the key of its settings, absent until its agent first connects.
type ClusterView struct {
	Name      string    `json:"name"`
	OrgID     string    `json:"org_id,omitempty"`
	Shared    bool      `json:"shared" doc:"The platform places every org's jobs on a shared cluster. Staff decide it."`
	ClusterID string    `json:"cluster_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func viewOf(c *domain.Cluster) ClusterView {
	return ClusterView{Name: c.Name, OrgID: c.OrgID, Shared: c.Shared, ClusterID: c.ReportedID, CreatedAt: c.CreatedAt}
}

// RegisteredCluster is a cluster with everything its agents are started with.
type RegisteredCluster struct {
	ClusterView
	RuntimeToken string `json:"runtime_token" doc:"The token this cluster's agents start with. Derived from the cluster's name, so it can be read again at any time by registering the name again."`
	APIURL       string `json:"api_url" doc:"The address this cluster's agents reach the API at."`
	MetricsURL   string `json:"metrics_url" doc:"The address this cluster's agents send metrics to."`
}

// ListedCluster is a cluster an org's jobs can be placed on and whether its agent is connected
// right now.
type ListedCluster struct {
	ClusterView
	Owned     bool `json:"owned" doc:"The org registered the cluster and may change its settings. False for a cluster of another org that the platform shares: its org_id and cluster_id are left out."`
	Connected bool `json:"connected"`
}

// listed shows a cluster to a caller listing the clusters of orgID. A cluster the caller does not
// know keeps its owner and the identity its settings are kept under to itself.
func listed(c *domain.Cluster, orgID string, caller auth.Principal, connected bool) ListedCluster {
	out := ListedCluster{ClusterView: viewOf(c), Owned: c.OrgID == orgID, Connected: connected}
	if !caller.KnowsCluster(*c) {
		out.OrgID, out.ClusterID = "", ""
	}
	return out
}

// WithdrawnCluster is a cluster after it stopped taking jobs it held, with the jobs that went
// back to the queue for it.
type WithdrawnCluster struct {
	ClusterView
	Requeued []string `json:"requeued" doc:"The jobs that were placed on the cluster and are queued again for another one. Each keeps its retries."`
}

type nameBody struct {
	Name string `json:"name"`
}

func validName(what, name string) (string, error) {
	name, err := domain.ValidateName(what, name)
	if err != nil {
		return "", huma.Error400BadRequest(err.Error())
	}
	return name, nil
}

// answer turns what a route returns into what the caller is told. A refusal goes out as it is.
// What the stores refuse gets its status. Anything else is the platform failing: it is logged
// and answered without its text, which can hold a query or a value of another org.
func (h *Handler) answer(op string, err error) error {
	var refusal huma.StatusError
	switch {
	case errors.As(err, &refusal):
		return err
	case errors.Is(err, db.ErrOrgNotFound):
		return huma.Error404NotFound("org not found")
	case errors.Is(err, db.ErrProjectNotFound):
		return huma.Error404NotFound("project not found")
	case errors.Is(err, db.ErrAPIKeyNotFound):
		return huma.Error404NotFound("api key not found")
	case errors.Is(err, db.ErrUserNotFound):
		return huma.Error404NotFound("user not found")
	case errors.Is(err, db.ErrClusterNotFound):
		return huma.Error404NotFound("cluster not found")
	case errors.Is(err, db.ErrOrgMemberNotFound):
		return notFound("member")
	case errors.Is(err, db.ErrOrgInviteNotFound):
		return notFound("invite")
	case errors.Is(err, db.ErrLastOwner):
		return conflict(ReasonLastOwner, "an org keeps at least one owner: make someone else an owner first")
	case errors.Is(err, db.ErrAlreadyOrgMember):
		return conflict(ReasonAlreadyMember, "you are already a member of this org; the invite is still unused")
	case errors.Is(err, db.ErrOrgInviteUsed):
		return conflict(ReasonInviteUsed, "the invite was already accepted")
	case errors.Is(err, db.ErrOrgInviteRevoked):
		return conflict(ReasonInviteRevoked, "the invite was revoked")
	case errors.Is(err, db.ErrOrgInviteExpired):
		return conflict(ReasonInviteExpired, "the invite has expired: ask an owner of the org for a new one")
	case errors.Is(err, db.ErrAPIKeyCreatorLacksOrg):
		return auth.Forbidden("forbidden: " + err.Error())
	case errors.Is(err, db.ErrMaxOrgsReached):
		return conflict(ReasonMaxOrgsReached, fmt.Sprintf("a person owns at most %d orgs, and the owner of this one already does", h.limits.MaxOrgsPerUser))
	case errors.Is(err, db.ErrMaxProjectsReached):
		return conflict(ReasonMaxProjectsReached, fmt.Sprintf("an org holds at most %d projects, and this one already does", h.limits.MaxProjectsPerOrg))
	case errors.Is(err, db.ErrProjectNameTaken), errors.Is(err, db.ErrClusterNameTaken):
		return huma.Error409Conflict(err.Error())
	default:
		h.logger.Error("accounts request failed", zap.String("operation", op), zap.Error(err))
		return huma.Error500InternalServerError(auth.InternalError)
	}
}

// route registers one accounts operation and passes every error it returns through answer.
func route[I, O any](doc *apidocs.Doc, h *Handler, op huma.Operation, handle func(context.Context, *I) (*O, error)) {
	apidocs.Register(doc, apidocs.AudienceCoordinator, op, func(ctx context.Context, in *I) (*O, error) {
		out, err := handle(ctx, in)
		if err != nil {
			return nil, h.answer(op.OperationID, err)
		}
		return out, nil
	})
}

// RegisterHuma registers the accounts operations on the shared API document.
func RegisterHuma(doc *apidocs.Doc, h *Handler) {
	h.access, h.policy = doc.Access, policy{access: doc.Access}
	registerMembers(doc, h)
	route(doc, h, huma.Operation{
		OperationID: "get-me", Method: "GET", Path: "/me",
		Summary: "Who this token is and what it reaches", Tags: []string{"accounts"},
		Description: "The caller with the orgs and projects it reaches. A person sees the orgs they belong to with their role, " +
			"an API key its org and the projects it is bound to, staff every org. limits is what the caller may create.",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body Me }, error) {
		me, err := h.me(ctx)
		if err != nil {
			return nil, err
		}
		return &struct{ Body Me }{Body: *me}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "create-org", Method: "POST", Path: "/orgs", DefaultStatus: http.StatusCreated,
		Summary: "Create an org", Tags: []string{"accounts"},
		Description: "The org starts with its creator as owner and a project named default. A person creates an org for " +
			"themselves. Staff name the owner in owner_user_id. An API key cannot create an org. " +
			"A person owns a bounded number of orgs, their first one included: past it the create answers 409 with reason max_orgs_reached. " +
			"GET /me states the bound in limits. Staff are held to none, whoever they create for.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name        string `json:"name"`
			OwnerUserID string `json:"owner_user_id,omitempty" doc:"Staff only: the user who owns the org."`
		}
	}) (*struct{ Body domain.Org }, error) {
		owner, err := h.policy.createOrg(ctx, in.Body.OwnerUserID)
		if err != nil {
			return nil, err
		}
		name, err := validName("org", in.Body.Name)
		if err != nil {
			return nil, err
		}
		maxOwned, err := h.limit(ctx, h.limits.MaxOrgsPerUser)
		if err != nil {
			return nil, err
		}
		org, err := h.accounts.CreateOrg(ctx, name, owner, maxOwned)
		if err != nil {
			return nil, err
		}
		return &struct{ Body domain.Org }{Body: *org}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "rename-org", Method: "PATCH", Path: "/orgs/{id}",
		Summary: "Rename an org", Tags: []string{"accounts"},
		Description: "The id stays. Two orgs may carry the same name.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body nameBody
	}) (*struct{ Body domain.Org }, error) {
		if _, err := h.access.OrgAdmin(ctx, in.ID); err != nil {
			return nil, err
		}
		name, err := validName("org", in.Body.Name)
		if err != nil {
			return nil, err
		}
		if err := h.accounts.RenameOrg(ctx, in.ID, name); err != nil {
			return nil, err
		}
		org, err := h.accounts.GetOrg(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		if org == nil {
			return nil, huma.Error404NotFound("org not found")
		}
		return &struct{ Body domain.Org }{Body: *org}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "list-projects", Method: "GET", Path: "/orgs/{id}/projects",
		Summary: "List an org's projects", Tags: []string{"accounts"},
		Description: "Oldest first. An API key bound to one project sees that project only.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct {
		Body struct {
			Projects []*domain.Project `json:"projects"`
		}
	}, error) {
		if _, err := h.access.OrgMember(ctx, in.ID); err != nil {
			return nil, err
		}
		projects, err := h.accounts.ListProjects(ctx, []string{in.ID})
		if err != nil {
			return nil, err
		}
		out := &struct {
			Body struct {
				Projects []*domain.Project `json:"projects"`
			}
		}{}
		if out.Body.Projects, err = h.policy.visibleProjects(ctx, in.ID, projects); err != nil {
			return nil, err
		}
		return out, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "create-project", Method: "POST", Path: "/orgs/{id}/projects", DefaultStatus: http.StatusCreated,
		Summary: "Create a project in an org", Tags: []string{"accounts"},
		Description: "A project groups platform experiments. Its name is unique within the org: a taken name answers 409. " +
			"An org holds a bounded number of projects, its default one included: past it the create answers 409 with reason max_projects_reached. " +
			"GET /me states the bound in limits. Staff are held to none.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body nameBody
	}) (*struct{ Body domain.Project }, error) {
		if _, err := h.access.OrgAdmin(ctx, in.ID); err != nil {
			return nil, err
		}
		name, err := validName("project", in.Body.Name)
		if err != nil {
			return nil, err
		}
		maxProjects, err := h.limit(ctx, h.limits.MaxProjectsPerOrg)
		if err != nil {
			return nil, err
		}
		project, err := h.accounts.CreateProject(ctx, in.ID, name, maxProjects)
		if err != nil {
			return nil, err
		}
		return &struct{ Body domain.Project }{Body: *project}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "rename-project", Method: "PATCH", Path: "/projects/{id}",
		Summary: "Rename a project", Tags: []string{"accounts"},
		Description: "The id stays, so platform experiments and API keys of the project are untouched. A name another project of the org carries answers 409.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body nameBody
	}) (*struct{ Body domain.Project }, error) {
		project, err := h.access.ProjectAdmin(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		name, err := validName("project", in.Body.Name)
		if err != nil {
			return nil, err
		}
		if err := h.accounts.RenameProject(ctx, in.ID, name); err != nil {
			return nil, err
		}
		project.Name = name
		return &struct{ Body domain.Project }{Body: *project}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "create-api-key", Method: "POST", Path: "/orgs/{id}/api-keys", DefaultStatus: http.StatusCreated,
		Summary: "Create an API key", Tags: []string{"accounts"},
		Description: "Returns the token once: the platform keeps its hash only, so a lost token is replaced by a new key. " +
			"It is the one secret of this API that cannot be read again; a cluster's runtime token and an agent's token are derived and are returned whenever asked. " +
			"With project_id the key reaches that project only, without it the whole org. An owner creates either; a member one bound to a project. " +
			"A key works while the person who created it could still create it: it stops when they leave the org, and one of the whole org when they stop being an owner. " +
			"A key may mint keys no wider than itself, and revoking a key revokes every key minted from it.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			Name      string `json:"name"`
			ProjectID string `json:"project_id,omitempty"`
		}
	}) (*struct{ Body CreatedAPIKey }, error) {
		created, err := h.createAPIKey(ctx, in.ID, in.Body.ProjectID, in.Body.Name)
		if err != nil {
			return nil, err
		}
		return &struct{ Body CreatedAPIKey }{Body: *created}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "list-api-keys", Method: "GET", Path: "/orgs/{id}/api-keys",
		Summary: "List an org's API keys", Tags: []string{"accounts"},
		Description: "Newest first, revoked ones included. No token is ever listed. Every member sees them all. An API key sees itself and the keys minted from it.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct {
		Body struct {
			APIKeys []*domain.APIKey `json:"api_keys"`
		}
	}, error) {
		keys, err := h.reachableKeys(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		out := &struct {
			Body struct {
				APIKeys []*domain.APIKey `json:"api_keys"`
			}
		}{}
		out.Body.APIKeys = keys
		return out, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "revoke-api-key", Method: "DELETE", Path: "/api-keys/{id}",
		Summary: "Revoke an API key", Tags: []string{"accounts"},
		Description: "The key and every key minted from it stop working at once. Revoking a revoked key changes nothing. " +
			"An owner revokes any key of the org, a member the keys they created, an API key itself and the keys minted from it.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body domain.APIKey }, error) {
		revoked, err := h.revokeAPIKey(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &struct{ Body domain.APIKey }{Body: *revoked}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "register-cluster", Method: "POST", Path: "/orgs/{id}/clusters",
		Summary: "Register a cluster for an org", Tags: []string{"clusters"},
		Description: "A cluster exists for the platform once its name is registered, and takes the jobs of its org only. " +
			"Returns what its agents are started with: the runtime token and the two addresses they reach the platform at. " +
			"The token is derived from the name, so it is never lost: registering a name the org already owns changes nothing " +
			"and returns the same token and addresses. A name another org owns answers 409.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body nameBody
	}) (*struct{ Body RegisteredCluster }, error) {
		if _, err := h.access.OrgAdmin(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := domain.ValidateIdentifier("cluster name", in.Body.Name); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		cluster, err := h.clusters.RegisterCluster(ctx, in.Body.Name, in.ID)
		if err != nil {
			return nil, err
		}
		token, err := auth.Mint(h.signingSecret, auth.RuntimeScope(cluster.Name))
		if err != nil {
			return nil, err
		}
		return &struct{ Body RegisteredCluster }{Body: RegisteredCluster{
			ClusterView: viewOf(cluster), RuntimeToken: token, APIURL: h.runtimes.APIURL, MetricsURL: h.runtimes.MetricsURL,
		}}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "list-clusters", Method: "GET", Path: "/orgs/{id}/clusters",
		Summary: "List the clusters an org's jobs run on", Tags: []string{"clusters"},
		Description: "The clusters the org registered and the ones the platform shares with every org, by name, with whether an agent is connected right now. " +
			"The org may change only the ones it owns.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct {
		Body struct {
			Clusters []ListedCluster `json:"clusters"`
		}
	}, error) {
		caller, err := h.policy.readOrg(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		clusters, err := h.clusters.ClustersHosting(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		connected, err := h.connected(ctx)
		if err != nil {
			return nil, err
		}
		out := &struct {
			Body struct {
				Clusters []ListedCluster `json:"clusters"`
			}
		}{}
		out.Body.Clusters = make([]ListedCluster, len(clusters))
		for i, c := range clusters {
			out.Body.Clusters[i] = listed(c, in.ID, caller, connected[c.Name])
		}
		return out, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "share-cluster", Method: "PATCH", Path: "/orgs/{id}/clusters/{name}",
		Summary: "Share a cluster with every org, or stop sharing it", Tags: []string{"clusters"},
		Description: "Staff only. A shared cluster takes jobs of every org. When the sharing ends, the jobs of other orgs placed on the cluster " +
			"go back to the queue in the same step and are placed elsewhere: the cluster's runtime stops them, giving each its checkpoint window, " +
			"and none of them spends a retry. requeued names them.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Name string `path:"name"`
		Body struct {
			Shared bool `json:"shared"`
		}
	}) (*struct{ Body WithdrawnCluster }, error) {
		if err := h.access.Staff(ctx); err != nil {
			return nil, err
		}
		if _, err := h.access.ClusterOf(ctx, in.ID, in.Name); err != nil {
			return nil, err
		}
		cluster, requeued, err := h.clusters.SetClusterShared(ctx, in.Name, in.Body.Shared)
		if err != nil {
			return nil, err
		}
		return &struct{ Body WithdrawnCluster }{Body: WithdrawnCluster{ClusterView: viewOf(cluster), Requeued: orEmpty(requeued)}}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "remove-cluster", Method: "DELETE", Path: "/orgs/{id}/clusters/{name}",
		Summary: "Remove a cluster from an org", Tags: []string{"clusters"},
		Description: "Ends the cluster for the platform. Every job placed on it goes back to the queue in the same step, keeping its retries, and is placed " +
			"on another cluster that takes its org. The cluster's settings and the identity bound to it are deleted, and its runtime token is refused from " +
			"then on, so stop its agents. The name is free to register again, by any org. requeued names the jobs.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Name string `path:"name"`
	}) (*struct{ Body WithdrawnCluster }, error) {
		cluster, err := h.access.ClusterOf(ctx, in.ID, in.Name)
		if err != nil {
			return nil, err
		}
		requeued, err := h.clusters.RemoveCluster(ctx, in.Name)
		if err != nil {
			return nil, err
		}
		return &struct{ Body WithdrawnCluster }{Body: WithdrawnCluster{ClusterView: viewOf(cluster), Requeued: orEmpty(requeued)}}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "forget-cluster-identity", Method: "DELETE", Path: "/orgs/{id}/clusters/{name}/identity",
		Summary: "Forget the identity a cluster's runtime reported", Tags: []string{"clusters"},
		Description: "A cluster is bound to the first cluster_id its runtime reports, and a runtime reporting any other is refused. After the machines " +
			"behind a name are rebuilt they report a new one: forget the old identity here and the cluster binds to what its runtime reports next. " +
			"The settings kept under the old identity are deleted with it.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Name string `path:"name"`
	}) (*struct{ Body ClusterView }, error) {
		if _, err := h.access.ClusterOf(ctx, in.ID, in.Name); err != nil {
			return nil, err
		}
		cluster, err := h.clusters.ClearReportedID(ctx, in.Name)
		if err != nil {
			return nil, err
		}
		return &struct{ Body ClusterView }{Body: viewOf(cluster)}, nil
	})
}

func orEmpty(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

func (h *Handler) me(ctx context.Context) (*Me, error) {
	p, err := h.access.Account(ctx)
	if err != nil {
		return nil, err
	}
	me := &Me{Kind: p.Kind, UserID: p.UserID, Name: p.Name, KeyID: p.KeyID}
	if p.Kind != auth.KindStaff {
		limits := h.limits
		me.Limits = &limits
	}
	switch p.Kind {
	case auth.KindStaff:
		if me.Orgs, err = h.accounts.ListAllOrgs(ctx); err != nil {
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
			for _, org := range me.Orgs {
				org.Role = roles[org.ID]
			}
		}
	case auth.KindKey:
		me.ProjectID = p.ProjectID
		if me.Orgs, err = h.accounts.ListOrgs(ctx, p.OrgIDs); err != nil {
			return nil, err
		}
	default:
		if me.Orgs, err = h.accounts.ListUserOrgs(ctx, p.UserID); err != nil {
			return nil, err
		}
	}
	orgIDs := make([]string, len(me.Orgs))
	for i, org := range me.Orgs {
		orgIDs[i] = org.ID
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

func (h *Handler) createAPIKey(ctx context.Context, orgID, projectID, name string) (*CreatedAPIKey, error) {
	p, err := h.policy.mintKey(ctx, orgID, projectID)
	if err != nil {
		return nil, err
	}
	if name, err = validName("api key", name); err != nil {
		return nil, err
	}
	key := &domain.APIKey{OrgID: orgID, ProjectID: projectID, CreatedBy: p.UserID, Name: name}
	if p.Kind == auth.KindKey {
		// The person answerable for a minted key is the one answerable for the key that minted it.
		minter, err := h.accounts.GetAPIKey(ctx, p.KeyID)
		if err != nil {
			return nil, err
		}
		if minter == nil {
			return nil, fmt.Errorf("the calling api key %s has no row", p.KeyID)
		}
		key.CreatedBy, key.MintedBy = minter.CreatedBy, minter.ID
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

func (h *Handler) reachableKeys(ctx context.Context, orgID string) ([]*domain.APIKey, error) {
	if _, err := h.access.OrgMember(ctx, orgID); err != nil {
		return nil, err
	}
	keys, err := h.accounts.ListAPIKeys(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return h.policy.reachableKeys(ctx, orgID, keys)
}

// revokeAPIKey answers for a key the caller cannot reach exactly as for one that does not exist.
func (h *Handler) revokeAPIKey(ctx context.Context, id string) (*domain.APIKey, error) {
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
