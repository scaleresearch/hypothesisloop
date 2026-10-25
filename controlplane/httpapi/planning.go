package httpapi

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
)

type planningAPI struct {
	platformExperimentsAPI
}

// registerPlanning registers the way from one sentence to a running platform experiment: a
// draft and the planner's proposal, which opens it.
func registerPlanning(doc *Doc, experiments *platformexperiments.Service, logger *zap.Logger) {
	a := planningAPI{platformExperimentsAPI{experiments: experiments, access: doc.Access, logger: logger}}
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "put-decline", Method: "PUT", Path: "/platform-experiments/{id}/decline",
		Summary: "Decline to propose, with the reason", Tags: []string{"platform-experiments"},
		Description: "Written by a draft's planner and by nobody else, while the platform experiment is a draft (409 not_a_draft after), when the goal cannot be made into an experiment: " +
			"nothing measurable, or nothing this platform can run. reason is read by the person who wrote the goal, in plain words, with what would make it plannable. " +
			"Stored, the draft carries declined_reason and declined_at and no proposed_at, its status_reason reads declined, " +
			"the planner's machine is kept until planner_ends_at for a goal changed soon and released then, and the person changes the draft's goal or closes it. It waits for that until proposal_expires_at, and the platform closes it then with close_reason proposal_expired. The next proposal clears the reason. " +
			"goal_version is the draft's goal_version the decline answers: 409 goal_changed once the draft has another goal, which the planner reads and answers instead. Stored, it is told as platform_experiment.proposal declined.",
	}, a.putDecline)
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "put-proposal", Method: "PUT", Path: "/platform-experiments/{id}/proposal",
		Summary: "Propose the platform experiment a draft becomes", Tags: []string{"platform-experiments"},
		Description: "Written by a draft's planner and by nobody else, while the platform experiment is a draft (409 not_a_draft after). " +
			"experiment is the experiment as its file would state it; budget_accelerator_hours, duration_hours, baseline_minutes and agents are what the run costs and who runs it. " +
			"agents is the team, one entry for each agent, [{agent_type, model, hyperparameters}]: an entry of agent_type researcher is a researcher, which runs no job, holds no quota and steers the work through the hypothesis graph, defaulting to the coordinator's model (coordinator_model) when neither the entry nor its agent type names one, and any other an executor, which runs the jobs; at least one executor, and the executors are the run's max_agents. " +
			"A proposal defines the experiment and runs nothing: its description states the objective and the measure and nothing measured yet, and its numbers are estimates. " +
			"The proposal is checked, its experiment and its cost by the rules of POST /platform-experiments, and refused with 422 and a reason the planner can act on: invalid_experiment, invalid_metrics, invalid_stages, " +
			"invalid_cost, invalid_agents, invalid_length, run_too_long (duration_hours over the platform's quota.max_platform_experiment_days), invalid_baseline_minutes (under planning.min_baseline_minutes, or over planning.baseline_limit_minutes), missing_explanation, too_long, " +
			"accelerator_unreachable for an accelerator_type no cluster this org's jobs can be placed on has now, naming every type that is. " +
			"One that passes is applied in the same write: the draft becomes an open platform experiment with the proposal's name, description, metrics, report interval, stages, policies, budget, length and team, the answers to its questions added to the end of the description under the assumptions the plan runs on, " +
			"and the platform hosts its coordinator and each agent of its team beside the baseline agent the draft was created with. " + platformexperiments.CoordinatorWanted + " " +
			"starts_at is now and it is open until baseline_due_at, baseline_minutes from now, and one not running by then is closed with close_reason baseline_limit_reached. The run lasts duration_hours from the moment it starts. The baseline agent measures the baseline, the platform declares the job it leaves when it finishes and starts the run, and the executors' machines start with the first baseline job, so they are up and have read the code when it does; no job of theirs is taken before. " +
			"proposal stays on the platform experiment as it was proposed, with proposed_at and accepted_at, and accepted_answers the answers added. It is told as platform_experiment.proposal proposed and platform_experiment.status open, and the planner's machine is released. A person changes what they want of the plan afterwards with PATCH /platform-experiments/{id}. " +
			"goal_version is the draft's goal_version the proposal was planned from: 409 goal_changed once the draft has another goal, so a proposal for a goal the person changed is never applied. 503 agentsandbox_not_configured when nothing could run it. " +
			"A refusal lists every problem the check found, each in problems with its reason and message, the first also as the refusal's own reason. " +
			"With check=true the proposal is checked exactly so and nothing is stored: 422 with every problem, or 200 with the draft as it stands, so a planner tries a variant with check before it proposes the one it means. " +
			"With working=true the proposal is checked exactly so and stored as the draft's working plan in proposal, with proposed_at unset: the draft stays a draft, the next working plan or proposal replaces it, a change of the goal drops it, and it is told as platform_experiment.proposal working, which the baseline agent starts from. 409 already_answered once the planner answered the goal.",
	}, a.putProposal)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "create-draft", Method: "POST", Path: "/platform-experiments/drafts",
		Summary: "Start planning a platform experiment from one sentence", Tags: []string{"platform-experiments"},
		DefaultStatus: 201,
		Description: "Creates a platform experiment in status draft from goal, one sentence saying what the caller wants to find out. " +
			"A draft has no budget, no metrics, and two agents, which the platform runs at once and which are in the roster like any agent: its planner and its baseline agent (role baseline). " +
			"Nobody else joins. The planner works out how to measure the goal and proposes the metrics, " +
			"the description, the hardware, the budget and the agents, and that proposal opens it. Until then its max_agents is the most agents a proposal's team may name: max_agentsandboxes less the coordinator and the baseline agent. A draft takes no job, so nothing is measured " +
			"and no compute is spent until then. Its name is the goal cut short until the proposal names it. POST /platform-experiments creates one whole instead, " +
			"and states what a run costs itself. Closing a draft (POST /platform-experiments/{id}/close) abandons it. " +
			"Its planner and later its agents keep their code in repository: the one the request names, checked as on POST /platform-experiments, or a private one the platform creates on the account the org connected on the git host. " +
			"repository_account says which account when the org connected several. 422 git_host_not_connected, repository_account_required and installation_not_connected as on POST /platform-experiments. " +
			"A repository the platform creates is created after this answers: the draft is answered without repository and with status_reason repository_creating, and its machines start once the repository is there. " +
			"When the git host does not create it the draft is closed with close_reason repository_not_created, and its status_reason message says what the git host answered. " +
			"repository and branch are checked as on POST /platform-experiments, and a repository named that does not exist is created from the experiment template. " +
			"On a deployment whose git host is plain, repository is a name under the host's root and is required: 422 repository_required without it. " +
			"422 invalid_goal for a goal that is empty or over the length, invalid_continues for a continues that names no closed platform experiment of the project and repository with a declared baseline. 503 agentsandbox_not_configured when the deployment has no sandbox provider to run the planner on.",
	}, a.createDraft)
}

