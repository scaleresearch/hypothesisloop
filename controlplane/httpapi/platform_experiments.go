package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
)

type platformExperimentsAPI struct {
	experiments *platformexperiments.Service
	access      *auth.Access
	logger      *zap.Logger
}

// registerPlatformExperiments registers the platform experiment operations on the shared Huma doc.
func registerPlatformExperiments(doc *Doc, experiments *platformexperiments.Service, logger *zap.Logger) {
	a := platformExperimentsAPI{experiments: experiments, access: doc.Access, logger: logger}

	// ---- platform experiments ----

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "get-experiment-file", Method: "GET", Path: "/projects/{id}/experiment-file",
		Summary: "Read what a repository's experiment file states", Tags: []string{"platform-experiments"},
		Description: "What " + domain.ExperimentFile + " at the head of the default branch of repository states of a platform experiment: name, description, metrics, " +
			"report_interval_seconds and stages, as POST /platform-experiments with from_repo would take them. Nothing is created. " +
			"The org of the project must reach the repository, with the refusals of that create: 422 no_experiment_file when the repository has none, " +
			"invalid_experiment_file when it cannot be used, and why.",
	}, a.getExperimentFile)

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "check-repository", Method: "GET", Path: "/projects/{id}/repository-check",
		Summary: "Check where a platform experiment would keep its code, before creating it", Tags: []string{"platform-experiments"},
		Description: "Runs the repository checks of POST /platform-experiments and POST /platform-experiments/drafts for repository and repository_account as they would be sent, and creates nothing. " +
			"A repository named must be reachable through an installation the org connected and the App allowed to push to it: it is answered as GitHub names it, with repository_url. " +
			"With none named, the account a new repository would be created on is answered in repository_account, after GitHub confirms the App is still installed there and has granted " +
			"every permission the App asks for. " +
			"Refused with the reasons of those creates: 422 github_not_connected, repository_account_required, installation_not_connected, installation_gone, installation_suspended, github_permission_missing, " +
			"repository_not_reachable, invalid_repository, invalid_request; 502 github_failed; on a plain git host 422 repository_required and repository_not_reachable.",
	}, a.checkRepository)

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "create-platform-experiment", Method: "POST", Path: "/platform-experiments",
		Summary: "Create a platform experiment", Tags: []string{"platform-experiments"},
		DefaultStatus: 201,
		Description: "Operator/dashboard endpoint. project_id, name, description, metrics, budget_accelerator_hours, max_agents and duration_hours are required. " +
			"max_agents is how many researchers (agents of role competitor, whatever their agent type) may be at work at once, and counts nobody else: no agent of role baseline, which measures the baseline before the run, no reviewer, no coordinator and no planner. " +
			"researchers, optional, is who the run starts with and is to keep at work: an entry each agent type and model with its count, [{agent_type, model, hyperparameters, count}], at most max_agents in all. " +
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
			"coordinator_hosted: true has the platform run the coordinator as one more machine beside the agents', on coordinator_model or the platform's coordinator default: " +
			"it holds this platform experiment's coordinator token, is no agent of it, holds no quota and is not ranked. " + platformexperiments.CoordinatorWanted + " " +
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
			"With coordinator_hosted: true it also creates, in the same write, one hosted researcher for each place of researchers, of its entry's agent type, model and hyperparameters, whose machines are asked for when the run starts. While the run goes on the platform keeps them at work: it adds a new researcher in the place of one that finished or whose machine stopped with no restart left, with its agent type, model and hyperparameters, until less of the run is left than one trial of the baseline job (job_estimated_duration_hours, at least 15 minutes) or the budget its researchers left unspent holds no trial for one, and a stage boundary lowers the counts by the researchers it cuts, so none of them is replaced. agent.replaced tells each researcher added. " +
			"It also creates one hosted agent of role baseline, named by its role, as accepting a draft does: its machine is asked for at once, it measures the baseline, and the platform declares the job it leaves when it finishes, so status_reason reads baseline_measuring from the first answer and nobody adds a baseline agent. 409 max_hosted_reached where the platform's limit holds no machine for it beside the coordinator's. " +
			"Without coordinator_hosted this call creates the experiment and no agent, and the platform adds none: every researcher, and any other agent, joins by its own POST /platform-experiments/{id}/agents. " +
			"max_hosted in the answer is how many machines the platform runs for this platform experiment at once, its hosted agents of every role and its coordinator together: " +
			"one of them is kept for an agent of role baseline, and an agent that finished holds none. " +
			"GET /platform-experiments/{id} shows the coordinator's model and what became of its machine.",
	}, a.createPlatformExperiment)

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "get-hosting", Method: "GET", Path: "/hosting",
		Summary: "Whether the platform can host agents now", Tags: []string{"platform-experiments"},
		Description: "available is true while the deployment has a sandbox provider to run hosted machines on. " +
			"While it is false, unavailable says why, and every request that would host an agent, a draft's planner or a coordinator is refused with 503 and the same reason.",
	}, a.getHosting)

	register(doc, auth.AudienceAgent, huma.Operation{
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
	}, a.listPlatformExperiments)

	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-job-cost", Method: "GET", Path: "/experiments/{id}/cost",
		Summary: "Get what a job actually cost", Tags: []string{"platform-experiments"},
		Description: "Actual consumption, not the admission-time estimate: confirmed-alive hours and the " +
			"accelerator/CPU hours billed for them. `settled` distinguishes a final bill from a running total. " +
			"The estimate is returned alongside so the two can be compared.",
	}, a.getJobCost)

	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment", Method: "GET", Path: "/platform-experiments/{id}",
		Summary: "Get a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Returns status, budget, the stage ladder, the list of signed-up agents and prior_runs, the closed runs of the same project and repository that its agents and coordinator read as their own. consumed_accelerator_hours is what it has spent of its budget, the whole spend once it is closed. status_reason says why it is in its status, in every status. duration_hours count from the moment the run starts, which a running one holds in starts_at, and ends_at is that start plus duration_hours. A running one reads agents_starting while no researcher's machine is running yet: the first one that is sets started_at, and the platform closes it at agents_due_at (close_reason agents_not_started) when none comes up. From then it reads running, and researchers_done once every researcher has finished, was cut or lost its machine with no restart left: nobody is left to submit a job, the platform still closes it only at ends_at, and waits_for names who writes the summary and closes it before, its coordinator where the platform hosts one and its owner otherwise. A closed one reads its close_reason. An open one says why it is not running yet. Without a baseline that is where the measuring stands: baseline_measuring (an agent of role baseline is at work, and the platform declares its job when it finishes), baseline_required (none is at work and none left a job to declare: one is added), baseline_undeclared (they left several jobs and one is declared with PATCH), baseline_declaring (the platform is about to declare the one job), or what the platform's declaration of that one job is refused for, by the reasons of PATCH baseline_job_id: budget_below_baseline, baseline_breaks_constraint, baseline_missing_metric. With a baseline: starts_at_ahead, no_researchers, budget_spent, or starting when nothing is missing: the request that declares the baseline or adds the researcher starts the run itself. The message says what the state waits for and who acts, with the hl command, and to an agent it begins with that agent's own next step: an agent of role baseline is told to measure, and any other that its jobs are refused until the start. Nothing else says what an open platform experiment waits for.repository is the code it works on, owner/name on GitHub, " +
			"and repository_url the address agents clone and jobs check out. Both are absent when it names none. " +
			"One made from a sentence carries goal, and while it is a draft what its planner proposed (proposal, proposed_at) and what a person asked to change (revisions). Once accepted it keeps proposal as proposed, with accepted_at, accepted_by (absent when the platform accepted it by itself) and accepted_answers, the answers given to proposal.questions. " +
			"For a draft, " +
			"status_reason says where a draft stands: planning, revising, proposed (proposed_at is set and it waits to be accepted) or declined (declined_at and declined_reason are set, there is nothing to accept, and it waits to be revised or closed). Either answer waits until proposal_expires_at, when the platform closes the draft.",
	}, a.getPlatformExperiment)

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "update-platform-experiment", Method: "PATCH", Path: "/platform-experiments/{id}",
		Summary: "Change fields of a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Takes only the fields that change; a field left out keeps its value, and a body with none answers 422 nothing_to_change. " +
			"While the experiment is open every field of the body can change, `metrics` among them, so a mis-specified ranking metric is fixed in place. " +
			"Once it runs only `name`, `description`, `duration_hours`, `max_agents` and the baseline (`baseline_job_id` with `baseline_reason`) do, because admission and the stage ladder have acted on the rest: a body that carries another field answers 409 experiment_running and names it. " +
			"A max_agents of a running one changes the places nobody took: a researcher added in one is given an even part, with the researchers at work, of what all researchers left unspent, and a max_agents raised so far that the hours its researchers left unspent, split over max_agents, would not hold one trial of the baseline each (the job again at job_estimated_duration_hours) is 422 budget_below_baseline. " +
			"A duration_hours of a running one moves the end of its run, still counted from its start, and one that ends it before now is 422 invalid_length. A new description or duration_hours reaches every agent of it as platform_experiment.description. " +
			"409 experiment_not_editable for a draft or a closed one. The experiment as patched is checked like a create and refused with the same 422: " +
			"invalid_experiment (an empty name or description among them), invalid_metrics, invalid_cost, invalid_agents, invalid_length, " +
			"A max_agents lowered below the researchers (agents of role competitor) at work of a running one cuts the newest of them, the count a join is held to: each winds down as a cut researcher does, told by agent.cut with detail max_agents_lowered, and nobody is added in its place; researchers the body does not state are lowered to it from the last entry. " +
			"Before the run starts 409 max_agents_below_researchers refuses one under the researchers signed up, which the message gives. 409 researchers_below_at_work for researchers that want fewer of an agent type and model than are at work. researchers above max_agents in all is 422 invalid_agents. " +
			"run_too_long for a duration_hours over the platform's quota.max_platform_experiment_days. A starts_at in the body moves baseline_due_at with it, a starts_at past being now. " +
			"`baseline_job_id` declares the baseline while the experiment is open, and nothing else is needed in the body. The platform makes this declaration itself, by the same check, when an agent of role baseline finishes naming the job (baseline_job_id of its finish) or, naming none, the baseline agents that finished left exactly one COMPLETED job that reports the ranking metric, and again in the patch that clears what that declaration was refused for (a raised budget_accelerator_hours): a caller declares only where a choice is needed, between several jobs or after a redo. The platform writes what it holds of the job into the description, a block that begins `" + platformexperiments.MeasuredBaselineHeader + "` and states job, code_ref, env (the job's settings, when it has any) and measured (the last value the job reported on each metric but the references), then a line for each value a COMPLETED job reported on a reference, its job and whether that job met the constraints (only one that did counts), and the references no completed job reported, as not measured, and keeps that block through every later patch of the description. With a researcher joined and no starts_at ahead the run starts in this same request and the answer reads running; otherwise it starts with the request that adds the researcher, or at starts_at. The start closes signup and gives each agent present, those of role baseline apart, its share of the first stage's part of the budget, less what measuring the baseline spent. The start also settles `stages`: a boundary cuts nobody from fewer than 5 researchers (role competitor), so a run started with fewer has one stage of its whole length, with the last declared stage's max_job_hours, and each researcher holds its whole share from the start. With 5 or more the ladder is as declared. status_reason in the answer says what an open experiment still waits for: no_researchers until one joined, budget_spent when the baseline left nothing to share. The job is read from the platform's own records and the declaration refused with 422: " +
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
			"A coordinator's token changes `description`, `baseline_job_id` and `baseline_reason` only: a body that carries any other field answers 403 coordinator_changes_description_only. Its owner raises max_agents, and the coordinator then adds researchers in the places that gives; researchers, which the platform keeps where it runs them, are its owner's to change too. " +
			"The project, the repository, the agent image, the stages and the default quota tier are fixed at creation and are no fields of this body.",
	}, a.updatePlatformExperiment)

	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment-signup", Method: "GET", Path: "/platform-experiments/{id}/signups/{agent_id}",
		Summary: "Read an agent's signup", Tags: []string{"platform-experiments"},
		Description: "What an agent was signed up as: its name, its role, which decides whether it is ranked and cut, and the flavor, " +
			"model and hyperparameters it runs with. An agent started with its token alone reads its own here. " +
			"404 if that agent is not signed up to this platform experiment — not signed up is not a role.",
	}, a.getPlatformExperimentSignup)

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "close-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/close",
		Summary: "Close a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Terminal: every still-running job is evicted (reason experiment_closed) and " +
			"no further submission is accepted. Optional top_results records the final standings; " +
			"omitted, standings are derived from observed metrics. The answer is the platform experiment as closed: close_reason closed_by_request with closed_by, " +
			"and close_message, the optional reason, which status_reason's message, the record of the close in summary and the detail of the platform_experiment.status event of the close repeat. " +
			"A run the platform closed at its ends_at reads end_time_reached, an open one it closed at its baseline_due_at baseline_limit_reached and a running one it closed at its agents_due_at agents_not_started. status_reason carries the same reason with a sentence. " +
			"One that was open or ran and has no summary is given one by the platform in the same write: the close_reason, the jobs submitted, what each agent last did and how its run ended, and the spend. Its coordinator, on its last turn, writes over it. " +
			"409 invalid_transition for one already closed, which changes nothing.",
	}, a.closePlatformExperiment)

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "remove-platform-experiment", Method: "DELETE", Path: "/platform-experiments/{id}", DefaultStatus: http.StatusNoContent,
		Summary: "Remove a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Takes the platform experiment off the platform for every account: its lists, its reads and those of everything in it, its hypotheses, jobs, " +
			"standings and watch stream, answer 404 from then on, to staff too, and the tokens of its agents and coordinator are refused. Whoever may close it removes it. " +
			"One not closed yet is closed first as POST /platform-experiments/{id}/close closes it, every running job evicted and every agent stopped, with close_reason removed, " +
			"in the same write. Nothing it holds is deleted, and what it spent counts toward every total as before. " +
			"Its agents and its coordinator are told with platform_experiment.removed. 404 for one removed before.",
	}, a.removePlatformExperiment)

	register(doc, auth.AudienceStaff, huma.Operation{
		OperationID: "reopen-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/reopen",
		Summary: "Set a closed platform experiment running again", Tags: []string{"platform-experiments"},
		Description: "Staff only. A run closed early, for one to redeploy the platform, continues as the same run until ends_at: its hypotheses, jobs, branches, brief, baseline and agents stay as the close left them. " +
			"closed_at, close_reason, closed_by, close_message, the record of the close and its placements go, and the answer reads running with the new ends_at and duration_hours counted from its start. " +
			"Every agent whose run ended only with the close is at work again, its machine asked for and its token accepted, and so is a hosted coordinator. One that finished or was cut before the close stays as it ended. " +
			"Jobs the close evicted stay evicted. Agents are told with platform_experiment.status running and platform_experiment.description, as the run's length changed. " +
			"coordinator_model moves a hosted coordinator to that model, checked as on a create, and left out keeps the one it has. " +
			"409 invalid_transition for one that is not closed, one that closed before its run started, and one a later run continues. 422 invalid_length for an ends_at that has passed, run_too_long for one past quota.max_platform_experiment_days from the run's start. " +
			"400 invalid_request for a coordinator_model on a run with no hosted coordinator.",
	}, a.reopenPlatformExperiment)

	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment-results", Method: "GET", Path: "/platform-experiments/{id}/results",
		Summary: "Final standings and operator summary", Tags: []string{"platform-experiments"},
		Description: "Per-metric standings, best-first, derived from the metrics store on every read " +
			"(never frozen into PostgreSQL), plus the operator's narrative summary. bests lists, for every " +
			"declared metric whatever its role, each reporting agent's best raw value and the job that set it, unranked. " +
			"Each ranking metric carries baseline, the last value the declared baseline job (baseline_job_id) reported on it, and each standing beats_baseline, whether its best is strictly better: they follow the baseline when it is raised. " +
			"references lists each metric of role reference, an outside system the goal compares against, with the value of every COMPLETED job that reported it, its agent, ran_on, the machine that job ran on, as each standing carries ran_on (a result compares with a reference measured on the same machine), and constraints_met: true, false when the job broke a constraint metric, unknown when it did not report one. best, leader and beaten count only values whose constraints_met is true. A reference with no values was not measured.",
	}, a.getPlatformExperimentResults)

}

