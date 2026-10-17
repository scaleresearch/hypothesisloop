package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/scope"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
)

type joinedAgents struct {
	Agents []platformexperiments.JoinedAgent `json:"agents"`
}

type finishedAgent struct {
	Agents         []platformexperiments.JoinedAgent    `json:"agents"`
	OpenHypotheses []platformexperiments.OpenHypothesis `json:"open_hypotheses" doc:"The hypotheses still open that are the agent's verdict to give, newest first, at most 200: its own, then the ideas of people no agent owns that a completed job of its tested. Each stays open for good unless the agent gives its verdict (POST /hypotheses/{id}/status) while its token still works. Empty when it left none. Nothing is refused for them."`
}

type experimentAgentsAPI struct {
	experiments *platformexperiments.Service
	graph       platformexperiments.Hypotheses
	launch      platformexperiments.AgentLaunch
	access      *auth.Access
	logger      *zap.Logger
}

// registerExperimentAgents registers how agents join and leave a platform experiment. graph is
// where a finish reads what the agent left open, and launch what joining needs beyond the store.
func registerExperimentAgents(doc *Doc, experiments *platformexperiments.Service, graph platformexperiments.Hypotheses, launch platformexperiments.AgentLaunch, logger *zap.Logger) {
	if launch.DefaultModel == "" || launch.SigningSecret == "" || launch.MaxAgentSandboxes <= 0 || graph == nil {
		panic("httpapi.registerExperimentAgents: every field of the agent launch and the hypothesis graph are required")
	}
	a := experimentAgentsAPI{experiments: experiments, graph: graph, launch: launch, access: doc.Access, logger: logger}
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "add-platform-experiment-agent", Method: "POST", Path: "/platform-experiments/{id}/agents",
		Summary: "Add one agent to a platform experiment", Tags: []string{"platform-experiments"},
		DefaultStatus: 201,
		Description: "The only way an agent joins, while the experiment is open: the platform starts the run once its baseline is declared, and quota is divided then over the agents present, those of role baseline apart. " +
			"While it runs, its coordinator or a person adds a researcher (role competitor, and no other role) in the place of one that finished, was cut or lost its machine, or in one its owner gained by raising max_agents: it is given an even part, with the researchers at work, of what all researchers left unspent, taken first from what the ones that ended left and then from the researchers at work that hold more unspent than that part, in proportion to what they hold above it, so none keeps less than the newcomer and none gives back what it used, and 409 no_share_left when that part would not hold one trial of the baseline (the job again at job_estimated_duration_hours). An agent's token adds no agent then (403). Adding the researcher a declared baseline still waited for starts the run in this request. " +
			"Creates one agent with a generated id and signs it up; or, with agent_id, signs up that registered agent. For several agents, call once for each. " +
			"An agent's name is unique in the platform experiment and, when the platform gives it, says its role: 409 name_taken for a stated name, or a registered agent's, that another agent of it carries. " +
			"The answer is agents, a list holding that one agent. " +
			"in_agentsandbox=true has the platform start a machine for the agent when the experiment starts and keep it until the experiment closes, the agent is cut or it finishes. " +
			"Until the start the agent answered carries status_reason, the reason the platform experiment is not running yet, and its instance reads waiting: no machine exists before the start. " +
			"409 max_agentsandboxes_reached beyond max_agentsandboxes machines at once, of which one is kept for an agent of role baseline: when one finished without a baseline that can be declared, or its machine stopped with no restart left, the next is added in its place. " +
			"agent_type and model are resolved here and stored: the answer shows what the agent runs. " +
			"in_agentsandbox=false returns the agent's token: run the agent yourself with it as HL_TOKEN and the API's address as API_URL, which is all it is started with. " +
			"The token acts as that agent in that platform experiment and nowhere else, and asking again with agent_id returns it again. A coordinator's token is returned none: a person or an API key of the org asks for it. " +
			"role decides ranking: \"competitor\" (default) is a researcher, whatever its agent type: ranked, cut-eligible and counted against max_agents; \"baseline\" and " +
			"\"reviewer\" are none of the three, though their jobs are billed and gated identically. " +
			"409 max_agents_reached for a researcher past max_agents, which counts the researchers at work, none that finished, was cut, was replaced or lost its machine with no restart left, and no agent of another role and no coordinator. 409 signup_closed for any role but researcher once the run started, and for every one once it closed. " +
			"An agent of role baseline measures the baseline before the start: its machine, when sandboxed, starts at once, it holds a quota from the moment it joins (half the budget, which no researcher can spend before the start, so a failed measurement can be run again; it follows a later change of the budget while the experiment is open)," +
			"and its job is the only one an open experiment accepts. It takes no share at the start, and what it spent is taken from the budget before the others' shares are carved.",
	}, a.add)

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "list-platform-experiment-agents", Method: "GET", Path: "/platform-experiments/{id}/agents",
		Summary: "List the agents of a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Every agent that joined, in the order they did, with its role, what it was launched with and actor, whose credential added it. No token is listed. " +
			"A sandboxed agent carries instance: the name its machine was last asked for under, how many times it exited and was started again, " +
			"and once it exited with no restart left, stopped=true with exited_at and exit_log, the end of what its command wrote. " +
			"A sandboxed agent whose machine is up carries activity {text, at}: what the agent is doing now, in a short sentence its harness writes from the tool call in flight (\"thinking\", \"running hl job submit\", \"waiting for platform updates\"), and when the platform first read that sentence. Only the latest is kept, it is read from the machine about every 15 seconds, and it is absent while no machine is up. The coordinator's is coordinator.activity on GET /platform-experiments/{id}, and a draft's planner's is activity there. " +
			"Every agent carries in_flight {jobs, queued, running}: how many of its jobs are queued or running now. 0 is an agent with nothing of its own on the platform, whatever its activity says. " +
			"While the platform experiment is open or running, an agent whose run goes on carries messages {left_this_hour, next_at}, how many more messages the caller may send it this hour, and in a running one minutes_without_job_in_flight. " +
			"A sandboxed one whose machine reports carries what its harness last said of that machine: subagents, the subagents running, turn {state working or waiting, since}, and model_allowance {utilization, resets_at, refused_until}. GET /platform-experiments/{id}/situation reads the same in one answer with the run's clock, objective, capacity and budget. " +
			"While the platform experiment is open, every agent but those of role baseline waits for the start and carries status_reason: the reason the platform experiment is not running yet, and that no machine is asked for the agent before it is. Its instance reads waiting until then. One that closed before its run started leaves each of them status_reason never_launched, whose message gives the close_reason and why it closed.A sandboxed agent whose machine is asked for and not running yet, its instance reading requested or starting, carries status_reason instance_starting with how long ago the machine was asked for: a start in progress, and a failed one reads stopped with end_reason.",
	}, a.list)

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "remove-platform-experiment-agent", Method: "DELETE", Path: "/platform-experiments/{id}/agents/{agent_id}",
		Summary: "Remove an agent from a platform experiment that has not started", Tags: []string{"platform-experiments"},
		DefaultStatus: http.StatusNoContent,
		Description: "Takes the agent out while the experiment is open: its token stops working and, if the platform runs it, its machine is taken down. " +
			"Only its place goes: the agent stays in the org and joins again with agent_id. " +
			"409 once the experiment has started, when the roster is what quota was divided over: from then on an agent leaves by being cut or by the experiment closing. " +
			"409 work_unfinished while the agent has a job queued or running, which an agent of role baseline may: the quota that job is charged to goes with the agent, so the job is cancelled or ends first. " +
			"A coordinator's token removes only an agent it added itself (actor.kind coordinator in the roster): 403 agent_not_added_by_coordinator for one a person or an API key added, which its owner removes.",
	}, a.remove)

	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "finish-platform-experiment-agent", Method: "POST", Path: "/platform-experiments/{id}/agents/{agent_id}/finish",
		Summary: "Say that an agent has nothing left to do", Tags: []string{"platform-experiments"},
		DefaultStatus: 200,
		Description: "Called by the agent itself, with its own token, in a running platform experiment, and by an agent of role baseline also in an open one, where it measures the baseline. From then its machine, if the platform runs it, is taken down after its last turn, " +
			"its signup reads finished_at and finished_reason, and agent.finished is sent. The answer is agents, a list holding that one agent as finished, and open_hypotheses, the id and text of each hypothesis of the agent that still has no verdict: the finish is taken whatever they are. Its quota, jobs and findings stay as they are, and its token stops working two minutes later. " +
			"A researcher's finished_reason, which agent.finished carries to the coordinator, ends with the time and quota it left, after the blocker it named. " +
			"409 time_and_quota_left, saying how much of each is left, for a researcher that has more than a tenth of the run's time and of its quota left and names no blocker. " +
			"409 work_unfinished while it has a job queued or running or a completed job without its summary, 409 experiment_closed once the platform experiment closed, which ended the agent's run already and leaves nothing to finish, 409 not_running in one not started yet for any role but baseline. Calling it again changes nothing. " +
			"The finish of an agent of role baseline in an open platform experiment hands over what it measured: with baseline_job_id the platform declares that job as the baseline in this request, as PATCH /platform-experiments/{id} with baseline_job_id does, and starts the run when nothing else is missing. " +
			"The job must be the agent's own and pass every check of that PATCH, or the finish is refused with its 422 (no_baseline, baseline_not_completed, baseline_without_finding, baseline_missing_metric, baseline_breaks_constraint, budget_below_baseline and the others) and nothing is stored, so the agent fixes what it says and finishes again. " +
			"422 not_a_baseline_agent for baseline_job_id from an agent of another role, 409 baseline_declared when it names another job than the baseline already declared. " +
			"Without baseline_job_id, when the baseline agents that finished left exactly one COMPLETED job that reports the ranking metric, the platform declares that job the same way. " +
			"Such a finish is answered 200 whatever became of the declaration: one that is refused, or a finish that left no such job or several, declares nothing, and the platform experiment's status_reason says which it was and who acts (baseline_required, baseline_undeclared, or the refusal of the declaration such as budget_below_baseline).",
	}, a.finish)

}