type declineRequest struct {
	Reason      string `json:"reason" doc:"Why no experiment is proposed, and what would make the goal plannable."`
	GoalVersion int    `json:"goal_version" doc:"The goal_version of the draft the decline answers."`
}

func (a planningAPI) putDecline(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body declineRequest
}) (*struct{ Body *domain.PlatformExperiment }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	caller, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	pe, err := a.experiments.Decline(ctx, in.ID, caller, in.Body.GoalVersion, in.Body.Reason)
	if err != nil {
		return nil, planningRefusal(err)
	}
	return a.answer(ctx, pe)
}

func (a planningAPI) putProposal(ctx context.Context, in *struct {
	ID      string `path:"id"`
	Check   bool   `query:"check" doc:"Check the proposal as a proposal is checked and store nothing: the answer is the draft as it stands, or 422 with every problem."`
	Working bool   `query:"working" doc:"Check the proposal as a proposal is checked and store it as the draft's working plan: the draft stays a draft, its baseline agent starts from it, and a later one replaces it."`
	Body    platformexperiments.Proposal
}) (*struct{ Body *domain.PlatformExperiment }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	caller, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	pe, err := a.experiments.Propose(ctx, in.ID, caller, &in.Body, in.Check, in.Working)
	if err != nil {
		return nil, planningRefusal(err)
	}
	return a.answer(ctx, pe)
}

type createDraftRequest struct {
	ProjectID         string `json:"project_id"`
	Goal              string `json:"goal" doc:"What you want to find out, in a sentence or two. At most 2000 bytes."`
	Repository        string `json:"repository,omitempty" required:"false" doc:"The repository as the git host names it (owner/name on GitHub), one the org reaches, created from the experiment template when it does not exist. Left out, the platform creates a private repository on the account the org connected on the git host."`
	Branch            string `json:"branch,omitempty" required:"false" doc:"The branch of repository its agents clone and start from. Required with repository, and refused without it: a repository the platform creates is worked on at its default branch."`
	RepositoryAccount string `json:"repository_account,omitempty" required:"false" doc:"The account the org connected on the git host that the new repository is created on. Required only when the org connected several."`
	Continues         string `json:"continues,omitempty" required:"false" doc:"The id of a closed platform experiment of the same project to carry on, which keeps its code in the repository this one names. Its baseline starts from the commit of that run's baseline job, so both are measured by the same harness. It keeps the metric keys and directions of that run: 422 invalid_metrics for others. Left out, it starts fresh. 422 invalid_continues for one that is no closed platform experiment of the project, keeps its code in another repository or declared no baseline."`
}

func (a planningAPI) createDraft(ctx context.Context, in *struct {
	Body createDraftRequest
}) (*struct{ Body *domain.PlatformExperiment }, error) {
	if in.Body.ProjectID == "" {
		return nil, refusal.Invalid(refusal.ReasonInvalidRequest, "project_id is required: the project the experiment belongs to").On("project_id")
	}
	if err := a.access.Project(ctx, in.Body.ProjectID); err != nil {
		return nil, err
	}
	caller, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	pe, err := a.experiments.CreateDraft(ctx, in.Body.ProjectID, in.Body.Goal, in.Body.Continues, in.Body.Repository, in.Body.Branch, in.Body.RepositoryAccount, caller.Actor())
	if err != nil {
		return nil, err
	}
	return a.answer(ctx, pe)
}

// planningRefusal answers the planner check and a draft that is gone as the API always has.
func planningRefusal(err error) error {
	switch {
	case errors.Is(err, platformexperiments.ErrNotThePlanner):
		return huma.Error403Forbidden(err.Error())
	case errors.Is(err, platformexperiments.ErrPlatformExperimentNotFound):
		return huma.Error404NotFound(err.Error())
	default:
		return err
	}
}
