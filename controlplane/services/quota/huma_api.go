package quota

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// RegisterHuma registers every quota-service operation (agents,
// platform-experiments, resource catalog) on the shared Huma doc.
func RegisterHuma(doc *apidocs.Doc, h *Handler, peh *PlatformExperimentsHandler) {
	access := doc.Access

	// ---- agents ----

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "register-agent", Method: "POST", Path: "/agents",
		Summary: "Register an agent", Tags: []string{"agents"},
		DefaultStatus: 201,
		Description:   "Registers an agent under an id of your choosing, for one whose standing should carry from one platform experiment to the next. Returns 409 if the id already exists. It joins nothing: POST /platform-experiments/{id}/agents with agent_id does, and that call alone creates agents that need no identity of their own.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			ID    string `json:"id" doc:"Stable unique agent id"`
			Name  string `json:"name,omitempty" required:"false" doc:"Human-readable display name; defaults to id"`
			OrgID string `json:"org_id,omitempty" required:"false" doc:"The org the agent belongs to. Needed only from a token that is not bound to a single org."`
		}
	}) (*struct{ Body *domain.Agent }, error) {
		// Validated here rather than only where it is used: an agent id is pasted verbatim into
		// Kubernetes object names and into the object-store key policy that decides what a
		// job may write. Registration is the one moment it can be refused for free.
		if err := domain.ValidateIdentifier("id", in.Body.ID); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		// id is the only identity that matters; name is presentation-only (UI/leaderboard), so
		// requiring it would reject an otherwise complete registration over a cosmetic field.
		name := in.Body.Name
		if name == "" {
			name = in.Body.ID
		}
		orgID, err := access.Org(ctx, in.Body.OrgID)
		if err != nil {
			return nil, err
		}
		agent, err := h.svc.RegisterAgent(ctx, in.Body.ID, name, orgID)
		if err != nil {
			if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint") {
				return nil, huma.Error409Conflict("agent already exists: " + in.Body.ID)
			}
			return nil, auth.Internal(h.logger, "register agent", err, zap.String("agentID", in.Body.ID))
		}
		return &struct{ Body *domain.Agent }{Body: agent}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "list-agents", Method: "GET", Path: "/agents",
		Summary: "List registered agents", Tags: []string{"agents"},
		Description: "Filter with ?platform_experiment_id (the agents signed up to it) and ?q (search id/name). " +
			"Optional ?limit (default 20, max 200) and ?offset, newest first. Total match count is " +
			"returned in the X-Total-Count response header.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string `query:"platform_experiment_id" doc:"Only the agents signed up to this platform experiment."`
		Search               string `query:"q" doc:"Case-insensitive substring match against id and name."`
		ProjectID            string `query:"project_id" doc:"Only the agents signed up to a platform experiment of this project."`
		Limit                int    `query:"limit" doc:"Page size, default 20, maximum 200. The default is small because the whole response is meant to be read by an agent: ask for more deliberately, using X-Total-Count to decide."`
		Offset               int    `query:"offset" doc:"Rows to skip, for paging through X-Total-Count total matches."`
	}) (*struct {
		Body       []*domain.Agent
		TotalCount int `header:"X-Total-Count"`
	}, error) {
		if in.Limit < 0 || in.Offset < 0 {
			return nil, huma.Error400BadRequest("limit and offset must not be negative")
		}
		scope, err := access.ListScope(ctx, in.ProjectID)
		if err != nil {
			return nil, err
		}
		filter := db.AgentsFilter{PlatformExperimentID: in.PlatformExperimentID, Search: in.Search, Scope: scope, Limit: in.Limit, Offset: in.Offset}
		agents, err := h.svc.store.ListAgents(ctx, filter)
		if err != nil {
			return nil, auth.Internal(h.logger, "list agents", err)
		}
		stats, err := h.svc.store.AgentStats(ctx, filter)
		if err != nil {
			return nil, auth.Internal(h.logger, "list agents", err)
		}
		return &struct {
			Body       []*domain.Agent
			TotalCount int `header:"X-Total-Count"`
		}{Body: agents, TotalCount: stats.Total}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "agent-stats", Method: "GET", Path: "/agents/stats",
		Summary: "Count registered agents, those with a top-3 placement, and their mean performance score", Tags: []string{"agents"},
		Description: "Whole-set figures for the same ?platform_experiment_id and ?q filters GET /agents accepts, " +
			"aggregated in PostgreSQL. A list read is one page, so these cannot be tallied from it.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string `query:"platform_experiment_id" doc:"Only the agents signed up to this platform experiment."`
		Search               string `query:"q" doc:"Case-insensitive substring match against id and name."`
		ProjectID            string `query:"project_id" doc:"Only the agents signed up to a platform experiment of this project."`
	}) (*struct{ Body *domain.AgentStats }, error) {
		scope, err := access.ListScope(ctx, in.ProjectID)
		if err != nil {
			return nil, err
		}
		stats, err := h.svc.store.AgentStats(ctx, db.AgentsFilter{PlatformExperimentID: in.PlatformExperimentID, Search: in.Search, Scope: scope})
		if err != nil {
			return nil, auth.Internal(h.logger, "agent stats", err)
		}
		return &struct{ Body *domain.AgentStats }{Body: stats}, nil
	})

	// ---- platform experiments ----

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-experiment-file", Method: "GET", Path: "/projects/{id}/experiment-file",
		Summary: "Read what a repository's experiment file states", Tags: []string{"platform-experiments"},
		Description: "What " + domain.ExperimentFile + " at the head of the default branch of repository states of a platform experiment: name, description, metrics, " +
			"report_interval_seconds and stages, as POST /platform-experiments with from_repo would take them. Nothing is created. " +
			"The org of the project must reach the repository, with the refusals of that create: 422 no_experiment_file when the repository has none, " +
			"invalid_experiment_file when it cannot be used, and why.",
	}, func(ctx context.Context, in *struct {
		ID         string `path:"id"`
		Repository string `query:"repository" required:"true" doc:"owner/name on GitHub"`
	}) (*struct{ Body *ExperimentFile }, error) {
		if err := access.Project(ctx, in.ID); err != nil {
			return nil, err
		}
		file, err := peh.experimentFile(ctx, in.ID, in.Repository)
		if err != nil {
			return nil, err
		}
		return &struct{ Body *ExperimentFile }{Body: file}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "create-platform-experiment", Method: "POST", Path: "/platform-experiments",
		Summary: "Create a platform experiment", Tags: []string{"platform-experiments"},
		DefaultStatus: 201,
		Description: "Operator/dashboard endpoint. project_id, name, description, metrics and budget_accelerator_hours are required, and so is how long it lasts: " +
			"duration_hours or ends_at, one of the two (422 invalid_length for both or neither). " +
			"max_agents left out is max_hosted, report_interval_seconds left out is the platform's default, and with no starts_at it starts when it is started. " +
			"default_quota_tier controls signups that omit quota_tier and defaults to \"guaranteed\". " +
			"With from_repo: true every field the body leaves out is read from " + domain.ExperimentFile + " at the head of the default branch of " +
			"repository, which the same body names (422 no_repository without it, no_experiment_file when the file is not there), a YAML file shaped like this body. What the body carries wins. budget_accelerator_hours, max_agents, duration_hours and ends_at " +
			"are never read from the file: any agent's token can push to that branch, so what a run costs and how " +
			"long it lasts is stated by whoever creates it. A file that cannot be used answers 422 and names the file. " +
			"coordinator_hosted: true has the platform run the coordinator as one more machine beside the agents', on coordinator_model or the platform's default: " +
			"it holds this platform experiment's coordinator token, is no agent of it, holds no quota and is not ranked, and it is kept until the experiment closes. " +
			"It is not read from the file, and is fixed from then on. " +
			"repository is the code the platform experiment works on, owner/name on GitHub: agents clone it and push their branches to it, jobs check it out, " +
			"A platform experiment keeps its code in a repository: left out, the platform creates a private one with one commit on the GitHub account the org connected, named from the experiment and its id, " +
			"stores it as if the request had named it and answers with it in repository and repository_url. The installation covers a repository it created whichever repositories its owner selected. " +
			"With several accounts connected, repository_account says which, and 422 repository_account_required lists them when it is left out; it is never stated with repository (422 invalid_request). " +
			"422 github_not_connected when the org has connected no GitHub account, 422 repository_not_created when the installation has not granted the App the creation of repositories " +
			"(its owner grants it, or the request names a repository), 409 repository_not_created when the name is taken. Nothing is created without its repository. " +
			"Only a deployment with no GitHub App has platform experiments without one: there agents start in an empty directory and the platform keeps the code each pushes, one git bundle per commit, which is what a job's code_ref then names. With repository, " +
			"and repository_url in the answer is where from. An installation of the platform's GitHub App that the org connected must cover it " +
			"(GET /orgs/{id}/github-repositories lists what may be named): 422 invalid_repository when it is not owner/name, installation_not_connected when " +
			"the org connected no installation on the account that owns it, repository_not_reachable when the installation does not cover it, " +
			"installation_gone when the App was removed on GitHub, and 503 github_not_configured on a deployment with no GitHub App. " +
			"agent_image is the image the hosted agents and the coordinator run, built FROM the platform's agent image so the harness is in it. Left out, " +
			"it is the platform's own. 422 invalid_agent_image when it is no image reference. Neither is read from the file and both are fixed from then on. " +
			"This call creates the experiment and no agent: each agent joins by its own POST /platform-experiments/{id}/agents. " +
			"max_hosted in the answer is how many machines the platform runs for this platform experiment, its hosted agents of every role and its coordinator together. " +
			"GET /platform-experiments/{id} shows the coordinator's launcher, model and what became of its machine.",
	}, func(ctx context.Context, in *struct {
		Body CreatePlatformExperimentRequest
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if in.Body.ProjectID == "" {
			return nil, huma.Error400BadRequest("project_id is required")
		}
		if err := access.Project(ctx, in.Body.ProjectID); err != nil {
			return nil, err
		}
		named, err := peh.repositoryOf(ctx, in.Body.ProjectID, in.Body.Repository, in.Body.RepositoryAccount)
		if err != nil {
			return nil, err
		}
		in.Body.Repository = named
		// Never the experiment file's to state, so it is held before the repository is read.
		if err := in.Body.checkLength(); err != nil {
			return nil, err
		}
		if err := domain.ValidateAgentImage(in.Body.AgentImage); err != nil {
			return nil, api.Refuse(http.StatusUnprocessableEntity, "invalid_agent_image", err.Error())
		}
		// What is wrong with a body laid over the experiment file may be the file's doing, so every
		// such refusal has one status and names the file.
		invalid := func(reason string) error { return huma.Error400BadRequest(reason) }
		if in.Body.FromRepo {
			invalid = func(reason string) error {
				return huma.Error422UnprocessableEntity("with " + domain.ExperimentFile + " of the repository under the request: " + reason)
			}
			if in.Body.Repository == "" {
				return nil, api.Refuse(http.StatusUnprocessableEntity, "no_repository", "from_repo reads "+domain.ExperimentFile+" of the repository the request names, and it names none: set repository")
			}
			named := in.Body.Repository
			body, err := peh.fromRepo(ctx, in.Body)
			body.Repository = named
			if errors.Is(err, ErrInvalidPlatformExperiment) {
				return nil, invalid(err.Error())
			}
			if err != nil {
				return nil, err
			}
			in.Body = body
		} else if missing := in.Body.missing(); len(missing) > 0 {
			return nil, huma.Error422UnprocessableEntity(strings.Join(missing, ", ") + " must be stated: only with from_repo may the repository's experiment file state them")
		}
		if in.Body.Name == "" || in.Body.BudgetAcceleratorHours <= 0 {
			return nil, invalid("name and budget_accelerator_hours are required")
		}
		hosting, err := peh.withHosting(ctx, in.Body)
		if err != nil {
			return nil, peh.refusal("create platform experiment", err)
		}
		if caller, ok := auth.FromContext(ctx); ok {
			by := caller.Actor()
			hosting.createdBy = &by
		}
		hosting.prepare = peh.provideRepository(hosting.Repository, in.Body.RepositoryAccount)
		pe, err := peh.svc.Create(ctx, hosting)
		var answered huma.StatusError
		if errors.As(err, &answered) {
			return nil, err
		}
		if errors.Is(err, ErrInvalidPlatformExperiment) {
			return nil, invalid(err.Error())
		}
		if err != nil {
			return nil, auth.Internal(peh.logger, "create platform experiment", err)
		}
		return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-platform-experiments", Method: "GET", Path: "/platform-experiments",
		Summary: "List platform experiments", Tags: []string{"platform-experiments"},
		Description: "The caller's own platform experiments, within ?project_id when given, and with them the ones " +
			"staff shared with every account: those are in every project's listing, marked shared, read like the " +
			"caller's own and written by their own org alone. ?shared=true keeps only the shared ones, ?shared=false " +
			"leaves them out. A token bound to one platform experiment lists that one. " +
			"Filter with ?status, ?q (search name/description), ?limit, ?offset. " +
			"?sort selects order: created_at, starts_at, name, or status, optionally prefixed with " +
			"- for descending (default -created_at). Total match count is returned in the " +
			"X-Total-Count response header.",
	}, func(ctx context.Context, in *struct {
		Status    string `query:"status" doc:"draft, open, running or closed."`
		Search    string `query:"q" doc:"Case-insensitive substring match against name and description."`
		ProjectID string `query:"project_id" doc:"Only the platform experiments of this project, and the shared ones."`
		Limit     int    `query:"limit" doc:"Page size, default 20, maximum 200. The default is small because the whole response is meant to be read by an agent: ask for more deliberately, using X-Total-Count to decide."`
		Offset    int    `query:"offset" doc:"Rows to skip, for paging through X-Total-Count total matches."`
		Sort      string `query:"sort" doc:"created_at, name or status, optionally prefixed with a minus for descending."`
		Shared    string `query:"shared" enum:"true,false" doc:"true: only the platform experiments staff shared with every account. false: none of them. Left out, the listing holds both."`
	}) (*struct {
		Body       []*domain.PlatformExperiment
		TotalCount int `header:"X-Total-Count"`
	}, error) {
		if in.Status != "" && !domain.ValidPlatformExperimentStatus(domain.PlatformExperimentStatus(in.Status)) {
			return nil, huma.Error400BadRequest("unknown status " + in.Status)
		}
		if !domain.ValidSortField(in.Sort, db.PlatformExperimentSortFields) {
			return nil, huma.Error400BadRequest("unknown sort field " + in.Sort)
		}
		if in.Limit < 0 || in.Offset < 0 {
			return nil, huma.Error400BadRequest("limit and offset must not be negative")
		}
		scope, withShared, err := access.PlatformExperimentListScope(ctx, in.ProjectID)
		if err != nil {
			return nil, err
		}
		filter := db.PlatformExperimentsFilter{
			Scope:      scope,
			WithShared: withShared,
			Status:     in.Status,
			Search:     in.Search,
			Limit:      in.Limit,
			Offset:     in.Offset,
			Sort:       in.Sort,
		}
		if in.Shared != "" {
			only := in.Shared == "true"
			filter.Shared = &only
		}
		pes, err := peh.svc.store.ListPlatformExperiments(ctx, filter)
		if err != nil {
			return nil, auth.Internal(peh.logger, "list platform experiments", err)
		}
		total, err := peh.svc.store.CountPlatformExperiments(ctx, filter)
		if err != nil {
			return nil, auth.Internal(peh.logger, "list platform experiments", err)
		}
		if pes == nil {
			pes = []*domain.PlatformExperiment{}
		}
		return &struct {
			Body       []*domain.PlatformExperiment
			TotalCount int `header:"X-Total-Count"`
		}{Body: pes, TotalCount: total}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-job-cost", Method: "GET", Path: "/experiments/{id}/cost",
		Summary: "Get what a job actually cost", Tags: []string{"platform-experiments"},
		Description: "Actual consumption, not the admission-time estimate: confirmed-alive hours and the " +
			"accelerator/CPU hours billed for them. `settled` distinguishes a final bill from a running total. " +
			"The estimate is returned alongside so the two can be compared.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *JobCost }, error) {
		if err := access.Experiment(ctx, in.ID); err != nil {
			return nil, err
		}
		cost, err := peh.svc.JobCost(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(peh.logger, "get job cost", err, zap.String("id", in.ID))
		}
		if cost == nil {
			return nil, huma.Error404NotFound("experiment not found")
		}
		return &struct{ Body *JobCost }{Body: cost}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment", Method: "GET", Path: "/platform-experiments/{id}",
		Summary: "Get a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Returns status, budget, the stage ladder and the list of signed-up agents. repository is the code it works on, owner/name on GitHub, " +
			"and repository_url the address agents clone and jobs check out. Both are absent when it names none. " +
			"One made from a sentence carries goal, and while it is a draft what its planner proposed (proposal, proposed_at) and what a person asked to change (revisions): " +
			"a draft with no proposed_at is being planned, one with it waits to be accepted.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		pe, err := peh.svc.store.GetPlatformExperiment(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(peh.logger, "get platform experiment", err, zap.String("id", in.ID))
		}
		if pe == nil {
			return nil, huma.Error404NotFound("platform experiment not found")
		}
		signups, err := peh.svc.store.ListSignups(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(peh.logger, "get platform experiment", err, zap.String("id", in.ID))
		}
		pe.SignedUpAgents = signups
		pe.SignupCount = len(signups)
		if err := peh.nameActors(ctx, pe); err != nil {
			return nil, auth.Internal(peh.logger, "get platform experiment", err, zap.String("id", in.ID))
		}
		return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "update-platform-experiment", Method: "PATCH", Path: "/platform-experiments/{id}",
		Summary: "Change fields of a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Takes only the fields that change; a field left out keeps its value, and a body with none answers 422 nothing_to_change. " +
			"While the experiment is open every field of the body can change, `metrics` among them, so a mis-specified ranking metric is fixed in place. " +
			"Once it runs only `name` and `description` do, because admission and the stage ladder have acted on the rest: a body that carries another field answers 409 experiment_running and names it. " +
			"409 experiment_not_editable for a draft or a closed one, 422 invalid_field for a value that cannot be taken. " +
			"A coordinator's token changes `description` only: a body that carries any other field answers 403 coordinator_changes_description_only. " +
			"The project, the repository, the agent image, the stages and the default quota tier are fixed at creation and are no fields of this body.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body PlatformExperimentPatch
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		// A coordinator's token is handed to a machine the platform rents, so it changes the
		// brief and nothing a run's cost or length follows from.
		if caller, _ := auth.FromContext(ctx); caller.Kind == auth.KindCoordinator {
			if others := in.Body.beyondTheBrief(true); len(others) > 0 {
				return nil, api.Refuse(http.StatusForbidden, "coordinator_changes_description_only", "a coordinator's token changes the description and nothing else, and the body also carries: "+strings.Join(others, ", ")+". Leave them out")
			}
		}
		pe, err := peh.svc.Update(ctx, in.ID, in.Body)
		if err != nil {
			return nil, peh.refusal("update platform experiment", err)
		}
		return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment-signup", Method: "GET", Path: "/platform-experiments/{id}/signups/{agent_id}",
		Summary: "Read an agent's signup", Tags: []string{"platform-experiments"},
		Description: "What an agent was signed up as: its role, which decides whether it is ranked and cut, and the flavor, " +
			"model and hyperparameters it runs with. An agent started with its token alone reads its own here. " +
			"404 if that agent is not signed up to this platform experiment — not signed up is not a role.",
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		AgentID string `path:"agent_id"`
	}) (*struct{ Body signupReply }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		signup, err := peh.svc.store.GetAgentSignup(ctx, in.ID, in.AgentID)
		if err != nil {
			return nil, auth.Internal(peh.logger, "get platform experiment signup", err, zap.String("id", in.ID))
		}
		if signup == nil {
			return nil, huma.Error404NotFound("agent is not signed up to this platform experiment")
		}
		return &struct{ Body signupReply }{Body: signupReply{
			AgentID: signup.AgentID, Role: string(signup.Role), Flavor: signup.Flavor, Model: signup.Model, Hyperparameters: signup.Hyperparameters,
			FinishedAt: signup.FinishedAt,
		}}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "start-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/start",
		Summary: "Start a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Closes signup and allocates each signed-up agent its quota, so it can only " +
			"run once and only from status 'open'. Allocation divides the first stage's share of " +
			"the budget across the agents present at this moment -- an agent that has not signed " +
			"up by now gets nothing and cannot join later. Fails if nobody has signed up.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct {
		Body struct {
			Status string               `json:"status"`
			Quotas []*domain.AgentQuota `json:"quotas"`
		}
	}, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		quotas, err := peh.svc.Start(ctx, in.ID)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		out := &struct {
			Body struct {
				Status string               `json:"status"`
				Quotas []*domain.AgentQuota `json:"quotas"`
			}
		}{}
		out.Body.Status = "running"
		out.Body.Quotas = quotas
		return out, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "close-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/close",
		Summary: "Close a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Terminal: every still-running job is evicted (reason experiment_closed) and " +
			"no further submission is accepted. Optional top_results records the final standings; " +
			"omitted, standings are derived from observed metrics. The experiment then reads close_reason closed_by_request with closed_by, " +
			"where one the platform closed at ends_at reads end_time_reached. Safe to call twice -- a second " +
			"call reports invalid_transition rather than changing anything.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body *struct {
			TopResults []AgentResult `json:"top_results,omitempty" doc:"Best-first final standings (up to 3). Omit to derive standings from the metrics store."`
		}
	}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		var topResults []AgentResult
		if in.Body != nil {
			topResults = in.Body.TopResults
		}
		caller, _ := auth.FromContext(ctx)
		by := caller.Actor()
		if err := peh.svc.Close(ctx, in.ID, &by, topResults); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		out := &struct {
			Body struct {
				Status string `json:"status"`
			}
		}{}
		out.Body.Status = "closed"
		return out, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment-results", Method: "GET", Path: "/platform-experiments/{id}/results",
		Summary: "Final standings and operator summary", Tags: []string{"platform-experiments"},
		Description: "Per-metric standings, best-first, derived from the metrics store on every read " +
			"(never frozen into PostgreSQL), plus the operator's narrative summary. bests lists, for every " +
			"declared metric whatever its role, each reporting agent's best raw value and the job that set it, unranked.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *PlatformExperimentResults }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		results, err := peh.svc.Results(ctx, in.ID)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		return &struct{ Body *PlatformExperimentResults }{Body: results}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, apidocs.AfterClose(huma.Operation{
		OperationID: "set-platform-experiment-summary", Method: "POST", Path: "/platform-experiments/{id}/summary",
		Summary: "Record the operator's narrative verdict on a run", Tags: []string{"platform-experiments"},
		Description: "Writable after the experiment closes — this is the one field whose whole point " +
			"is to be written once the run is over and the results are in.",
	}), func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			Summary string `json:"summary"`
		}
	}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := peh.svc.SetSummary(ctx, in.ID, in.Body.Summary); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		out := &struct {
			Body struct {
				Status string `json:"status"`
			}
		}{}
		out.Body.Status = "summary_recorded"
		return out, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-platform-experiment-quotas", Method: "GET", Path: "/platform-experiments/{id}/quotas",
		Summary: "List per-agent quotas for a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Guaranteed/burst accelerator-hours, used vs available, per signed-up agent.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body []*domain.AgentQuota }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		quotas, err := peh.svc.ListQuotas(ctx, in.ID)
		if errors.Is(err, ErrPlatformExperimentNotFound) {
			return nil, huma.Error404NotFound(err.Error())
		}
		if err != nil {
			return nil, auth.Internal(peh.logger, "list platform experiment quotas", err, zap.String("id", in.ID))
		}
		if quotas == nil {
			quotas = []*domain.AgentQuota{}
		}
		return &struct{ Body []*domain.AgentQuota }{Body: quotas}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-agent-quota", Method: "GET", Path: "/platform-experiments/{id}/quotas/{agent_id}",
		Summary: "Get one agent's quota within a platform experiment", Tags: []string{"platform-experiments"},
		Description: "One row of what GET /platform-experiments/{id}/quotas returns for every agent.",
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		AgentID string `path:"agent_id"`
	}) (*struct{ Body *domain.AgentQuota }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		aq, err := peh.svc.GetAgentQuota(ctx, in.AgentID, in.ID)
		if err != nil {
			return nil, auth.Internal(peh.logger, "get agent quota", err, zap.String("id", in.ID))
		}
		if aq == nil {
			return nil, huma.Error404NotFound("agent " + in.AgentID + " has no quota in platform experiment " + in.ID + ": quota is granted when the experiment starts, to the agents signed up to it")
		}
		return &struct{ Body *domain.AgentQuota }{Body: aq}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-stages", Method: "GET", Path: "/platform-experiments/{id}/stages",
		Summary: "Get the stage ladder and current standing of a platform experiment", Tags: []string{"platform-experiments"},
		Description: "The ladder, which stage is running, progress through it, and which signed-up agents are still active vs cut. Each stage carries max_job_hours: the longest one job may run while it is current (0/absent = unlimited). Where the next cut falls is not shown; every agent's best value is, at GET /platform-experiments/{id}/results.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body StagesStatusResponse }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		pe, err := peh.svc.store.GetPlatformExperiment(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(peh.logger, "get stages", err, zap.String("id", in.ID))
		}
		if pe == nil {
			return nil, huma.Error404NotFound("platform experiment not found")
		}
		cuts, err := peh.svc.store.ListCutAgents(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(peh.logger, "get stages", err, zap.String("id", in.ID))
		}
		if cuts == nil {
			cuts = []domain.AgentCut{}
		}
		cutSet := make(map[string]bool, len(cuts))
		for _, c := range cuts {
			cutSet[c.AgentID] = true
		}
		advances, err := peh.svc.store.ListStageAdvances(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(peh.logger, "get stages", err, zap.String("id", in.ID))
		}
		if advances == nil {
			advances = []domain.StageAdvance{}
		}
		allSignups, err := peh.svc.store.ListParticipants(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(peh.logger, "get stages", err, zap.String("id", in.ID))
		}
		activeAgents := []string{}
		for _, a := range allSignups {
			if !cutSet[a] {
				activeAgents = append(activeAgents, a)
			}
		}
		progress, err := peh.svc.stageProgress(ctx, pe)
		if err != nil {
			return nil, auth.Internal(peh.logger, "get stages", err, zap.String("id", in.ID))
		}
		return &struct{ Body StagesStatusResponse }{Body: StagesStatusResponse{
			Stages:               pe.Stages,
			CurrentStage:         pe.CurrentStage,
			Progress:             progress,
			NextBoundaryProgress: domain.BoundaryProgress(pe.Stages, pe.CurrentStage),
			Advances:             advances,
			ActiveAgents:         activeAgents,
			CutAgents:            cuts,
		}}, nil
	})

	// ---- resource catalog ----

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-resource-catalog", Method: "GET", Path: "/resource-catalog",
		Summary: "Get accelerator/CPU/RAM/storage billing rates", Tags: []string{"resource-catalog"},
		Description: "Static reference data: which accelerator types exist and their billing rates. Listed here does NOT mean any cluster has one free — see /resource-catalog/capacity.",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body ResourceCatalog }, error) {
		return &struct{ Body ResourceCatalog }{Body: peh.catalog}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-resource-capacity", Method: "GET", Path: "/resource-catalog/capacity",
		Summary: "Get live per-cluster accelerator capacity", Tags: []string{"resource-catalog"},
		Description: "Live {accelerator_type, available, total} per cluster the jobs of one org can be placed on: the org's own " +
			"clusters and the ones the platform shares, without the ones the org turned off for its jobs. The org is the one your token is bound to: every API key, agent and " +
			"coordinator token has exactly one, and so does a person in one org. A person in several orgs names it with org_id, " +
			"and is answered 400 without it. An org you do not reach is not found. Staff read one org with org_id and every " +
			"cluster that reports without it. accelerator_type is the exact " +
			"driver-published string a job spec takes, so what you read here is what you submit. Check before " +
			"choosing: a type with no live capacity anywhere QUEUES FOREVER and never errors. Only " +
			"currently-schedulable nodes count — powered-off hardware never appears."}, func(ctx context.Context, in *struct {
		OrgID string `query:"org_id" doc:"The org whose clusters to read. Required only when your token reaches more than one org."`
	}) (*struct {
		Body struct {
			Clusters  []ClusterCapacity `json:"clusters"`
			JobImages []domain.JobImage `json:"job_images" doc:"The images the deployment offers a job to run in, each with what it carries. A capacity line names the ones that suit its type. A job may name any other image."`
		}
	}, error) {
		orgID, every, err := access.PlacementOrg(ctx, in.OrgID)
		if err != nil {
			return nil, err
		}
		clusters, err := peh.liveCapacity(ctx, orgID, every)
		if err != nil {
			return nil, auth.Internal(peh.logger, "get resource capacity", err)
		}
		out := &struct {
			Body struct {
				Clusters  []ClusterCapacity `json:"clusters"`
				JobImages []domain.JobImage `json:"job_images" doc:"The images the deployment offers a job to run in, each with what it carries. A capacity line names the ones that suit its type. A job may name any other image."`
			}
		}{}
		out.Body.Clusters = clusters
		out.Body.JobImages = peh.catalog.JobImages
		if out.Body.JobImages == nil {
			out.Body.JobImages = []domain.JobImage{}
		}
		return out, nil
	})
}

// StagesStatusResponse is the response body for GET /platform-experiments/{id}/stages.
// Boundaries are published ahead so agents can plan. The cut line is not here; the standings
// are public at GET /platform-experiments/{id}/results.
type StagesStatusResponse struct {
	Stages       []domain.Stage `json:"stages"`
	CurrentStage int            `json:"current_stage"`
	// Progress is max(budget_consumed / total_budget, elapsed / duration) — the single clock
	// driving every boundary.
	Progress             float64 `json:"progress"`
	NextBoundaryProgress float64 `json:"next_boundary_progress"`
	// Advances records each boundary already crossed and when.
	Advances     []domain.StageAdvance `json:"advances"`
	ActiveAgents []string              `json:"active_agents"`
	CutAgents    []domain.AgentCut     `json:"cut_agents"`
}

// signupReply is an agent's signup as any caller in its platform experiment reads it.
type signupReply struct {
	AgentID         string         `json:"agent_id"`
	Role            string         `json:"role"`
	Flavor          string         `json:"flavor"`
	Model           string         `json:"model"`
	Hyperparameters map[string]any `json:"hyperparameters"`
	FinishedAt      *time.Time     `json:"finished_at,omitempty" doc:"When the agent said it has nothing left to do."`
}