func (a platformExperimentsAPI) getExperimentFile(ctx context.Context, in *struct {
	ID         string `path:"id"`
	Repository string `query:"repository" required:"true" doc:"owner/name on GitHub"`
}) (*struct {
	Body *platformexperiments.ExperimentFile
}, error) {
	if err := a.access.Project(ctx, in.ID); err != nil {
		return nil, err
	}
	file, err := a.experiments.ReadExperimentFile(ctx, in.ID, in.Repository)
	if err != nil {
		return nil, err
	}
	return &struct {
		Body *platformexperiments.ExperimentFile
	}{Body: file}, nil
}

func (a platformExperimentsAPI) checkRepository(ctx context.Context, in *struct {
	ID                string `path:"id"`
	Repository        string `query:"repository" doc:"owner/name, as the create would name it. Left out, a new repository is checked."`
	RepositoryAccount string `query:"repository_account" doc:"The connected account a new repository would be created on, as the create would say it."`
}) (*struct {
	Body platformexperiments.RepositoryCheck
}, error) {
	if err := a.access.Project(ctx, in.ID); err != nil {
		return nil, err
	}
	check, err := a.experiments.CheckRepository(ctx, in.ID, in.Repository, in.RepositoryAccount)
	if err != nil {
		return nil, err
	}
	return &struct {
		Body platformexperiments.RepositoryCheck
	}{Body: check}, nil
}

