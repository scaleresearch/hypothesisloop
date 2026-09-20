// Package accounts serves who exists and what they own: orgs, projects, API keys and clusters.
// Who may call what is decided in policy.go and nowhere else.
package accounts

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Accounts is the persistence the handler needs for orgs, projects and API keys.
// *db.AccountsStore satisfies it.
type Accounts interface {
	RoleReader
	CreateOrg(ctx context.Context, name, ownerUserID string) (*domain.Org, error)
	RenameOrg(ctx context.Context, id, name string) error
	GetOrg(ctx context.Context, id string) (*domain.Org, error)
	ListOrgs(ctx context.Context, orgIDs []string) ([]*domain.Org, error)
	ListAllOrgs(ctx context.Context) ([]*domain.Org, error)
	ListUserOrgs(ctx context.Context, userID string) ([]*domain.Org, error)
	CreateProject(ctx context.Context, orgID, name string) (*domain.Project, error)
	RenameProject(ctx context.Context, id, name string) error
	GetProject(ctx context.Context, id string) (*domain.Project, error)
	ListProjects(ctx context.Context, orgIDs []string) ([]*domain.Project, error)
	CreateAPIKey(ctx context.Context, key *domain.APIKey, keyHash string) error
	ListAPIKeys(ctx context.Context, orgID string) ([]*domain.APIKey, error)
	GetAPIKey(ctx context.Context, id string) (*domain.APIKey, error)
	RevokeAPIKey(ctx context.Context, id string) error
}

// Clusters is the persistence the handler needs for cluster registration. *db.ClustersStore
// satisfies it.
type Clusters interface {
	RegisterCluster(ctx context.Context, name, orgID string) (*domain.Cluster, error)
	GetCluster(ctx context.Context, name string) (*domain.Cluster, error)
	ListClusters(ctx context.Context, orgIDs []string) ([]*domain.Cluster, error)
	SetClusterShared(ctx context.Context, name string, shared bool) (*domain.Cluster, error)
}

// Connectivity reports which clusters have an agent connected right now, by name.
type Connectivity func(ctx context.Context) (map[string]bool, error)

// maxNameLength bounds the display name of an org, a project and an API key.
const maxNameLength = 100

type Handler struct {
	accounts      Accounts
	clusters      Clusters
	connected     Connectivity
	signingSecret string
	policy        policy
}