func (a experimentAgentsAPI) add(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body platformexperiments.AddAgentRequest
}) (*struct{ Body joinedAgents }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	if in.Body.AgentID != "" {
		if err := a.access.Agent(ctx, in.Body.AgentID); err != nil {
			return nil, err
		}
	}
	caller, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	out := &struct{ Body joinedAgents }{}
	if out.Body.Agents, err = a.experiments.Join(ctx, in.ID, in.Body, caller, a.launch); err != nil {
		return nil, refused(a.logger, "add agent", err)
	}
	for i, agent := range out.Body.Agents {
		// A coordinator does not act as an agent (auth.Access.ActAs), so it is handed no agent's token.
		if agent.InAgentSandbox || !caller.HoldsAccount() {
			continue
		}
		token, err := auth.Mint(a.launch.SigningSecret, scope.Agent(in.ID, agent.AgentID))
		if err != nil {
			return nil, refused(a.logger, "mint agent token", err)
		}
		out.Body.Agents[i].Token = token
	}
	return out, nil
}

func (a experimentAgentsAPI) list(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body joinedAgents }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	caller, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	writes, err := a.access.Writes(ctx, in.ID)
	if err != nil {
		return nil, auth.Internal(a.logger, "list agents", err, zap.String("id", in.ID))
	}
	out := &struct{ Body joinedAgents }{}
	out.Body.Agents, err = a.experiments.Roster(ctx, in.ID, caller, writes)
	if errors.Is(err, platformexperiments.ErrPlatformExperimentNotFound) {
		return nil, huma.Error404NotFound("platform experiment not found")
	}
	if err != nil {
		return nil, refused(a.logger, "list agents", err)
	}
	return out, nil
}

