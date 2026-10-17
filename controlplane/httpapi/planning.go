package httpapi

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
)

type planningAPI struct {
	platformExperimentsAPI
}

// registerPlanning registers the way from one sentence to a running platform experiment: a
// draft, the planner's proposal, and the person's acceptance.
func registerPlanning(doc *Doc, experiments *platformexperiments.Service, logger *zap.Logger) {
	a := planningAPI{platformExperimentsAPI{experiments: experiments, access: doc.Access, logger: logger}}
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "put-decline", Method: "PUT", Path: "/platform-experiments/{id}/decline",
		Summary: "Decline to propose, with the reason", Tags: []string{"platform-experiments"},
		Description: "Written by a draft's planner and by nobody else, while the platform experiment is a draft (409 not_a_draft after), when the goal cannot be made into an experiment: " +
			"nothing measurable, or nothing this platform can run. reason is read by the person who wrote the goal, in plain words, with what would make it plannable. " +
			"Stored, it answers the goal as a proposal does: the draft carries declined_reason and declined_at and no proposed_at, its status_reason reads declined, a proposal made before is cleared so there is nothing to accept, " +
			"the planner's machine is kept until planner_ends_at for a goal changed soon and released then, and the person changes the draft's goal or closes it. It waits for that until proposal_expires_at, as a proposal does, and the platform closes it then with close_reason proposal_expired. The next proposal clears the reason. " +
			"goal_version is the draft's goal_version the decline answers: 409 goal_changed once the draft has another goal, which the planner reads and answers instead. Stored, it is told as platform_experiment.proposal declined.",
	}, a.putDecline)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "accept-draft", Method: "POST", Path: "/platform-experiments/{id}/accept",
		Summary: "Accept a draft's proposal", Tags: []string{"platform-experiments"},
		Description: "Makes a proposed draft an open platform experiment in one write: its name, description, metrics, report interval, stages and policies are the proposal's; " +
			"its budget, its length, the minutes its baseline has and its researchers are the ones this body states, whatever the proposal proposed, because an agent wrote the proposal. " +
			"name, when the body states one, replaces the proposal's name and nothing else of it (422 invalid_experiment for one that is no name). " +
			"answers holds one answer for each of the proposal's questions, in their order, as the person confirmed or changed it: each question and its answer are added to the end of the description, a line each, so every agent reads the decisions as made. 422 invalid_answers when it does not hold one non-empty answer for each question. " +
			"proposal stays on the platform experiment as it was proposed, for anyone who reads it, beside accepted_at, accepted_by (absent when the platform accepted it by itself) and accepted_answers. " +
			"The budget caps everything the platform experiment spends, the baseline included: what measuring it cost is taken from the budget, the rest is shared among the researchers, and an agent's burst hours run on idle capacity and come out of its share, never on top of it. " +
			"The platform hosts its coordinator, each researcher and one agent of role baseline, named by its role, which the proposal signed up so that its machine starts while the proposal is read, and which is created here for a proposal that signed none up. " + platformexperiments.CoordinatorWanted + " " +
			"starts_at is now and it is open until baseline_due_at, baseline_minutes from now, and one not running by then is closed with close_reason baseline_limit_reached. The run lasts duration_hours from the moment it starts, and measuring the baseline takes nothing from it. Nothing has run yet: the baseline agent measures the baseline, the platform declares the job it leaves when it finishes and starts the run, " +
			"and no researcher's machine is started before. Where that agent leaves no job, or several, the coordinator adds another or declares one (PATCH /platform-experiments/{id}, baseline_job_id), and status_reason says so. " +
			"The platform makes the same accept by itself at accepts_at, at the proposal's own budget, length, baseline_minutes and researchers, for the person who created the draft, unless a person accepted, held or closed it or changed its goal first; one the check refuses then waits for a person. A proposal with questions has no accepts_at and waits for a person to answer them. " +
			"409 invalid_transition for anything but a draft with a proposal, which a change of its goal takes back until its planner answers again; 422 with the proposal's reasons when the stated cost, baseline_minutes or agents do not pass its check, " +
			"accelerator_unreachable among them when the proposal's accelerator_type is on no cluster the org's jobs can be placed on now, and no_cluster_for_image when none that has it runs the proposal's image any more; " +
			"503 agentsandbox_not_configured when nothing could run it. A draft's own planner cannot accept it.",
	}, a.acceptDraft)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "hold-draft", Method: "POST", Path: "/platform-experiments/{id}/hold",
		Summary: "Keep the platform from accepting a draft's proposal by itself", Tags: []string{"platform-experiments"},
		Description: "For a person who edits the proposal before accepting it: accepts_at is cleared, status_reason reads proposed, and the draft waits for the person to accept it (POST /platform-experiments/{id}/accept, with what they changed), change its goal or close it. " +
			"The baseline agent the proposal signed up is let go of its machine until then, and is given one again with the accept. Holding a draft that is held changes nothing. " +
			"409 invalid_transition for anything but a draft with a proposal standing.",
	}, a.holdDraft)
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "put-proposal", Method: "PUT", Path: "/platform-experiments/{id}/proposal",
		Summary: "Propose the platform experiment a draft becomes", Tags: []string{"platform-experiments"},
		Description: "Written by a draft's planner and by nobody else, while the platform experiment is a draft (409 not_a_draft after). " +
			"experiment is the experiment as its file would state it; budget_accelerator_hours, duration_hours, baseline_minutes and agents are proposed, and the person who accepts states them. " +
			"A proposal defines the experiment and runs nothing: its description states the objective and the measure and nothing measured yet, its numbers are estimates, " +
			"and the baseline is measured once it is accepted, before any researcher starts, by the baseline agent the proposal signs up, whose machine starts with it. " +
			"The proposal is checked before it is stored, its experiment and its cost by the rules of POST /platform-experiments, and refused with 422 and a reason the planner can act on: invalid_experiment, invalid_metrics, invalid_stages, " +
			"invalid_cost, invalid_agents, invalid_length, run_too_long (duration_hours over the platform's quota.max_platform_experiment_days), invalid_baseline_minutes (under planning.min_baseline_minutes, or over planning.baseline_limit_minutes), missing_explanation, too_long, " +
			"accelerator_unreachable for an accelerator_type no cluster this org's jobs can be placed on has now, naming every type that is, and for image the two refusals a job submit gives: " +
			"unknown_job_image for one that is neither a job image the platform offers nor a whole registry address, and no_cluster_for_image for one no cluster that has the type runs. Stored, it replaces the one before, marks the draft proposed " +
			"(proposed_at), is told as platform_experiment.proposal proposed, and the planner's machine is released: GET /platform-experiments/{id} shows the proposal, and the platform accepts it as proposed at accepts_at (planning.auto_accept_seconds from now) unless a person accepts it sooner, holds it, changes its goal or closes it. A proposal with questions, or one made with no auto-accept configured, has no accepts_at and waits for a person to accept it, and its baseline agent's machine starts with the accept. " +
			"goal_version is the draft's goal_version the proposal was planned from: 409 goal_changed once the draft has another goal, so a proposal for a goal the person changed is never stored nor accepted by itself. " +
			"A refusal lists every problem the check found, each in problems with its reason and message, the first also as the refusal's own reason. " +
			"With check=true the proposal is checked exactly so and nothing is stored: 422 with every problem, or 200 with the draft as it stands, so a planner tries variants with check and proposes only the one it means.",
	}, a.putProposal)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "create-draft", Method: "POST", Path: "/platform-experiments/drafts",
		Summary: "Start planning a platform experiment from one sentence", Tags: []string{"platform-experiments"},
		DefaultStatus: 201,
		Description: "Creates a platform experiment in status draft from goal, one sentence saying what the caller wants to find out. " +
			"A draft has no budget, no metrics, and one agent, its planner, which the platform runs (role planner, in the roster like any agent) " +
			"and nobody else joins: the planner works out how to measure the goal and proposes the metrics, " +
			"the description, the hardware, the budget and the agents, and it opens only when that proposal is accepted: by a person, or by the platform as proposed at accepts_at. Until then its max_agents is the most researchers a proposal may name: max_agentsandboxes less the coordinator and the baseline agent an accepted draft is given. A draft takes no job, so nothing is measured " +
			"and no compute is spent until then. Its name is the goal cut short until the proposal names it. POST /platform-experiments creates one whole instead, " +
			"and states what a run costs itself. Closing a draft (POST /platform-experiments/{id}/close) abandons it. " +
			"Its planner and later its agents keep their code in repository: the one the request names, checked as on POST /platform-experiments, or a private one the platform creates on the GitHub account the org connected. " +
			"repository_account says which account when the org connected several. 422 github_not_connected, repository_account_required and installation_not_connected as on POST /platform-experiments. " +
			"A repository the platform creates is created after this answers: the draft is answered without repository and with status_reason repository_creating, and its planner starts once the repository is there. " +
			"When GitHub does not create it the draft is closed with close_reason repository_not_created, and its status_reason message says what GitHub answered. " +
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