func (a platformExperimentsAPI) createPlatformExperiment(ctx context.Context, in *struct {
	Body platformexperiments.CreatePlatformExperimentRequest
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
	pe, err := a.experiments.CreateExperiment(ctx, in.Body, caller.Actor())
	if err != nil {
		return nil, err
	}
	return a.answer(ctx, pe)
}

func (a platformExperimentsAPI) getHosting(ctx context.Context, _ *struct{}) (*struct{ Body platformexperiments.Hosting }, error) {
	if _, err := a.access.Account(ctx); err != nil {
		return nil, err
	}
	return &struct{ Body platformexperiments.Hosting }{Body: a.experiments.Hosting()}, nil
}

func (a platformExperimentsAPI) listPlatformExperiments(ctx context.Context, in *struct {
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
	if !platformexperiments.ValidListSort(in.Sort) {
		return nil, huma.Error400BadRequest("unknown sort field " + in.Sort)
	}
	if in.Limit < 0 || in.Limit > platformexperiments.MaxListLimit || in.Offset < 0 {
		return nil, refusal.Refuse(http.StatusUnprocessableEntity, "invalid_page", "limit must be 1 to "+strconv.Itoa(platformexperiments.MaxListLimit)+" and offset must not be negative")
	}
	scope, withShared, err := a.access.PlatformExperimentListScope(ctx, in.ProjectID)
	if err != nil {
		return nil, err
	}
	query := platformexperiments.ListQuery{
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
		query.Shared = &only
	}
	pes, total, err := a.experiments.List(ctx, query)
	if err != nil {
		return nil, err
	}
	return &struct {
		Body       []*domain.PlatformExperiment
		TotalCount int `header:"X-Total-Count"`
	}{Body: pes, TotalCount: total}, nil
}

func (a platformExperimentsAPI) getJobCost(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body *platformexperiments.JobCost }, error) {
	if err := a.access.Experiment(ctx, in.ID); err != nil {
		return nil, err
	}
	cost, err := a.experiments.JobCost(ctx, in.ID)
	if err != nil {
		return nil, auth.Internal(a.logger, "get job cost", err, zap.String("id", in.ID))
	}
	if cost == nil {
		return nil, huma.Error404NotFound("experiment not found")
	}
	return &struct{ Body *platformexperiments.JobCost }{Body: cost}, nil
}

func (a platformExperimentsAPI) getPlatformExperiment(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body *domain.PlatformExperiment }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	pe, err := a.experiments.Get(ctx, in.ID, func(id string) bool { return a.access.PlatformExperiment(ctx, id) == nil })
	if errors.Is(err, platformexperiments.ErrPlatformExperimentNotFound) {
		return nil, huma.Error404NotFound(err.Error())
	}
	if err != nil {
		return nil, err
	}
	return a.answer(ctx, pe)
}