func (a experimentAgentsAPI) remove(ctx context.Context, in *struct {
	ID      string `path:"id"`
	AgentID string `path:"agent_id"`
}) (*struct{}, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	caller, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	if err := a.experiments.RemoveAgent(ctx, in.ID, in.AgentID, caller.Actor()); err != nil {
		return nil, refused(a.logger, "remove agent", err)
	}
	return &struct{}{}, nil
}

type finishAgentRequest struct {
	Reason        string `json:"reason" minLength:"1" doc:"Why nothing is left to do: quota spent, or what was tried and why no further idea is worth a job."`
	Blocker       string `json:"blocker,omitempty" doc:"What stops the agent that the time and quota it has left cannot fix. Required of a researcher with more than a tenth of the run's time and of its quota left."`
	BaselineJobID string `json:"baseline_job_id,omitempty" doc:"For an agent of role baseline: its COMPLETED job the platform declares as the baseline in this request, the control the run is ranked against. Needed when it left several jobs that report the ranking metric."`
}

func (a experimentAgentsAPI) finish(ctx context.Context, in *struct {
	ID      string `path:"id"`
	AgentID string `path:"agent_id"`
	Body    finishAgentRequest
}) (*struct{ Body finishedAgent }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	caller, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	if caller.Kind != auth.KindAgent || caller.AgentID != in.AgentID || caller.PlatformExperimentID != in.ID {
		return nil, huma.Error403Forbidden("an agent finishes itself, with its own token")
	}
	out := &struct{ Body finishedAgent }{}
	out.Body.Agents, out.Body.OpenHypotheses, err = a.experiments.Finish(ctx, a.graph, in.ID, in.AgentID, in.Body.Reason,
		strings.TrimSpace(in.Body.Blocker), strings.TrimSpace(in.Body.BaselineJobID), caller.Actor())
	if err != nil {
		return nil, refused(a.logger, "finish agent", err)
	}
	return out, nil
}
