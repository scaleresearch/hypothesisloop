package domain

import "github.com/scaleresearch/hypothesisloop/contract/platformapi"

// Event is one change as GET /watch delivers it (platformapi).
type Event = platformapi.Event

// EventKindDoc describes one kind well enough to subscribe to it without reading this file: what
// its subject and value are, what an `agent` filter keeps of it, and what it is for.
type EventKindDoc struct {
	Kind        string `json:"kind"`
	Subject     string `json:"subject"`
	Value       string `json:"value"`
	Description string `json:"description"`
	// AgentScope says what an `agent` filter keeps of this kind. It is filled in from agentScope,
	// written beside the rule Matches applies, so the documented scope and the applied one sit together.
	AgentScope string `json:"agent_scope"`
}

// EventKinds is the vocabulary of the stream, and the only list of it. The server validates a
// subscription's `kinds` against exactly this slice and GET /watch/kinds serves exactly this
// slice, so a kind cannot be documented but unaccepted, or accepted but undocumented — the two
// failures a hand-kept second list produces the first time a kind is added.
var EventKinds = []EventKindDoc{
	{Kind: platformapi.EventExperimentStatus, Subject: "experiment id", Value: "QUEUED, SUBMITTED, RUNNING, COMPLETED, FAILED, EVICTED, REJECTED, CANCELLED",
		Description: "Your job moved. detail carries why, when the platform knows, as a reason code, or the code, a colon and a sentence (code: sentence): the reason a job was evicted, rejected or requeued (no_offer: no machine to rent for shape rtx4090 in 3m0s of asking), or how a FAILED job's process ended (container_failed: container exited with code 1, then a line last output: and the last lines it said, the most recent last), which a FAILED job always has. The code is the text before the first colon, and the same split as eviction_reason on the job. A job requeued for a fault of the platform's (code_unreachable, no_offer, stuck_pending and the like) is QUEUED with that detail and runs again at no cost. The queue reason is experiment.blocked."},
	{Kind: platformapi.EventExperimentBlocked, Subject: "experiment id", Value: "the code of the queue reason (not_admitted_reason)",
		Description: "Why a job is still waiting. detail is the sentence that goes with the code (not_admitted_message). It changes while the status does not, so a stuck job has something to wait on. awaiting_admission is not sent, and an agent scope leaves out every reason that waiting clears, such as capacity_unavailable and outranked: GET /experiments has them."},
	{Kind: platformapi.EventQuotaChanged, Subject: "agent id", Value: "share_accelerator_hours of GET /quota: all the agent may spend",
		Description: "A grant or a stage move landed. What is used of the share comes from GET /quota."},
	{Kind: platformapi.EventStageBoundary, Subject: "platform experiment id", Value: "stage index",
		Description: "The elimination ladder advanced. detail is 'advanced' or 'cut'; agent_id names who was cut."},
	{Kind: platformapi.EventAgentCut, Subject: "agent id", Value: "stage index",
		Description: "This agent was cut, at a stage boundary or, with detail max_agents_lowered, as one of the newest researchers above a max_agents lowered below the researchers at work. Its own stop condition, without a GET to find out whether the cut was its."},
	{Kind: platformapi.EventAgentFinished, Subject: "agent id", Value: "empty by design",
		Description: "This agent said it has nothing left to do, and its machine stops. detail is the reason it gave, and for a researcher the blocker it named and the time and quota it left. When every agent is finished or cut the run has nobody left to wait for."},
	{Kind: platformapi.EventAgentReplaced, Subject: "agent id of the new researcher", Value: "agent id of the researcher it replaces",
		Description: "The platform added a researcher in the place of one that finished or whose machine stopped with no restart left, with its agent type, model and hyperparameters, to keep the researchers the run is to have at work (researchers on GET /platform-experiments/{id}). detail says why the place was free. Nobody adds it: the platform does, while the time and the budget left hold a trial for it. A cut researcher is not replaced."},
	{Kind: platformapi.EventAgentIdle, Subject: "agent id", Value: "new or continues",
		Description: "No job of this researcher of a running platform experiment ran on the platform for the platform's agents.idle_notice_minutes, and it has none queued or running: nothing of its own is in flight. " +
			"Sent again every agents.idle_notice_minutes while the stretch lasts: new on the first notice of a stretch, continues on each after it, and a job it starts ends the stretch. " +
			"detail says, as the platform knows them at the notice: for how long, the subagents and turn its harness last reported, how many jobs shaped like the run's baseline job the free capacity places now, the minutes left in the run and what is left of its share. Nothing is done to the agent."},
	{Kind: platformapi.EventAgentInstructions, Subject: "agent id, or coordinator", Value: "the current version of its agent type",
		Description: "The instructions this agent is served changed: the agent type it runs was edited or restored, or the platform's rules have a new version. detail is the agent type. " +
			"A pointer that carries no text: read them with GET /platform-experiments/{id}/agents/{agent_id}/instructions, or /platform-experiments/{id}/coordinator/instructions for the coordinator, whose subject and agent_id are coordinator, and apply them at the next turn boundary."},
	{Kind: platformapi.EventCapacityIdle, Subject: "platform experiment id", Value: "new or continues",
		Description: "The free capacity of a running platform experiment with time left has placed more jobs shaped like its baseline job than it has queued (fits_now beyond the queued jobs) for the platform's agents.idle_notice_minutes, whoever has jobs in flight. " +
			"Sent again every agents.idle_notice_minutes while places stay free, a reached target included: new on the first notice of a stretch, continues on each after it, and a pass that sees no place free ends the stretch. " +
			"detail says, as the platform knows them at the notice: how many places are free and for how long, the run's jobs queued and running, the minutes left in the run, and how far its best is short of or past the target. An agent filter carries none."},
	{Kind: platformapi.EventAgentMessage, Subject: "agent id, the one it was sent to, or coordinator", Value: "coordinator or owner: who it is from",
		Description: "A direct message to this agent from its platform experiment's coordinator or owner (POST /platform-experiments/{id}/messages), or from the owner to the sandboxed coordinator, whose subject and agent_id are coordinator. detail is the text, direction to act on. GET /platform-experiments/{id}/messages reads every one sent."},
	{Kind: platformapi.EventAgentActivity, Subject: "agent id, or coordinator", Value: "signed_up, left, machine or activity",
		Description: "What the run's page shows of this agent changed: it signed up (signed_up) or was removed (left), the machine the platform runs for it was asked for, reported in another state or ended (machine), or that machine said something new of what its agent does (activity). " +
			"A pointer that carries nothing: GET /platform-experiments/{id}/agents has the agent and its machine. A machine's progress is not told."},
	{Kind: platformapi.EventHypothesisNew, Subject: "hypothesis id", Value: "the kind of who registered it: agent, coordinator, user, key or staff",
		Description: "An idea entered the shared pool. agent_id is whose it is, empty on one no agent owns. The value says whether a person (user, key, staff), the coordinator or an agent registered it; the claim itself and who registered it come from GET /hypotheses/{id}."},
	{Kind: platformapi.EventHypothesisStatus, Subject: "hypothesis id", Value: "open, confirmed, refuted, inconclusive",
		Description: "A claim was settled. A settled claim is to read, not to retest."},
	{Kind: platformapi.EventHypothesisLink, Subject: "hypothesis id (the link's source)", Value: "builds_on, contradicts or related",
		Description: "A link was added to an existing hypothesis. detail is the target: a hypothesis id, or the platform experiment id for the objective. Links given at registration ride on hypothesis.new instead."},
	{Kind: platformapi.EventFindingNew, Subject: "hypothesis id", Value: "the experiment id that produced it",
		Description: "A result was attached to a claim — someone's number is now readable instead of yours being re-derived."},
	{Kind: platformapi.EventCommentNew, Subject: "hypothesis id, or the platform experiment id for the objective", Value: "empty by design",
		Description: "Someone said something on a claim, or on the objective, people included. agent_id is whose note it is, empty on one no agent owns. GET /hypotheses/{id}/comments reads it."},
	{Kind: platformapi.EventPlatformExperimentStatus, Subject: "platform experiment id", Value: "draft, open, running, closed",
		Description: "The run itself moved. 'closed' is a stop condition for every agent in it, and its detail is what whoever closed it said of why, empty when they said nothing. With a detail on any other status the status stayed and either its status_reason changed to one that waits for its coordinator, where the platform hosts one: detail is that reason, such as researchers_done once no researcher is left to work, sent within a minute of the change and once for each; " +
			"or, on a running one, the platform keeps places of its researchers empty: detail starts researchers not replaced, and names each researcher that ended before submitting a job or within 10 minutes of joining, which is never replaced, and each that waits because its pool entry had 3 replacements in the last hour (researchers_held on GET /platform-experiments/{id}). GET /platform-experiments/{id} has its message."},
	{Kind: platformapi.EventPlatformExperimentDescription, Subject: "platform experiment id", Value: "empty by design",
		Description: "The description or the run's length (duration_hours) changed. A description is unbounded and is never copied into an event: re-read both with GET /platform-experiments/{id}."},
	{Kind: platformapi.EventPlatformExperimentGoal, Subject: "platform experiment id", Value: "the new goal_version",
		Description: "A person changed the goal (PATCH /platform-experiments/{id} with goal): re-read it with GET /platform-experiments/{id}. On a draft it is its planner's, agent_id names it, and the draft is planned again from the new goal: its status_reason reads revising until the planner answers, and a planner whose machine was released is started again. Once the draft is accepted it is the coordinator's, whose subject and agent_id are coordinator, to bring the description in line with it."},
	{Kind: platformapi.EventPlatformExperimentProposal, Subject: "platform experiment id", Value: "proposed or declined",
		Description: "A draft's planner answered its goal: proposed (proposal on GET /platform-experiments/{id}) or declined (declined_reason). It waits for a person from then on."},
	{Kind: platformapi.EventPlatformExperimentPlan, Subject: "platform experiment id", Value: "empty by design",
		Description: "A person changed fields of the plan of an open or running platform experiment (PATCH /platform-experiments/{id}): detail names them, among name, metrics, budget_accelerator_hours, max_agents, researchers, max_concurrent_accelerators, report_interval_seconds and starts_at. Re-read them with GET /platform-experiments/{id}. The description and the run's length are told by platform_experiment.description, a cut by agent.cut and a raised baseline by platform_experiment.baseline."},
	{Kind: platformapi.EventPlatformExperimentBaseline, Subject: "platform experiment id", Value: "the job that is the baseline from now on",
		Description: "The baseline of the running run was raised to a stronger job that measured the control (PATCH /platform-experiments/{id} with baseline_job_id and baseline_reason). detail says by whom, from which value to which on the ranking metric, and why. The description's measured baseline changed with it, and standings are set against the new bar: re-read the description with GET /platform-experiments/{id}."},
	{Kind: platformapi.EventPlatformExperimentRemoved, Subject: "platform experiment id", Value: "empty by design",
		Description: "A person removed the run (DELETE /platform-experiments/{id}). One still going was closed in the same write, told by platform_experiment.status closed just before. It is a stop condition for every agent in it: every read of it answers 404 from then on, and the tokens of its agents and coordinator are refused, so stop at once and file nothing more."},
	{Kind: platformapi.EventJobHeld, Subject: "experiment id", Value: "the code of the queue reason (not_admitted_reason), or baseline_late",
		Description: "This job is held in the queue. It is sent when the job has waited longer than its estimated duration, and for at least the platform's scheduler.held_job_notice_seconds: detail is the sentence that says why (not_admitted_message), never empty, for capacity_unavailable the cluster, what it is short of and how many jobs hold that. Nothing is done to the job: it stays queued, and its owner decides whether to wait, cancel it or ask for less. Sent once per job, with the reason it had then. " +
			"baseline_late is a job of an open platform experiment's baseline, queued or placed and not running yet, that started now would end after baseline_due_at: detail says so with baseline_due_at, sent once per job."},
	{Kind: platformapi.EventJobDuplicate, Subject: "experiment id", Value: "the hypothesis id it tests",
		Description: "This job was submitted on a hypothesis that another agent's job was testing then: queued, running or completed within the hour. detail names those jobs, whose they are and their status, as the submitter was told in same_hypothesis. Sent once per job, at its submit."},
	{Kind: platformapi.EventJobUnderperforming, Subject: "experiment id", Value: "the metric name it trails on",
		Description: "This job trails its cohort on the ranking metric by a real margin. An opinion, not a command: nothing stops it but you. Consider POST /experiments/{id}/cancel if it is not worth its allocation. Sent once per job, and not for a job that has already ended."},
}