func (a platformExperimentsAPI) updatePlatformExperiment(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body platformexperiments.PlatformExperimentPatch
}) (*struct{ Body *domain.PlatformExperiment }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	caller, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	pe, err := a.experiments.Patch(ctx, in.ID, in.Body, caller)
	if err != nil {
		return nil, err
	}
	answered := *pe
	return a.answer(ctx, &answered)
}

func (a platformExperimentsAPI) getPlatformExperimentSignup(ctx context.Context, in *struct {
	ID      string `path:"id"`
	AgentID string `path:"agent_id"`
}) (*struct{ Body signupReply }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	signup, err := a.experiments.AgentSignup(ctx, in.ID, in.AgentID)
	if errors.Is(err, platformexperiments.ErrNotSignedUp) {
		return nil, huma.Error404NotFound(err.Error())
	}
	if err != nil {
		return nil, err
	}
	return &struct{ Body signupReply }{Body: signupReply{
		AgentID: signup.AgentID, Name: signup.Name, Role: string(signup.Role), Flavor: signup.AgentType,
		Model: signup.Model, Hyperparameters: signup.Hyperparameters, FinishedAt: signup.FinishedAt, Ended: signup.Ended, CutReason: signup.CutReason,
	}}, nil
}

func (a platformExperimentsAPI) closePlatformExperiment(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body *struct {
		Reason     string                            `json:"reason,omitempty" maxLength:"2000" doc:"Why it is closed, in a sentence or two. Kept as close_message and told to its coordinator and agents with the close."`
		TopResults []platformexperiments.AgentResult `json:"top_results,omitempty" doc:"Best-first final standings (up to 3). Omit to derive standings from the metrics store."`
	}
}) (*struct{ Body *domain.PlatformExperiment }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	var topResults []platformexperiments.AgentResult
	message := ""
	if in.Body != nil {
		topResults, message = in.Body.TopResults, strings.TrimSpace(in.Body.Reason)
	}
	caller, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	by := caller.Actor()
	pe, err := a.experiments.Close(ctx, in.ID, &by, message, topResults)
	if err != nil {
		return nil, refused(a.logger, "close platform experiment", err)
	}
	return a.answer(ctx, pe)
}