func (a planningAPI) acceptDraft(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body platformexperiments.AcceptRequest
}) (*struct{ Body *domain.PlatformExperiment }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	caller, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	pe, err := a.experiments.Accept(ctx, in.ID, in.Body, caller.Actor())
	if err != nil {
		return nil, planningRefusal(err)
	}
	return a.answer(ctx, pe)
}

func (a planningAPI) holdDraft(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body *domain.PlatformExperiment }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	pe, err := a.experiments.Hold(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	return a.answer(ctx, pe)
}

func (a planningAPI) putProposal(ctx context.Context, in *struct {
	ID    string `path:"id"`
	Check bool   `query:"check" doc:"Check the proposal as a proposal is checked and store nothing: the answer is the draft as it stands, or 422 with every problem."`
	Body  platformexperiments.Proposal
}) (*struct{ Body *domain.PlatformExperiment }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	caller, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	pe, err := a.experiments.Propose(ctx, in.ID, caller, &in.Body, in.Check)
	if err != nil {
		return nil, planningRefusal(err)
	}
	return a.answer(ctx, pe)
}

type createDraftRequest struct {
	ProjectID         string `json:"project_id"`
	Goal              string `json:"goal" doc:"What you want to find out, in a sentence or two. At most 2000 bytes."`
	Repository        string `json:"repository,omitempty" required:"false" doc:"owner/name on GitHub, covered by an installation the org connected, created from the experiment template when it does not exist. Left out, the platform creates a private repository on the GitHub account the org connected."`
	Branch            string `json:"branch,omitempty" required:"false" doc:"The branch of repository its agents clone and start from. Required with repository, and refused without it: a repository the platform creates is worked on at its default branch."`
	RepositoryAccount string `json:"repository_account,omitempty" required:"false" doc:"The connected GitHub account the new repository is created on. Required only when the org connected several."`
	Continues         string `json:"continues,omitempty" required:"false" doc:"The id of a closed platform experiment of the same project to carry on, which keeps its code in the repository this one names. Its baseline starts from the commit of that run's baseline job, so both are measured by the same harness. It keeps the metric keys and directions of that run: 422 invalid_metrics for others. Left out, it starts fresh. 422 invalid_continues for one that is no closed platform experiment of the project, keeps its code in another repository or declared no baseline."`
}

func (a planningAPI) createDraft(ctx context.Context, in *struct {
	Body createDraftRequest
}) (*struct{ Body *domain.PlatformExperiment }, error) {
	if in.Body.ProjectID == "" {
		return nil, huma.Error400BadRequest("project_id is required")
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