func init() {
	for i := range EventKinds {
		EventKinds[i].AgentScope = agentScope(EventKinds[i].Kind)
	}
}

// DefaultKinds is the kind set of a subscription that names none: every kind of EventKinds.
func DefaultKinds() map[string]bool {
	kinds := make(map[string]bool, len(EventKinds))
	for _, k := range EventKinds {
		kinds[k.Kind] = true
	}
	return kinds
}

// EventFilter scopes a subscription. Empty fields do not filter.
type EventFilter struct {
	// ProjectID scopes a subscription to the platform experiments a listing of the project holds
	// for its caller. The read applies it, since a change names no project.
	ProjectID            string
	PlatformExperimentID string
	ExperimentID         string
	AgentID              string
	// Kinds, when non-empty, admits only these kinds.
	Kinds map[string]bool
	// Decisions narrows a stream of a whole platform experiment to what whoever steers it may
	// have to act on: of a job's statuses the terminal ones, and of its queue reasons those
	// waiting will not clear. An agent filter is that narrow already.
	Decisions bool
}

// isExperimentSubject reports the kinds whose Subject is an experiment id, and therefore the only
// ones an ExperimentID filter can meaningfully narrow to.
func isExperimentSubject(kind string) bool {
	switch kind {
	case platformapi.EventExperimentStatus, platformapi.EventExperimentBlocked, platformapi.EventJobUnderperforming, platformapi.EventJobHeld, platformapi.EventJobDuplicate:
		return true
	default:
		return false
	}
}

