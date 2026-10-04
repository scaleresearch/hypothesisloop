package quota

import (
	"context"
	"errors"
	"net/http"
	"strconv"
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
		Description:   "Registers an agent under an id of your choosing, for one whose standing should carry from one platform experiment to the next. 409 agent_exists if the id already exists. It joins nothing: POST /platform-experiments/{id}/agents with agent_id does, and that call alone creates agents that need no identity of their own.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			ID    string `json:"id" doc:"Stable unique agent id"`
			Name  string `json:"name" minLength:"1" doc:"The name the agent is shown under and its branches carry."`
			OrgID string `json:"org_id,omitempty" required:"false" doc:"The org the agent belongs to. Needed only from a token that is not bound to a single org."`
		}
	}) (*struct{ Body *domain.Agent }, error) {
		// Validated here rather than only where it is used: an agent id is pasted verbatim into
		// Kubernetes object names and into the object-store key policy that decides what a
		// job may write. Registration is the one moment it can be refused for free.
		if err := domain.ValidateIdentifier("id", in.Body.ID); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		orgID, err := access.Org(ctx, in.Body.OrgID)
		if err != nil {
			return nil, err
		}
		agent, err := h.svc.RegisterAgent(ctx, in.Body.ID, in.Body.Name, orgID)
		if errors.Is(err, db.ErrAgentExists) {
			return nil, api.Refuse(http.StatusConflict, "agent_exists", "an agent with id "+in.Body.ID+" already exists: register under another id, or join with this one")
		}
		if err != nil {
			return nil, auth.Internal(h.logger, "register agent", err, zap.String("agentID", in.Body.ID))
		}
		return &struct{ Body *domain.Agent }{Body: agent}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "list-agents", Method: "GET", Path: "/agents",
		Summary: "List registered agents", Tags: []string{"agents"},
		Description: "Filter with ?platform_experiment_id (the agents signed up to it) and ?q (search id/name). " +
			"Optional ?limit (default 20, max 200, 422 invalid_page over it) and ?offset, newest first. Total match count is " +
			"returned in the X-Total-Count response header.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string `query:"platform_experiment_id" doc:"Only the agents signed up to this platform experiment."`
		Search               string `query:"q" doc:"Case-insensitive substring match against id and name."`
		ProjectID            string `query:"project_id" doc:"Only the agents signed up to a platform experiment of this project."`
		Limit                int    `query:"limit" doc:"Page size, default 20, maximum 200: a larger one is refused with 422 invalid_page. The default is small because the whole response is meant to be read by an agent: ask for more deliberately, using X-Total-Count to decide."`
		Offset               int    `query:"offset" doc:"Rows to skip, for paging through X-Total-Count total matches."`
	}) (*struct {
		Body       []*domain.Agent
		TotalCount int `header:"X-Total-Count"`
	}, error) {
		if in.Limit < 0 || in.Limit > db.MaxAgentListLimit || in.Offset < 0 {
			return nil, api.Refuse(http.StatusUnprocessableEntity, "invalid_page", "limit must be 1 to "+strconv.Itoa(db.MaxAgentListLimit)+" and offset must not be negative")
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
		Description: "Operator/dashboard endpoint. project_id, name, description, metrics, budget_accelerator_hours, max_agents and duration_hours are required. " +
			"max_agents is how many researchers (agents of role competitor, whatever their flavor) may join over its whole life, and counts nobody else: no agent of role baseline, no reviewer, no coordinator. " +
			"duration_hours is how long the run lasts from the moment it starts, at most the platform's quota.max_platform_experiment_days (422 invalid_length without it, run_too_long over that). The answer carries no ends_at: the run ends duration_hours after it starts. " +
			"Created, it is open until its baseline is declared and the platform starts the run, and baseline_due_at in the answer is until when: planning.baseline_limit_minutes after starts_at, which is this create or a later one the body states. One not running by then is closed with close_reason baseline_limit_reached. " +
			"Three fields left out are the platform's settings: report_interval_seconds is scheduler.default_report_interval_seconds, stages is stages.default, as one stage of its whole length when max_agents is below 5, since a boundary cuts nobody from fewer researchers (more than one stage stated for such a field is refused invalid_stages), and " +
			"default_quota_tier, which controls signups that omit quota_tier, is quota.default_quota_tier. " +
			"The platform starts the run, and nothing else does: once a baseline is declared (PATCH baseline_job_id), at starts_at or at once without one or with one past. Until then it is open and GET /platform-experiments/{id} says in status_reason what it waits for. " +
			"What it is created as is checked by the rules a draft's proposal is checked by, and refused with 422 and the same reasons: invalid_experiment, invalid_metrics, invalid_stages, invalid_cost, invalid_agents, invalid_length, run_too_long, " +
			"and accelerator_unreachable when no cluster the org's jobs can be placed on reports capacity now (GET /resource-catalog/capacity lists none): nothing of it could run. " +
			"With from_repo: true every field the body leaves out is read from " + domain.ExperimentFile + " at the head of the default branch of " +
			"repository, which the same body names (422 no_repository without it, no_experiment_file when the file is not there), a YAML file shaped like this body. What the body carries wins. budget_accelerator_hours, max_agents and duration_hours " +
			"are never read from the file: any agent's token can push to that branch, so what a run costs and how " +
			"long it lasts is stated by whoever creates it. A file that cannot be used answers 422 invalid_experiment_file and names the file. " +
			"coordinator_hosted: true has the platform run the coordinator as one more machine beside the agents', on coordinator_model or the platform's default: " +
			"it holds this platform experiment's coordinator token, is no agent of it, holds no quota and is not ranked. " + coordinatorWanted + " " +
			"It is not read from the file, and is fixed from then on. " +
			"repository is the code the platform experiment works on, owner/name on GitHub: agents clone it and push their branches to it, jobs check it out, and repository_url in the answer is where from. " +
			"A platform experiment keeps its code in a repository: left out, the platform creates a private one on the GitHub account the org connected, named from the experiment and its id, " +
			"whose one commit is the experiment template (README.md, " + domain.ExperimentFile + " and .hypothesisloop/job.yaml, the base job spec), " +
			"after the create answered: the answer has no repository yet and status_reason repository_creating, no machine of it starts before the repository is recorded, and a read then names it in repository and repository_url. The installation covers a repository it created whichever repositories its owner selected. " +
			"With several accounts connected, repository_account says which, and 422 repository_account_required lists them when it is left out; it is never stated with repository (422 invalid_request). " +
			"422 github_not_connected when the org has connected no GitHub account, and installation_not_connected when repository_account is none it connected: those are answered before anything is created. " +
			"On a GitHub organisation the installation creates the repository. On a person's own account GitHub creates one for its owner alone, so the platform creates it with the authorisation the owner connected the account with " +
			"and adds it to the installation. When GitHub does not create it (the authorisation is gone, GitHub refuses the creation or its first commit, the name is taken by a repository that is not this experiment's), " +
			"the platform experiment is closed with close_reason repository_not_created, and its status_reason message carries what GitHub answered. A repository the request names is never written to. " +
			"Named, an installation of the platform's GitHub App that the org connected must cover it " +
			"(GET /orgs/{id}/github-repositories lists what may be named): 422 invalid_repository when it is not owner/name, installation_not_connected when " +
			"the org connected no installation on the account that owns it, repository_not_reachable when the installation does not cover it, " +
			"and installation_gone when the App was removed on GitHub. " +
			"502 github_failed when GitHub answers 5xx or not at all for the installation, the message naming it and GitHub's answer. " +
			"On a deployment whose git host is plain the platform creates none and reads no experiment file: repository is a name under the host's root, created there beforehand, " +
			"422 repository_required without it, repository_not_reachable when it does not answer git, and 422 experiment_file_unsupported with from_repo. " +
			"agent_image is the image the hosted agents and the coordinator run, built FROM the platform's agent image so the harness is in it. Left out, " +
			"it is the platform's own. 422 invalid_agent_image when it is no image reference. Neither is read from the file and both are fixed from then on. " +
			"With coordinator_hosted: true it also creates one hosted agent of role baseline, named by its role, as accepting a draft does: its machine is asked for at once, it measures the baseline, and the platform declares the job it leaves when it finishes, so status_reason reads baseline_measuring from the first answer and nobody adds a baseline agent. 409 max_hosted_reached where the platform's limit holds no machine for it beside the coordinator's. " +
			"Without coordinator_hosted this call creates the experiment and no agent. Every researcher, and any other agent, joins by its own POST /platform-experiments/{id}/agents. " +
			"max_hosted in the answer is how many machines the platform runs for this platform experiment at once, its hosted agents of every role and its coordinator together: " +
			"one of them is kept for an agent of role baseline, and an agent that finished holds none. " +
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
		if err := peh.svc.checkLength(in.Body.DurationHours); err != nil {
			return nil, err
		}
		if err := domain.ValidateAgentImage(in.Body.AgentImage); err != nil {
			return nil, api.Refuse(http.StatusUnprocessableEntity, "invalid_agent_image", err.Error())
		}
		if in.Body.FromRepo {
			if in.Body.Repository == "" {
				return nil, api.Refuse(http.StatusUnprocessableEntity, "no_repository", "from_repo reads "+domain.ExperimentFile+" of the repository the request names, and it names none: set repository")
			}
			named := in.Body.Repository
			body, err := peh.fromRepo(ctx, in.Body)
			body.Repository = named
			if err != nil {
				return nil, err
			}
			in.Body = body
		}
		hosting, err := peh.withHosting(ctx, in.Body)
		if err != nil {
			return nil, peh.refusal("create platform experiment", err)
		}
		caller, err := peh.caller(ctx)
		if err != nil {
			return nil, err
		}
		by := caller.Actor()
		hosting.createdBy = &by
		if hosting.CoordinatorHosted {
			if hosting.agents, hosting.signups, err = peh.hostedAgents(nil, by, time.Now().UTC()); err != nil {
				return nil, peh.refusal("create platform experiment", err)
			}
		}
		if hosting.repository, err = peh.chooseRepository(ctx, in.Body.ProjectID, hosting.Repository, in.Body.RepositoryAccount); err != nil {
			return nil, err
		}
		pe, err := peh.svc.Create(ctx, hosting)
		// What is wrong with a body laid over the experiment file may be the file's doing.
		var refused *api.Refusal
		if in.Body.FromRepo && errors.As(err, &refused) {
			return nil, api.Refuse(refused.GetStatus(), refused.Reason, "with "+domain.ExperimentFile+" of the repository under the request: "+refused.Message)
		}
		var answered huma.StatusError
		if errors.As(err, &answered) {
			return nil, err
		}
		if err != nil {
			return nil, auth.Internal(peh.logger, "create platform experiment", err)
		}
		return peh.Answer(ctx, pe)
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-platform-experiments", Method: "GET", Path: "/platform-experiments",
		Summary: "List platform experiments", Tags: []string{"platform-experiments"},
		Description: "The caller's own platform experiments, within ?project_id when given, and with them the ones " +
			"staff shared with every account: those are in every project's listing, marked shared, read like the " +
			"caller's own and written by their own org alone. ?shared=true keeps only the shared ones, ?shared=false " +
			"leaves them out. A token bound to one platform experiment lists that one. A closed one carries closed_at and close_reason. " +
			"Filter with ?status, ?q (search name/description), ?limit (default 20, max 200, 422 invalid_page over it), ?offset. " +
			"?sort selects order: created_at, starts_at, name, or status, optionally prefixed with " +
			"- for descending (default -created_at). Total match count is returned in the " +
			"X-Total-Count response header.",
	}, func(ctx context.Context, in *struct {
		Status    string `query:"status" doc:"draft, open, running or closed."`
		Search    string `query:"q" doc:"Case-insensitive substring match against name and description."`
		ProjectID string `query:"project_id" doc:"Only the platform experiments of this project, and the shared ones."`
		Limit     int    `query:"limit" doc:"Page size, default 20, maximum 200: a larger one is refused with 422 invalid_page. The default is small because the whole response is meant to be read by an agent: ask for more deliberately, using X-Total-Count to decide."`
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
		if in.Limit < 0 || in.Limit > db.MaxPlatformExperimentListLimit || in.Offset < 0 {
			return nil, api.Refuse(http.StatusUnprocessableEntity, "invalid_page", "limit must be 1 to "+strconv.Itoa(db.MaxPlatformExperimentListLimit)+" and offset must not be negative")
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
		Description: "Returns status, budget, the stage ladder, the list of signed-up agents and prior_runs, the closed runs of the same project and repository that its agents and coordinator read as their own. consumed_accelerator_hours is what it has spent of its budget, the whole spend once it is closed. status_reason says why it is in its status, in every status. duration_hours count from the moment the run starts, which a running one holds in starts_at, and ends_at is that start plus duration_hours. A running one reads agents_starting while no researcher's machine is running yet: the first one that is sets started_at, and the platform closes it at agents_due_at (close_reason agents_not_started) when none comes up. From then it reads running, and researchers_done once every researcher has finished, was cut or lost its machine with no restart left: nobody is left to submit a job, the platform still closes it only at ends_at, and waits_for names who writes the summary and closes it before, its coordinator where the platform hosts one and its owner otherwise. A closed one reads its close_reason. An open one says why it is not running yet. Without a baseline that is where the measuring stands: baseline_measuring (an agent of role baseline is at work, and the platform declares its job when it finishes), baseline_required (none is at work and none left a job to declare: one is added), baseline_undeclared (they left several jobs and one is declared with PATCH), baseline_declaring (the platform is about to declare the one job), or what the platform's declaration of that one job is refused for, by the reasons of PATCH baseline_job_id: budget_below_baseline, baseline_breaks_constraint, baseline_missing_metric. With a baseline: starts_at_ahead, no_researchers, budget_spent, or starting when nothing is missing: the request that declares the baseline or adds the researcher starts the run itself. The message says what the state waits for and who acts, with the hl command, and to an agent it begins with that agent's own next step: an agent of role baseline is told to measure, and any other that its jobs are refused until the start. Nothing else says what an open platform experiment waits for.repository is the code it works on, owner/name on GitHub, " +
			"and repository_url the address agents clone and jobs check out. Both are absent when it names none. " +
			"One made from a sentence carries goal, and while it is a draft what its planner proposed (proposal, proposed_at) and what a person asked to change (revisions): " +
			"status_reason says where a draft stands: planning, revising, proposed (proposed_at is set and it waits to be accepted) or declined (declined_at and declined_reason are set, there is nothing to accept, and it waits to be revised or closed). Either answer waits until proposal_expires_at, when the platform closes the draft.",
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
		prior, err := peh.svc.store.PriorRuns(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(peh.logger, "get platform experiment", err, zap.String("id", in.ID))
		}
		reached := prior[:0]
		for _, id := range prior {
			if access.PlatformExperiment(ctx, id) == nil {
				reached = append(reached, id)
			}
		}
		if pe.PriorRuns, err = peh.svc.PriorRuns(ctx, reached); err != nil {
			return nil, auth.Internal(peh.logger, "get platform experiment", err, zap.String("id", in.ID))
		}
		if err := peh.svc.Spend(ctx, pe); err != nil {
			return nil, auth.Internal(peh.logger, "get platform experiment", err, zap.String("id", in.ID))
		}
		if err := peh.nameActors(ctx, pe); err != nil {
			return nil, auth.Internal(peh.logger, "get platform experiment", err, zap.String("id", in.ID))
		}
		return peh.Answer(ctx, pe)
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "update-platform-experiment", Method: "PATCH", Path: "/platform-experiments/{id}",
		Summary: "Change fields of a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Takes only the fields that change; a field left out keeps its value, and a body with none answers 422 nothing_to_change. " +
			"While the experiment is open every field of the body can change, `metrics` among them, so a mis-specified ranking metric is fixed in place. " +
			"Once it runs only `name`, `description`, `duration_hours` and the baseline (`baseline_job_id` with `baseline_reason`) do, because admission and the stage ladder have acted on the rest: a body that carries another field answers 409 experiment_running and names it. " +
			"A duration_hours of a running one moves the end of its run, still counted from its start, and one that ends it before now is 422 invalid_length. A new description or duration_hours reaches every agent of it as platform_experiment.description. " +
			"409 experiment_not_editable for a draft or a closed one. The experiment as patched is checked like a create and refused with the same 422: " +
			"invalid_experiment (an empty name or description among them), invalid_metrics, invalid_cost, invalid_agents, invalid_length, " +
			"409 max_agents_below_researchers for a max_agents under the researchers (agents of role competitor) that already joined, " +
			"run_too_long for a duration_hours over the platform's quota.max_platform_experiment_days. A starts_at in the body moves baseline_due_at with it, a starts_at past being now. " +
			"`baseline_job_id` declares the baseline while the experiment is open, and nothing else is needed in the body. The platform makes this declaration itself, by the same check, when an agent of role baseline finishes naming the job (baseline_job_id of its finish) or, naming none, the baseline agents that finished left exactly one COMPLETED job that reports the ranking metric, and again in the patch that clears what that declaration was refused for (a raised budget_accelerator_hours): a caller declares only where a choice is needed, between several jobs or after a redo. The platform writes what it holds of the job into the description, a block that begins `" + measuredBaselineHeader + "` and states job, code_ref, env (the job's settings, when it has any) and measured (the last value the job reported on each metric but the references), then a line for each value a COMPLETED job reported on a reference, its job and whether that job met the constraints (only one that did counts), and the references no completed job reported, as not measured, and keeps that block through every later patch of the description. With a researcher joined and no starts_at ahead the run starts in this same request and the answer reads running; otherwise it starts with the request that adds the researcher, or at starts_at. The start closes signup and gives each agent present, those of role baseline apart, its share of the first stage's part of the budget, less what measuring the baseline spent. The start also settles `stages`: a boundary cuts nobody from fewer than 5 researchers (role competitor), so a run started with fewer has one stage of its whole length, with the last declared stage's max_job_hours, and each researcher holds its whole share from the start. With 5 or more the ladder is as declared. status_reason in the answer says what an open experiment still waits for: no_researchers until one joined, budget_spent when the baseline left nothing to share. The job is read from the platform's own records and the declaration refused with 422: " +
			"no_baseline (no job of an agent of role baseline of this platform experiment), baseline_not_completed, baseline_not_on_branch (the job ran a branch other than the baseline branch), baseline_without_finding, " +
			"baseline_missing_metric (a declared metric the job never reported, a reference apart: one left out is listed in the block as not measured), " +
			"baseline_breaks_constraint (the job's value does not hold a constraint's bound), budget_below_baseline (the budget does not hold one trial for each of max_agents: the job again at job_estimated_duration_hours). " +
			"Stored, the experiment answers baseline_job_id and job_estimated_duration_hours, one and a half times the hours the job was observed running, which is the estimated_duration_hours of every later job that states none. " +
			"An open experiment that has a baseline is held to it by every later patch, with the same refusals, so its metrics and budget cannot leave the baseline behind. " +
			"While it runs, `baseline_job_id` with `baseline_reason` raises the baseline to a stronger COMPLETED job of the experiment that ran the baseline's commit unchanged (its code_ref equals the current baseline's, only its arguments or environment differ) and so measured the control in a better configuration it already has, with baseline_reason saying which configuration, whoever ran it on whichever branch. A job of any other commit is refused with 422 baseline_code_differs, naming the commit it ran and the baseline's. The job is held to the same checks as a declaration, but for the agent's role and the branch: baseline_not_completed, baseline_without_finding, baseline_missing_metric, baseline_breaks_constraint, and baseline_environment_differs or baseline_not_in_environment when it did not run in the environment the head of the baseline branch declares (a job of another branch whose commit declares the same file ran in it), with no_baseline for a job that is not the experiment's, and 422 baseline_not_stronger when it is not strictly better than the baseline on the first ranking metric. " +
			"422 baseline_reason_required without a reason, baseline_job_required for a reason with no job, baseline_unchanged for the baseline itself, and 409 baseline_not_running for a baseline_reason on an experiment that is not running (while it is open the baseline agent's job is declared; a closed one changes nothing). " +
			"A raise rewrites the measured baseline block of the description from the new job, keeps job_estimated_duration_hours, and is recorded in baseline_raises (from, to, reason, who, when), in the record of the close and as platform_experiment.baseline to every agent, whose detail says the values before and after. Standings in results are set against the new baseline from then on, and a verdict already given against the old one stays as recorded. " +
			"422 invalid_experiment for a report_interval_seconds of 0 or less. " +
			"A change of budget_accelerator_hours writes the quota of every agent of role baseline again, as half the budget. " +
			"A coordinator's token changes `description`, `baseline_job_id` and `baseline_reason` only: a body that carries any other field answers 403 coordinator_changes_description_only. " +
			"The project, the repository, the agent image, the stages and the default quota tier are fixed at creation and are no fields of this body.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body PlatformExperimentPatch
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		// A coordinator's token is handed to a machine the platform rents, so it changes the
		// brief and declares the baseline, and nothing a run's cost or length follows from.
		caller, err := peh.caller(ctx)
		if err != nil {
			return nil, err
		}
		if caller.Kind == auth.KindCoordinator {
			if others := in.Body.carried("description", "baseline_job_id", "baseline_reason"); len(others) > 0 {
				return nil, api.Refuse(http.StatusForbidden, "coordinator_changes_description_only", "a coordinator's token changes the description and the baseline (baseline_job_id, baseline_reason) and nothing else, and the body also carries: "+strings.Join(others, ", ")+". Leave them out")
			}
		}
		by := caller.Actor()
		in.Body.by = &by
		pe, err := peh.svc.Update(ctx, in.ID, in.Body)
		if err != nil {
			return nil, peh.refusal("update platform experiment", err)
		}
		answered := *pe
		return peh.Answer(ctx, &answered)
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment-signup", Method: "GET", Path: "/platform-experiments/{id}/signups/{agent_id}",
		Summary: "Read an agent's signup", Tags: []string{"platform-experiments"},
		Description: "What an agent was signed up as: its name, its role, which decides whether it is ranked and cut, and the flavor, " +
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
			AgentID: signup.AgentID, Name: signup.Name, Role: string(signup.Role), Flavor: signup.Flavor, Model: signup.Model, Hyperparameters: signup.Hyperparameters,
			FinishedAt: signup.FinishedAt, Ended: signup.Ended,
		}}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "close-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/close",
		Summary: "Close a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Terminal: every still-running job is evicted (reason experiment_closed) and " +
			"no further submission is accepted. Optional top_results records the final standings; " +
			"omitted, standings are derived from observed metrics. The answer is the platform experiment as closed: close_reason closed_by_request with closed_by, " +
			"and close_message, the optional reason, which status_reason's message, the record of the close in summary and the detail of the platform_experiment.status event of the close repeat. " +
			"A run the platform closed at its ends_at reads end_time_reached, an open one it closed at its baseline_due_at baseline_limit_reached and a running one it closed at its agents_due_at agents_not_started. status_reason carries the same reason with a sentence. " +
			"One that was open or ran and has no summary is given one by the platform in the same write: the close_reason, the jobs submitted, what each agent last did and how its run ended, and the spend. Its coordinator, on its last turn, writes over it. " +
			"409 invalid_transition for one already closed, which changes nothing.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body *struct {
			Reason     string        `json:"reason,omitempty" maxLength:"2000" doc:"Why it is closed, in a sentence or two. Kept as close_message and told to its coordinator and agents with the close."`
			TopResults []AgentResult `json:"top_results,omitempty" doc:"Best-first final standings (up to 3). Omit to derive standings from the metrics store."`
		}
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		var topResults []AgentResult
		message := ""
		if in.Body != nil {
			topResults, message = in.Body.TopResults, strings.TrimSpace(in.Body.Reason)
		}
		caller, err := peh.caller(ctx)
		if err != nil {
			return nil, err
		}
		by := caller.Actor()
		pe, err := peh.svc.Close(ctx, in.ID, &by, message, topResults)
		if err != nil {
			return nil, peh.refusal("close platform experiment", err)
		}
		return peh.Answer(ctx, pe)
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment-results", Method: "GET", Path: "/platform-experiments/{id}/results",
		Summary: "Final standings and operator summary", Tags: []string{"platform-experiments"},
		Description: "Per-metric standings, best-first, derived from the metrics store on every read " +
			"(never frozen into PostgreSQL), plus the operator's narrative summary. bests lists, for every " +
			"declared metric whatever its role, each reporting agent's best raw value and the job that set it, unranked. " +
			"Each ranking metric carries baseline, the last value the declared baseline job (baseline_job_id) reported on it, and each standing beats_baseline, whether its best is strictly better: they follow the baseline when it is raised. " +
			"references lists each metric of role reference, an outside system the goal compares against, with the value of every COMPLETED job that reported it, its agent, ran_on, the machine that job ran on, as each standing carries ran_on (a result compares with a reference measured on the same machine), and constraints_met: true, false when the job broke a constraint metric, unknown when it did not report one. best, leader and beaten count only values whose constraints_met is true. A reference with no values was not measured.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *PlatformExperimentResults }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		results, err := peh.svc.Results(ctx, in.ID)
		if errors.Is(err, ErrPlatformExperimentNotFound) {
			return nil, huma.Error404NotFound(err.Error())
		}
		if err != nil {
			return nil, auth.Internal(peh.logger, "get platform experiment results", err, zap.String("id", in.ID))
		}
		return &struct{ Body *PlatformExperimentResults }{Body: results}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment-health", Method: "GET", Path: "/platform-experiments/{id}/health",
		Summary: "Where each agent of a run last stood", Tags: []string{"platform-experiments"},
		Description: "For each agent and the hosted coordinator, the latest values of the run's health series, read from the metrics store, which records them while the run is open or running: " +
			"seconds to its first job and its first completed job, jobs in flight, restarts of its machine over the whole run and messages the coordinator sent it. " +
			"agents is empty for a run that never opened.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *RunHealth }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		health, err := peh.svc.Health(ctx, in.ID)
		if errors.Is(err, ErrPlatformExperimentNotFound) {
			return nil, huma.Error404NotFound(err.Error())
		}
		if err != nil {
			return nil, auth.Internal(peh.logger, "get platform experiment health", err, zap.String("id", in.ID))
		}
		return &struct{ Body *RunHealth }{Body: health}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, apidocs.AfterClose(huma.Operation{
		OperationID: "set-platform-experiment-summary", Method: "PUT", Path: "/platform-experiments/{id}/summary",
		Summary: "Record the operator's narrative verdict on a run", Tags: []string{"platform-experiments"},
		Description: "Replaces the summary and answers the platform experiment with it. Writable after the experiment closes: it is the one field whose whole point " +
			"is to be written once the run is over and the results are in. A person or an API key writes it at any time. " +
			"A coordinator's token writes it until five minutes after the close (domain.CoordinatorWindDown), when the token ends (401). 422 summary_too_long past 32768 bytes.",
	}), func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			Summary string `json:"summary"`
		}
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		pe, err := peh.svc.SetSummary(ctx, in.ID, in.Body.Summary)
		if err != nil {
			return nil, peh.refusal("set platform experiment summary", err)
		}
		return peh.Answer(ctx, pe)
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-platform-experiment-quotas", Method: "GET", Path: "/platform-experiments/{id}/quotas",
		Summary: "List per-agent quotas for a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Each signed-up agent's share of the budget, what it has used and what a job submitted now may still reserve. Guaranteed and burst jobs spend the same share: burst gives no hours on top of it.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body []*AgentQuotaView }, error) {
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
		return &struct{ Body []*AgentQuotaView }{Body: quotas}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-agent-quota", Method: "GET", Path: "/platform-experiments/{id}/quotas/{agent_id}",
		Summary: "Get one agent's quota within a platform experiment", Tags: []string{"platform-experiments"},
		Description: "One row of what GET /platform-experiments/{id}/quotas returns for every agent, and with it data_bytes and max_data_bytes: " +
			"what the agent holds in the object store and the ceiling a submit is refused on with data_quota_exceeded, where the platform sets one. " +
			"503 object_store_unavailable while the object store does not answer. " +
			"404 planner_has_no_quota for a draft's planner, which submits no job and needs none: the message says where the run's budget comes from instead. " +
			"404 quota_not_granted while the platform experiment is open and the agent holds none yet: quota is granted at the start, and the message is the platform experiment's status_reason, what the caller does next.",
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		AgentID string `path:"agent_id"`
	}) (*struct{ Body *AgentQuotaView }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		aq, err := peh.svc.QuotaView(ctx, in.AgentID, in.ID)
		if err != nil {
			return nil, auth.Internal(peh.logger, "get agent quota", err, zap.String("id", in.ID))
		}
		if aq == nil {
			pe, err := peh.svc.store.GetPlatformExperiment(ctx, in.ID)
			if err != nil {
				return nil, auth.Internal(peh.logger, "get agent quota", err, zap.String("id", in.ID))
			}
			signup, err := peh.svc.store.GetAgentSignup(ctx, in.ID, in.AgentID)
			if err != nil {
				return nil, auth.Internal(peh.logger, "get agent quota", err, zap.String("id", in.ID))
			}
			if signup != nil && signup.Role == domain.SignupRolePlanner {
				return nil, api.Refuse(http.StatusNotFound, "planner_has_no_quota", "agent "+in.AgentID+" is the planner of platform experiment "+in.ID+": a planner submits no job, so it has no quota and needs none. The run's budget is the budget_accelerator_hours its proposal states (hl platform-experiments propose --help), sized against hl capacity, and it is shared among the researchers when the run starts")
			}
			if pe == nil || pe.Status != domain.PlatformExpOpen {
				return nil, huma.Error404NotFound("agent " + in.AgentID + " has no quota in platform experiment " + in.ID)
			}
			reader, err := peh.caller(ctx)
			if err != nil {
				return nil, err
			}
			if err := peh.svc.Describe(ctx, pe, reader); err != nil {
				return nil, auth.Internal(peh.logger, "get agent quota", err, zap.String("id", in.ID))
			}
			return nil, api.Refuse(http.StatusNotFound, "quota_not_granted", "agent "+in.AgentID+" holds no quota yet: it is granted when the platform experiment starts. "+pe.StatusReason.Message)
		}
		return &struct{ Body *AgentQuotaView }{Body: aq}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-stages", Method: "GET", Path: "/platform-experiments/{id}/stages",
		Summary: "Get the stage ladder and current standing of a platform experiment", Tags: []string{"platform-experiments"},
		Description: "The ladder, which stage is running, progress through it, and which signed-up agents are still active vs cut. Open, the ladder is the one declared. From the start it is the one the run has: a single stage where fewer than 5 researchers joined, which has no boundary and sends no stage.boundary. Each stage carries max_job_hours: the longest one job may run while it is current (0/absent = unlimited). Where the next cut falls is not shown; every agent's best value is, at GET /platform-experiments/{id}/results.",
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
		OperationID: "get-resource-capacity", Method: "GET", Path: "/resource-catalog/capacity",
		Summary: "Get live per-cluster accelerator capacity", Tags: []string{"resource-catalog"},
		Description: "Live {accelerator_type, available, total} per cluster the jobs of one org can be placed on: the org's own " +
			"clusters and the ones the platform shares, without the ones the org turned off for its jobs. The org is the one your token is bound to: every API key, agent and " +
			"coordinator token has exactly one, and so does a person in one org. A person in several orgs names it with org_id, " +
			"and is answered 400 without it. An org you do not reach is not found. Staff read one org with org_id and every " +
			"cluster that reports without it. accelerator_type is the exact " +
			"driver-published string a job spec takes, so what you read here is what you submit. pod_resources on a type " +
			"is what the platform adds to every node of a job on it, which the job does not state itself. A job's cpu, memory " +
			"and accelerator_count are per node, so per_node on a cluster says the most cores and memory one node has, installed " +
			"and free now, and per_node_available on a type the most of it free on one node now. Check before " +
			"choosing: a type with no live capacity anywhere QUEUES FOREVER and never errors. Only " +
			"currently-schedulable nodes count — powered-off hardware never appears."}, func(ctx context.Context, in *struct {
		OrgID string `query:"org_id" doc:"The org whose clusters to read. Required only when your token reaches more than one org."`
	}) (*struct {
		Body struct {
			Clusters  []ClusterCapacity `json:"clusters"`
			JobImages []domain.JobImage `json:"job_images" doc:"The images the deployment offers a job to run in, each with what it carries. A capacity line names the ones that suit its type. A job names one of these by its name, or any other image by its full registry address, host first: 422 unknown_job_image otherwise, and no_cluster_for_image for one no cluster could run."`
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
				JobImages []domain.JobImage `json:"job_images" doc:"The images the deployment offers a job to run in, each with what it carries. A capacity line names the ones that suit its type. A job names one of these by its name, or any other image by its full registry address, host first: 422 unknown_job_image otherwise, and no_cluster_for_image for one no cluster could run."`
			}
		}{}
		out.Body.Clusters = clusters
		out.Body.JobImages = peh.jobImages
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
	Name            string         `json:"name" doc:"The name the agent's branches carry: hl/<platform experiment>/<name>/..."`
	Role            string         `json:"role"`
	Flavor          string         `json:"flavor"`
	Model           string         `json:"model"`
	Hyperparameters map[string]any `json:"hyperparameters"`
	FinishedAt      *time.Time     `json:"finished_at,omitempty" doc:"When the agent said it has nothing left to do."`
	Ended           domain.RunEnd  `json:"ended" enum:",closed,cut,finished,planned" doc:"Why the agent's run here is over, the empty string while it goes on: the platform experiment closed, the agent was cut at a stage boundary, it finished, or it is a draft's planner whose proposal or decline is stored. A machine the platform runs for the agent is told to stop for this and nothing else."`
}