func (a platformExperimentsAPI) removePlatformExperiment(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{}, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	caller, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	if err := a.experiments.Remove(ctx, in.ID, caller.Actor()); err != nil {
		return nil, refused(a.logger, "remove platform experiment", err)
	}
	return &struct{}{}, nil
}

func (a platformExperimentsAPI) reopenPlatformExperiment(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		EndsAt           time.Time `json:"ends_at" doc:"When the run closes again, RFC 3339, in the future."`
		CoordinatorModel string    `json:"coordinator_model,omitempty" doc:"The model the hosted coordinator runs on from now. Left out, it keeps the one it has."`
	}
}) (*struct{ Body *domain.PlatformExperiment }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	model := ""
	if in.Body.CoordinatorModel != "" {
		if err := a.experiments.CheckCoordinatorModel(in.Body.CoordinatorModel); err != nil {
			return nil, err
		}
		model = in.Body.CoordinatorModel
	}
	pe, err := a.experiments.Reopen(ctx, in.ID, in.Body.EndsAt, model)
	if err != nil {
		return nil, refused(a.logger, "reopen platform experiment", err)
	}
	return a.answer(ctx, pe)
}

func (a platformExperimentsAPI) getPlatformExperimentResults(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct {
	Body *platformexperiments.PlatformExperimentResults
}, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	results, err := a.experiments.Results(ctx, in.ID)
	if errors.Is(err, platformexperiments.ErrPlatformExperimentNotFound) {
		return nil, huma.Error404NotFound(err.Error())
	}
	if err != nil {
		return nil, auth.Internal(a.logger, "get platform experiment results", err, zap.String("id", in.ID))
	}
	return &struct {
		Body *platformexperiments.PlatformExperimentResults
	}{Body: results}, nil
}