// isAgentOwned reports the kinds that describe one agent's own property: its jobs and its
// allocation. agent.cut is here and stage.boundary is not, though one row emits both: the cut is the
// one agent's stop condition, the boundary is about the ladder.
func isAgentOwned(kind string) bool {
	switch kind {
	case platformapi.EventExperimentStatus, platformapi.EventExperimentBlocked, platformapi.EventQuotaChanged, platformapi.EventAgentCut, platformapi.EventAgentFinished, platformapi.EventAgentReplaced, platformapi.EventAgentIdle, platformapi.EventAgentInstructions, platformapi.EventAgentMessage, platformapi.EventJobUnderperforming, platformapi.EventJobHeld,
		platformapi.EventJobDuplicate, platformapi.EventPlatformExperimentGoal:
		return true
	default:
		return false
	}
}

// IsHypothesisActivity reports the kinds whose subject is a hypothesis.
func IsHypothesisActivity(kind string) bool {
	switch kind {
	case platformapi.EventHypothesisNew, platformapi.EventHypothesisStatus, platformapi.EventHypothesisLink, platformapi.EventFindingNew, platformapi.EventCommentNew:
		return true
	default:
		return false
	}
}

func agentScope(kind string) string {
	switch {
	case kind == platformapi.EventExperimentStatus:
		return "terminal statuses of your own jobs, and their returns to the queue, which carry why"
	case kind == platformapi.EventExperimentBlocked:
		return "your own jobs, and only a reason that waiting will not clear"
	case isAgentOwned(kind):
		return "your own"
	case kind == platformapi.EventHypothesisNew:
		return "someone else's hypothesis linking to one you registered"
	case kind == platformapi.EventCommentNew:
		return "someone else's, on a hypothesis you registered or on the objective"
	case IsHypothesisActivity(kind):
		return "someone else's, on a hypothesis you registered"
	case kind == platformapi.EventStageBoundary:
		return "advances only; your own cut is agent.cut"
	case kind == platformapi.EventCapacityIdle:
		return "none: it is for whoever steers the run"
	case kind == platformapi.EventAgentActivity:
		return "none: it is for whoever watches the run's page"
	default:
		return "every one"
	}
}