func NewHandler(accounts Accounts, clusters Clusters, connected Connectivity, signingSecret string) *Handler {
	return &Handler{accounts: accounts, clusters: clusters, connected: connected, signingSecret: signingSecret, policy: policy{roles: accounts}}
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
	OrgID     string    `json:"org_id"`
	Shared    bool      `json:"shared" doc:"The platform places every org's jobs on a shared cluster. Staff decide it."`
	ClusterID string    `json:"cluster_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func viewOf(c *domain.Cluster) ClusterView {
	return ClusterView{Name: c.Name, OrgID: c.OrgID, Shared: c.Shared, ClusterID: c.ReportedID, CreatedAt: c.CreatedAt}
}

// RegisteredCluster is a cluster with the token its agents present.
type RegisteredCluster struct {
	ClusterView
	RuntimeToken string `json:"runtime_token" doc:"The token this cluster's agents start with. Derived from the name, so registering again returns the same one."`
}

// ListedCluster is a registered cluster and whether its agent is connected right now.
type ListedCluster struct {
	ClusterView
	Connected bool `json:"connected"`
}

type nameBody struct {
	Name string `json:"name"`
}

func validName(what, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", huma.Error400BadRequest(what + " name is required")
	}
	if len(name) > maxNameLength {
		return "", huma.Error400BadRequest(what + " name is longer than 100 bytes")
	}
	return name, nil
}

// storeError answers for what the stores refuse; anything else is the platform failing.
func storeError(err error) error {
	switch {
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
	case errors.Is(err, db.ErrProjectNameTaken):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, db.ErrClusterNameTaken):
		return huma.Error409Conflict(err.Error())
	default:
		return huma.Error500InternalServerError(err.Error())
	}
}

// RegisterHuma registers the accounts operations on the shared API document.
func RegisterHuma(doc *apidocs.Doc, h *Handler) {
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-me", Method: "GET", Path: "/me",
		Summary: "Who this token is and what it reaches", Tags: []string{"accounts"},
		Description: "The caller with the orgs and projects it reaches. A person sees the orgs they belong to with their role, " +
			"an API key its org and the projects it is bound to, staff every org.",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body Me }, error) {
		me, err := h.me(ctx)
		if err != nil {
			return nil, err
		}
		return &struct{ Body Me }{Body: *me}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "create-org", Method: "POST", Path: "/orgs", DefaultStatus: http.StatusCreated,
		Summary: "Create an org", Tags: []string{"accounts"},
		Description: "The org starts with its creator as owner and a project named default. A person creates an org for " +
			"themselves. Staff name the owner in owner_user_id. An API key cannot create an org.",
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
		org, err := h.accounts.CreateOrg(ctx, name, owner)
		if err != nil {
			return nil, storeError(err)
		}
		return &struct{ Body domain.Org }{Body: *org}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "rename-org", Method: "PATCH", Path: "/orgs/{id}",
		Summary: "Rename an org", Tags: []string{"accounts"},
		Description: "The id stays. Two orgs may carry the same name.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body nameBody
	}) (*struct{ Body domain.Org }, error) {
		if _, err := h.policy.administerOrg(ctx, in.ID); err != nil {
			return nil, err
		}
		name, err := validName("org", in.Body.Name)
		if err != nil {
			return nil, err
		}
		if err := h.accounts.RenameOrg(ctx, in.ID, name); err != nil {
			return nil, storeError(err)
		}
		org, err := h.accounts.GetOrg(ctx, in.ID)
		if err != nil {
			return nil, storeError(err)
		}
		if org == nil {
			return nil, huma.Error404NotFound("org not found")
		}
		return &struct{ Body domain.Org }{Body: *org}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
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
		if _, err := h.policy.inOrg(ctx, in.ID); err != nil {
			return nil, err
		}
		projects, err := h.accounts.ListProjects(ctx, []string{in.ID})
		if err != nil {
			return nil, storeError(err)
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

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "create-project", Method: "POST", Path: "/orgs/{id}/projects", DefaultStatus: http.StatusCreated,
		Summary: "Create a project in an org", Tags: []string{"accounts"},
		Description: "A project groups platform experiments. Its name is unique within the org: a taken name answers 409.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body nameBody
	}) (*struct{ Body domain.Project }, error) {
		if _, err := h.policy.administerOrg(ctx, in.ID); err != nil {
			return nil, err
		}
		name, err := validName("project", in.Body.Name)
		if err != nil {
			return nil, err
		}
		project, err := h.accounts.CreateProject(ctx, in.ID, name)
		if err != nil {
			return nil, storeError(err)
		}
		return &struct{ Body domain.Project }{Body: *project}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "rename-project", Method: "PATCH", Path: "/projects/{id}",
		Summary: "Rename a project", Tags: []string{"accounts"},
		Description: "The id stays, so platform experiments and API keys of the project are untouched. A name another project of the org carries answers 409.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body nameBody
	}) (*struct{ Body domain.Project }, error) {
		if _, err := h.policy.caller(ctx); err != nil {
			return nil, err
		}
		project, err := h.accounts.GetProject(ctx, in.ID)
		if err != nil {
			return nil, storeError(err)
		}
		// A project of an org the caller is outside of answers as one that does not exist.
		if project == nil {
			return nil, huma.Error404NotFound("project not found")
		}
		if _, err := h.policy.inOrg(ctx, project.OrgID); err != nil {
			return nil, huma.Error404NotFound("project not found")
		}
		if _, err := h.policy.administerOrg(ctx, project.OrgID); err != nil {
			return nil, err
		}
		name, err := validName("project", in.Body.Name)
		if err != nil {
			return nil, err
		}
		if err := h.accounts.RenameProject(ctx, in.ID, name); err != nil {
			return nil, storeError(err)
		}
		project.Name = name
		return &struct{ Body domain.Project }{Body: *project}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "create-api-key", Method: "POST", Path: "/orgs/{id}/api-keys", DefaultStatus: http.StatusCreated,
		Summary: "Create an API key", Tags: []string{"accounts"},
		Description: "Returns the token once. With project_id the key reaches that project only, without it the whole org. " +
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

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "list-api-keys", Method: "GET", Path: "/orgs/{id}/api-keys",
		Summary: "List an org's API keys", Tags: []string{"accounts"},
		Description: "Newest first, revoked ones included. No token is ever listed. An API key sees itself and the keys minted from it.",
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

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "revoke-api-key", Method: "DELETE", Path: "/api-keys/{id}",
		Summary: "Revoke an API key", Tags: []string{"accounts"},
		Description: "The key and every key minted from it stop working at once. Revoking a revoked key changes nothing.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body domain.APIKey }, error) {
		revoked, err := h.revokeAPIKey(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &struct{ Body domain.APIKey }{Body: *revoked}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "register-cluster", Method: "POST", Path: "/orgs/{id}/clusters",
		Summary: "Register a cluster for an org", Tags: []string{"clusters"},
		Description: "A cluster exists for the platform once its name is registered, and takes the jobs of its org only. " +
			"Returns the runtime token its agents start with. Registering a name the org already owns returns the same " +
			"token; a name another org owns answers 409.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body nameBody
	}) (*struct{ Body RegisteredCluster }, error) {
		if _, err := h.policy.administerOrg(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := domain.ValidateIdentifier("cluster name", in.Body.Name); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		cluster, err := h.clusters.RegisterCluster(ctx, in.Body.Name, in.ID)
		if err != nil {
			return nil, storeError(err)
		}
		token, err := auth.Mint(h.signingSecret, auth.RuntimeScope(cluster.Name))
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body RegisteredCluster }{Body: RegisteredCluster{ClusterView: viewOf(cluster), RuntimeToken: token}}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "list-clusters", Method: "GET", Path: "/orgs/{id}/clusters",
		Summary: "List an org's clusters", Tags: []string{"clusters"},
		Description: "The clusters the org registered, by name, with whether an agent is connected right now and whether the platform shares the cluster with every org.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct {
		Body struct {
			Clusters []ListedCluster `json:"clusters"`
		}
	}, error) {
		if _, err := h.policy.readOrg(ctx, in.ID); err != nil {
			return nil, err
		}
		clusters, err := h.clusters.ListClusters(ctx, []string{in.ID})
		if err != nil {
			return nil, storeError(err)
		}
		connected, err := h.connected(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out := &struct {
			Body struct {
				Clusters []ListedCluster `json:"clusters"`
			}
		}{}
		out.Body.Clusters = make([]ListedCluster, len(clusters))
		for i, c := range clusters {
			out.Body.Clusters[i] = ListedCluster{ClusterView: viewOf(c), Connected: connected[c.Name]}
		}
		return out, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "share-cluster", Method: "PATCH", Path: "/orgs/{id}/clusters/{name}",
		Summary: "Share a cluster with every org, or stop sharing it", Tags: []string{"clusters"},
		Description: "Staff only. A shared cluster takes jobs of every org. Jobs already placed on a cluster stay where they are when it stops being shared.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Name string `path:"name"`
		Body struct {
			Shared bool `json:"shared"`
		}
	}) (*struct{ Body ClusterView }, error) {
		if err := h.policy.staff(ctx); err != nil {
			return nil, err
		}
		cluster, err := h.clusters.GetCluster(ctx, in.Name)
		if err != nil {
			return nil, storeError(err)
		}
		if cluster == nil || cluster.OrgID != in.ID {
			return nil, huma.Error404NotFound("cluster not found")
		}
		if cluster, err = h.clusters.SetClusterShared(ctx, in.Name, in.Body.Shared); err != nil {
			return nil, storeError(err)
		}
		return &struct{ Body ClusterView }{Body: viewOf(cluster)}, nil
	})
}

func (h *Handler) me(ctx context.Context) (*Me, error) {
	p, err := h.policy.caller(ctx)
	if err != nil {
		return nil, err
	}
	me := &Me{Kind: p.Kind, UserID: p.UserID, Name: p.Name, KeyID: p.KeyID}
	switch p.Kind {
	case auth.KindStaff:
		if me.Orgs, err = h.accounts.ListAllOrgs(ctx); err != nil {
			return nil, storeError(err)
		}
		if p.UserID != "" {
			own, err := h.accounts.ListUserOrgs(ctx, p.UserID)
			if err != nil {
				return nil, storeError(err)
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
			return nil, storeError(err)
		}
	default:
		if me.Orgs, err = h.accounts.ListUserOrgs(ctx, p.UserID); err != nil {
			return nil, storeError(err)
		}
	}
	orgIDs := make([]string, len(me.Orgs))
	for i, org := range me.Orgs {
		orgIDs[i] = org.ID
	}
	projects, err := h.accounts.ListProjects(ctx, orgIDs)
	if err != nil {
		return nil, storeError(err)
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
			return nil, storeError(err)
		}
		if minter == nil {
			return nil, huma.Error500InternalServerError("the calling api key " + p.KeyID + " has no row")
		}
		key.CreatedBy, key.MintedBy = minter.CreatedBy, minter.ID
	}
	id, token, err := auth.NewAPIKey()
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	key.ID = id
	if err := h.accounts.CreateAPIKey(ctx, key, auth.HashAPIKey(token)); err != nil {
		return nil, storeError(err)
	}
	return &CreatedAPIKey{APIKey: *key, Token: token}, nil
}

func (h *Handler) reachableKeys(ctx context.Context, orgID string) ([]*domain.APIKey, error) {
	if _, err := h.policy.inOrg(ctx, orgID); err != nil {
		return nil, err
	}
	keys, err := h.accounts.ListAPIKeys(ctx, orgID)
	if err != nil {
		return nil, storeError(err)
	}
	return h.policy.reachableKeys(ctx, orgID, keys)
}

// revokeAPIKey answers for a key the caller cannot reach exactly as for one that does not exist.
func (h *Handler) revokeAPIKey(ctx context.Context, id string) (*domain.APIKey, error) {
	if _, err := h.policy.caller(ctx); err != nil {
		return nil, err
	}
	notFound := huma.Error404NotFound("api key not found")
	key, err := h.accounts.GetAPIKey(ctx, id)
	if err != nil {
		return nil, storeError(err)
	}
	if key == nil {
		return nil, notFound
	}
	reachable, err := h.reachableKeys(ctx, key.OrgID)
	if err != nil {
		var refusal huma.StatusError
		if errors.As(err, &refusal) && refusal.GetStatus() == http.StatusNotFound {
			return nil, notFound
		}
		return nil, err
	}
	for _, k := range reachable {
		if k.ID != id {
			continue
		}
		if err := h.accounts.RevokeAPIKey(ctx, id); err != nil {
			return nil, storeError(err)
		}
		revoked, err := h.accounts.GetAPIKey(ctx, id)
		if err != nil {
			return nil, storeError(err)
		}
		if revoked == nil {
			return nil, notFound
		}
		return revoked, nil
	}
	return nil, notFound
}