// answer is how a route answers one platform experiment: described for its reader, with its
// researchers at work and where its repository is served.
func (a platformExperimentsAPI) answer(ctx context.Context, pe *domain.PlatformExperiment) (*struct{ Body *domain.PlatformExperiment }, error) {
	reader, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	if err := a.experiments.Describe(ctx, pe, reader); err != nil {
		return nil, auth.Internal(a.logger, "describe platform experiment", err, zap.String("id", pe.ID))
	}
	atWork, err := a.experiments.ResearchersAtWork(ctx, pe.ID)
	if err != nil {
		return nil, auth.Internal(a.logger, "count researchers at work", err, zap.String("id", pe.ID))
	}
	counts := pe.Researchers.AtWorkOf(atWork)
	pe.ResearchersAtWork = &counts
	if pe.Repository != "" {
		if pe.GitURL, err = a.experiments.GitURL(pe.ID); err != nil {
			return nil, auth.Internal(a.logger, "describe platform experiment", err, zap.String("id", pe.ID))
		}
	}
	return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
}

// signupReply is an agent's signup as any caller in its platform experiment reads it.
type signupReply struct {
	AgentID         string         `json:"agent_id"`
	Name            string         `json:"name" doc:"The name the agent's branches carry: hl/<platform experiment>/<name>/..."`
	Role            string         `json:"role"`
	Flavor          string         `json:"flavor" doc:"The agent type it runs, by id."`
	Model           string         `json:"model" doc:"Its own model, empty when it runs on its agent type's. What it runs with is its instructions: GET /platform-experiments/{id}/agents/{agent_id}/instructions."`
	Hyperparameters map[string]any `json:"hyperparameters" doc:"Its own hyperparameters, null when it runs with its agent type's."`
	FinishedAt      *time.Time     `json:"finished_at,omitempty" doc:"When the agent said it has nothing left to do."`
	Ended           domain.RunEnd  `json:"ended" enum:",closed,cut,finished,planned" doc:"Why the agent's run here is over, the empty string while it goes on: the platform experiment closed, the agent was cut, it finished, or it is a draft's planner whose proposal or decline is stored. A machine the platform runs for the agent is told to stop for this and nothing else."`
	CutReason       string         `json:"cut_reason,omitempty" doc:"Why the platform cut it other than at a stage boundary: max_agents_lowered for one of the newest researchers above a max_agents lowered below those at work. Absent for a stage cut."`
}