// Matches decides whether one event belongs on one subscription. A backlog and what is appended
// live go through it alike.
//
// An agent filter scopes the stream to what that agent has to act on, so it never needs to poll
// its own resources: its jobs' terminal statuses, their requeues and the queue reasons waiting will not clear,
// its allocation and cut, ladder advances, the run's status and description, and other people's
// activity on hypotheses it registered. A job waiting its turn for capacity is no news.
// Whether a hypothesis event concerns one of the agent's hypotheses is a graph question, answered
// by the graph (graphdb.Store.ConcernsAgent); Matches only drops the agent's own echo. Without an agent the stream is
// the whole platform experiment, or with Decisions what of it may need a decision: a job that
// was queued, admitted or started is on its way and asks nothing of anyone. What the run's page
// shows of an agent (agent.activity) asks nothing of anyone either.
func (f EventFilter) Matches(e Event) bool {
	if len(f.Kinds) > 0 && !f.Kinds[e.Kind] {
		return false
	}
	if f.PlatformExperimentID != "" && e.PlatformExperimentID != f.PlatformExperimentID {
		return false
	}
	if f.ExperimentID != "" && (!isExperimentSubject(e.Kind) || e.Subject != f.ExperimentID) {
		return false
	}
	if f.AgentID == "" {
		switch {
		case !f.Decisions:
			return true
		case e.Kind == platformapi.EventAgentActivity:
			return false
		case e.Kind == platformapi.EventExperimentStatus:
			return ExperimentStatus(e.Value).Ended()
		case e.Kind == platformapi.EventExperimentBlocked:
			return !QueueReasonClearsByWaiting(e.Value)
		default:
			return true
		}
	}
	switch {
	case e.Kind == platformapi.EventExperimentStatus:
		return e.AgentID == f.AgentID && (ExperimentStatus(e.Value).Ended() || requeued(e))
	case e.Kind == platformapi.EventExperimentBlocked:
		return e.AgentID == f.AgentID && !QueueReasonClearsByWaiting(e.Value)
	case isAgentOwned(e.Kind):
		return e.AgentID == f.AgentID
	case IsHypothesisActivity(e.Kind):
		return e.AgentID != f.AgentID
	case e.Kind == platformapi.EventStageBoundary:
		return e.AgentID == ""
	case e.Kind == platformapi.EventCapacityIdle:
		return false
	default:
		return true
	}
}

// requeued says the job of a status event went back to the queue after an attempt, which a
// first queueing does not say: only a requeue carries the reason the attempt ended.
func requeued(e Event) bool {
	return e.Value == string(StatusQueued) && e.Detail != ""
}
