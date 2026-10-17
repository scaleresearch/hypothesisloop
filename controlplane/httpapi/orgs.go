package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
)

// accountsAPI serves who exists and what they own: orgs, their members and invites, projects,
// API keys and clusters.
type accountsAPI struct {
	accounts *accounts.Service
}

type nameBody struct {
	Name string `json:"name"`
}

// accountsRoute registers one accounts operation and passes every error it returns through
// accounts.Service.Answer.
func accountsRoute[I, O any](doc *Doc, svc *accounts.Service, op huma.Operation, handle func(context.Context, *I) (*O, error)) {
	register(doc, auth.AudienceCoordinator, op, func(ctx context.Context, in *I) (*O, error) {
		out, err := handle(ctx, in)
		if err != nil {
			return nil, rejected(svc.Answer(op.OperationID, err))
		}
		return out, nil
	})
}

// registerAccounts registers the accounts operations on the shared API document.
func registerAccounts(doc *Doc, svc *accounts.Service) {
	a := accountsAPI{accounts: svc}
	a.registerMembers(doc)
	accountsRoute(doc, svc, huma.Operation{
		OperationID: "get-me", Method: "GET", Path: "/me",
		Summary: "Who this token is and what it reaches", Tags: []string{"accounts"},
		Description: "The caller with the orgs and projects it reaches. A person sees the orgs they belong to with their role, " +
			"an API key its org and the projects it is bound to, staff every org. limits is what the caller may create.",
	}, a.me)
	accountsRoute(doc, svc, huma.Operation{
		OperationID: "create-org", Method: "POST", Path: "/orgs", DefaultStatus: http.StatusCreated,
		Summary: "Create an org", Tags: []string{"accounts"},
		Description: "The org starts with its creator as owner and a project named Default project. A person creates an org for " +
			"themselves. Staff name the owner in owner_user_id. An API key cannot create an org. " +
			"A person owns a bounded number of orgs, their first one included: past it the create answers 409 with reason max_orgs_reached. " +
			"GET /me states the bound in limits. Staff are held to none, whoever they create for.",
	}, a.createOrg)
	accountsRoute(doc, svc, huma.Operation{
		OperationID: "rename-org", Method: "PATCH", Path: "/orgs/{id}",
		Summary: "Rename an org", Tags: []string{"accounts"},
		Description: "The id stays. Two orgs may carry the same name.",
	}, a.renameOrg)
	accountsRoute(doc, svc, huma.Operation{
		OperationID: "list-projects", Method: "GET", Path: "/orgs/{id}/projects",
		Summary: "List an org's projects", Tags: []string{"accounts"},
		Description: "Oldest first. An API key bound to one project sees that project only.",
	}, a.listProjects)
	accountsRoute(doc, svc, huma.Operation{
		OperationID: "create-project", Method: "POST", Path: "/orgs/{id}/projects", DefaultStatus: http.StatusCreated,
		Summary: "Create a project in an org", Tags: []string{"accounts"},
		Description: "A project groups platform experiments. Its name is unique within the org: a taken name answers 409. " +
			"An org holds a bounded number of projects, its default one included: past it the create answers 409 with reason max_projects_reached. " +
			"GET /me states the bound in limits. Staff are held to none.",
	}, a.createProject)
	accountsRoute(doc, svc, huma.Operation{
		OperationID: "rename-project", Method: "PATCH", Path: "/projects/{id}",
		Summary: "Rename a project", Tags: []string{"accounts"},
		Description: "The id stays, so platform experiments and API keys of the project are untouched. A name another project of the org carries answers 409.",
	}, a.renameProject)
	a.registerAPIKeys(doc)
	a.registerClusters(doc)
}

func (a accountsAPI) me(ctx context.Context, _ *struct{}) (*struct{ Body accounts.Me }, error) {
	me, err := a.accounts.Me(ctx)
	if err != nil {
		return nil, err
	}
	return &struct{ Body accounts.Me }{Body: *me}, nil
}

type createOrgRequest struct {
	Name        string `json:"name"`
	OwnerUserID string `json:"owner_user_id,omitempty" doc:"Staff only: the user who owns the org."`
}

func (a accountsAPI) createOrg(ctx context.Context, in *struct {
	Body createOrgRequest
}) (*struct{ Body domain.Org }, error) {
	org, err := a.accounts.CreateOrg(ctx, in.Body.OwnerUserID, in.Body.Name)
	if err != nil {
		return nil, err
	}
	return &struct{ Body domain.Org }{Body: *org}, nil
}

func (a accountsAPI) renameOrg(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body nameBody
}) (*struct{ Body domain.Org }, error) {
	org, err := a.accounts.RenameOrg(ctx, in.ID, in.Body.Name)
	if err != nil {
		return nil, err
	}
	return &struct{ Body domain.Org }{Body: *org}, nil
}

type projectList struct {
	Projects []*domain.Project `json:"projects"`
}

func (a accountsAPI) listProjects(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body projectList }, error) {
	projects, err := a.accounts.Projects(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	return &struct{ Body projectList }{Body: projectList{Projects: projects}}, nil
}

func (a accountsAPI) createProject(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body nameBody
}) (*struct{ Body domain.Project }, error) {
	project, err := a.accounts.CreateProject(ctx, in.ID, in.Body.Name)
	if err != nil {
		return nil, err
	}
	return &struct{ Body domain.Project }{Body: *project}, nil
}

func (a accountsAPI) renameProject(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body nameBody
}) (*struct{ Body domain.Project }, error) {
	project, err := a.accounts.RenameProject(ctx, in.ID, in.Body.Name)
	if err != nil {
		return nil, err
	}
	return &struct{ Body domain.Project }{Body: *project}, nil
}
