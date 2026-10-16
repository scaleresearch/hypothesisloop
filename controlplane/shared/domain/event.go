package domain

import "time"

// Event is one change, described as small as it can be described: what kind of thing changed,
// which thing, its new value, one qualifying detail, the two ids a subscription is scoped by,
// a position and a cursor. Never a copy of the row — a client that wants detail follows with a normal GET,
// so this stream duplicates no read path and no state.
type Event struct {
	Kind                 string `json:"kind"`
	Subject              string `json:"subject"`
	Value                string `json:"value"`
	Detail               string `json:"detail"`
	PlatformExperimentID string `json:"platform_experiment_id"`
	AgentID              string `json:"agent_id"`
	// Position is the id of the transaction that made the change, the same on a live event and
	// on its replayed twin. With the fields above it identifies the event; transaction ids are
	// not commit ordered, so it does not order the stream.
	Position int64 `json:"position"`
	// OccurredAt is when the transaction at Position began, the same on a live event and on its
	// replayed twin. It is absent on the replay of a change made before positions kept a time.
	OccurredAt time.Time `json:"occurred_at,omitzero"`
	// Cursor is where to resume: every transaction below it had finished when this event was
	// written, so its events were already delivered. A client hands the largest cursor it has
	// seen back as `since`, and Replay returns every event at or above it. That can repeat an
	// event the client already has (same Position), and never skips one.
	Cursor int64 `json:"cursor"`
}

// Identity is the event without its cursor and its time: a live event and its replayed twin carry
// different cursors, so a client dropping repeats compares identities, and the position already
// says which transaction it is.
func (e Event) Identity() Event {
	e.Cursor = 0
	e.OccurredAt = time.Time{}
	return e
}

// Event kinds. Every event on the stream carries one of these.
const (
	EventExperimentStatus  = "experiment.status"
	EventExperimentBlocked = "experiment.blocked"
	EventQuotaChanged      = "quota.changed"
	EventStageBoundary     = "stage.boundary"
	EventHypothesisNew     = "hypothesis.new"
	EventHypothesisStatus  = "hypothesis.status"
	EventHypothesisLink    = "hypothesis.link"
	EventFindingNew        = "finding.new"
	EventCommentNew        = "comment.new"
	// EventAgentCut says the run ended for one agent. stage.boundary already says the ladder
	// moved, but it names the platform experiment, so an agent still had to GET to learn whether
	// the cut fell on it. This is the same row addressed to the only party it stops.
	EventAgentCut = "agent.cut"
	// EventAgentFinished says an agent told the platform it has nothing left to do. It is the
	// agent's own, so no rival is woken by it; whoever steers the run sees every one.
	EventAgentFinished = "agent.finished"
	// EventAgentReplaced says the platform added a researcher in the place of one that finished
	// or lost its machine for good, keeping its platform experiment's researchers. It is the new
	// researcher's own, so no rival is woken by it; whoever steers the run sees every one.
	EventAgentReplaced = "agent.replaced"
	// EventAgentIdle says no job of a researcher of a running platform experiment ran on
	// the platform for a while, and it has none queued or running: its machines are idle. It is the agent's own, so it
	// reaches that agent and whoever steers the run, and no rival. Its value is IdleNew on the
	// first notice of a stretch and IdleContinues on each after it.
	EventAgentIdle = "agent.idle"
	// EventCapacityIdle says the capacity of a running platform experiment with time left holds
	// places beyond its queued jobs, whether or not its researchers have jobs in flight and
	// whatever the best stands at against the target. It is the run's, so it reaches whoever
	// steers the run and no agent's own stream. Its values are those of agent.idle.
	EventCapacityIdle = "capacity.idle"
	// EventAgentMessage is a direct message from the coordinator or the owner to one agent, or from
	// the owner to the coordinator (CoordinatorRecipient), with its text: the one kind that
	// carries what was said, since it is said to its reader alone.
	EventAgentMessage = "agent.message"
	// EventPlatformExperimentStatus and EventPlatformExperimentDescription describe the run
	// itself: whether it is still open, and whether the brief still says what it said. The
	// description event deliberately carries no text — see EventKinds.
	EventPlatformExperimentStatus      = "platform_experiment.status"
	EventPlatformExperimentDescription = "platform_experiment.description"
	// EventPlatformExperimentRevision says a person asked a draft's planner to change its answer.
	// A pointer like the description event: what was asked is read from the draft.
	EventPlatformExperimentRevision = "platform_experiment.revision"
	// EventPlatformExperimentBaseline says the baseline of a running platform experiment was
	// raised to a stronger job, with the sentence every agent is told.
	EventPlatformExperimentBaseline = "platform_experiment.baseline"
	// EventMetricPoint is a pointer, never a value: experiment id, metric name, fraction
	// complete. Metrics live solely in the metrics store, and this event says only that one
	// arrived — a subscriber that wants the number reads it from there. It is the one kind
	// Replay cannot return, because Postgres never saw it (see Replay).
	EventMetricPoint = "metric.point"
	// EventJobUnderperforming says only that this job trailed its cohort on the platform
	// experiment's ranking metric by enough margin to be worth a look: an opinion, never an
	// instruction. Trailing is recomputed from the metrics store when a sample makes it true; the
	// job's row records the metric its owner was told about, once, and that write is the event,
	// live and replayed. The agent decides; the existing cancel-experiment call is the only path
	// that ever ends the job.
	EventJobUnderperforming = "job.underperforming"
	// EventJobHeld says a job has stayed QUEUED for longer than it was to run, and why. A queue
	// reason that waiting clears is kept from an agent's stream (experiment.blocked), so without
	// this an agent whose job never starts learns nothing. The job's row records the reason its
	// owner was told, once, and that write is the event, live and replayed.
	EventJobHeld = "job.held"
	// EventJobDuplicate says a job was submitted on a hypothesis another agent's job was already
	// testing: queued, running or completed within the hour. Its submitter is told in the answer
	// to its submit; the event is for whoever steers the run, so two seats do not run one idea.
	// The job's row records the sentence, once, and that write is the event, live and replayed.
	EventJobDuplicate = "job.duplicate"
	// EventEnvironmentStatus says the build of an environment an agent asked for was loaded on a
	// cluster, which runs its jobs before the push ends, or ended, ready or failed, to that agent.
	// A job waiting for it is told by its own status; an agent that built ahead of its job had
	// nothing else to tell it.
	EventEnvironmentStatus = "environment.status"
)

// EventKindDoc describes one kind well enough to subscribe to it without reading this file: what
// its subject and value are, whether an `agent` filter narrows it, and what it is for.
type EventKindDoc struct {
	Kind        string `json:"kind"`
	Subject     string `json:"subject"`
	Value       string `json:"value"`
	Description string `json:"description"`
	// AgentScope says what an `agent` filter keeps of this kind. It is filled in from agentScope,
	// written beside the rule Matches applies, so the documented scope and the applied one sit together.
	AgentScope string `json:"agent_scope"`
	// Default is true when a subscription that names no kinds carries this one. Like AgentScope it
	// is filled in from the predicate the server actually consults (isDefaultKind), so the default
	// set cannot be advertised here and be a different set on the wire.
	Default bool `json:"default"`
}

// EventKinds is the vocabulary of the stream, and the only list of it. The server validates a
// subscription's `kinds` against exactly this slice and GET /watch/kinds serves exactly this
// slice, so a kind cannot be documented but unaccepted, or accepted but undocumented — the two
// failures a hand-kept second list produces the first time a kind is added.
var EventKinds = []EventKindDoc{
	{Kind: EventExperimentStatus, Subject: "experiment id", Value: "QUEUED, SUBMITTED, RUNNING, COMPLETED, FAILED, EVICTED, REJECTED, CANCELLED",
		Description: "Your job moved. detail carries why, when the platform knows, as a reason code, or the code, a colon and a sentence (code: sentence): the reason a job was evicted, rejected or requeued (no_offer: no machine to rent for shape rtx4090 in 3m0s of asking), or how a FAILED job's process ended (container_failed: container exited with code 1, then a line last output: and the last lines it said, the most recent last), which a FAILED job always has. The code is the text before the first colon, and the same split as eviction_reason on the job. A job requeued for a fault of the platform's (code_unreachable, no_offer, stuck_pending and the like) is QUEUED with that detail and runs again at no cost. The queue reason is experiment.blocked."},
	{Kind: EventExperimentBlocked, Subject: "experiment id", Value: "the code of the queue reason (not_admitted_reason)",
		Description: "Why a job is still waiting. detail is the sentence that goes with the code (not_admitted_message). It changes while the status does not, so a stuck job has something to wait on. awaiting_admission is not sent, and an agent scope leaves out every reason that waiting clears, such as capacity_unavailable and outranked: GET /experiments has them."},
	{Kind: EventQuotaChanged, Subject: "agent id", Value: "share_accelerator_hours of GET /quota: all the agent may spend",
		Description: "A grant or a stage move landed. What is used of the share comes from GET /quota."},
	{Kind: EventStageBoundary, Subject: "platform experiment id", Value: "stage index",
		Description: "The elimination ladder advanced. detail is 'advanced' or 'cut'; agent_id names who was cut."},
	{Kind: EventAgentCut, Subject: "agent id", Value: "stage index",
		Description: "This agent was cut at a stage boundary. Its own stop condition, without a GET to find out whether the cut was its."},
	{Kind: EventAgentFinished, Subject: "agent id", Value: "empty by design",
		Description: "This agent said it has nothing left to do, and its machine stops. detail is the reason it gave, and for a researcher the blocker it named and the time and quota it left. When every agent is finished or cut the run has nobody left to wait for."},
	{Kind: EventAgentReplaced, Subject: "agent id of the new researcher", Value: "agent id of the researcher it replaces",
		Description: "The platform added a researcher in the place of one that finished or whose machine stopped with no restart left, with its flavor, model and hyperparameters, to keep the researchers the run is to have at work (researchers on GET /platform-experiments/{id}). detail says why the place was free. Nobody adds it: the platform does, while the time and the budget left hold a trial for it. A cut researcher is not replaced."},
	{Kind: EventAgentIdle, Subject: "agent id", Value: "new or continues",
		Description: "No job of this researcher of a running platform experiment ran on the platform for the platform's agents.idle_notice_minutes, and it has none queued or running: nothing of its own is in flight. " +
			"Sent again every agents.idle_notice_minutes while the stretch lasts: new on the first notice of a stretch, continues on each after it, and a job it starts ends the stretch. " +
			"detail says, as the platform knows them at the notice: for how long, the subagents and turn its harness last reported, how many jobs shaped like the run's baseline job the free capacity places now, the minutes left in the run and what is left of its share. Nothing is done to the agent."},
	{Kind: EventCapacityIdle, Subject: "platform experiment id", Value: "new or continues",
		Description: "The free capacity of a running platform experiment with time left has placed more jobs shaped like its baseline job than it has queued (fits_now beyond the queued jobs) for the platform's agents.idle_notice_minutes, whoever has jobs in flight. " +
			"Sent again every agents.idle_notice_minutes while places stay free, a reached target included: new on the first notice of a stretch, continues on each after it, and a pass that sees no place free ends the stretch. " +
			"detail says, as the platform knows them at the notice: how many places are free and for how long, the run's jobs queued and running, the minutes left in the run, and how far its best is short of or past the target. An agent filter carries none."},
	{Kind: EventAgentMessage, Subject: "agent id, the one it was sent to, or coordinator", Value: "coordinator or owner: who it is from",
		Description: "A direct message to this agent from its platform experiment's coordinator or owner (POST /platform-experiments/{id}/messages), or from the owner to the hosted coordinator, whose subject and agent_id are coordinator. detail is the text, direction to act on. GET /platform-experiments/{id}/messages reads every one sent."},
	{Kind: EventHypothesisNew, Subject: "hypothesis id", Value: "the kind of who registered it: agent, coordinator, user, key or staff",
		Description: "An idea entered the shared pool. agent_id is whose it is, empty on one no agent owns. The value says whether a person (user, key, staff), the coordinator or an agent registered it; the claim itself and who registered it come from GET /hypotheses/{id}."},
	{Kind: EventHypothesisStatus, Subject: "hypothesis id", Value: "open, confirmed, refuted, inconclusive",
		Description: "A claim was settled. A settled claim is to read, not to retest."},
	{Kind: EventHypothesisLink, Subject: "hypothesis id (the link's source)", Value: "builds_on, contradicts or related",
		Description: "A link was added to an existing hypothesis. detail is the target: a hypothesis id, or the platform experiment id for the objective. Links given at registration ride on hypothesis.new instead."},
	{Kind: EventFindingNew, Subject: "hypothesis id", Value: "the experiment id that produced it",
		Description: "A result was attached to a claim — someone's number is now readable instead of yours being re-derived."},
	{Kind: EventCommentNew, Subject: "hypothesis id, or the platform experiment id for the objective", Value: "empty by design",
		Description: "Someone said something on a claim, or on the objective, people included. agent_id is whose note it is, empty on one no agent owns. GET /hypotheses/{id}/comments reads it."},
	{Kind: EventPlatformExperimentStatus, Subject: "platform experiment id", Value: "draft, open, running, closed",
		Description: "The run itself moved. 'closed' is a stop condition for every agent in it, and its detail is what whoever closed it said of why, empty when they said nothing. With a detail on any other status the status stayed and its status_reason changed to one that waits for its coordinator, where the platform hosts one: detail is that reason, such as researchers_done once no researcher is left to work, sent within a minute of the change and once for each. GET /platform-experiments/{id} has its message."},
	{Kind: EventPlatformExperimentDescription, Subject: "platform experiment id", Value: "empty by design",
		Description: "The brief or the run's length (duration_hours) changed. A description is unbounded and is never copied into an event: re-read both with GET /platform-experiments/{id}."},
	{Kind: EventPlatformExperimentRevision, Subject: "platform experiment id", Value: "empty by design",
		Description: "A person asked the planner of a draft to change its answer (POST /platform-experiments/{id}/revise). What was asked is the last of revisions on GET /platform-experiments/{id}, whose status_reason reads revising until the planner answers again. A planner whose machine is still there after its answer is told by this event, and one whose machine was released is started again."},
	{Kind: EventPlatformExperimentBaseline, Subject: "platform experiment id", Value: "the job that is the baseline from now on",
		Description: "The baseline of the running run was raised to a stronger job that measured the control (PATCH /platform-experiments/{id} with baseline_job_id and baseline_reason). detail says by whom, from which value to which on the ranking metric, and why. The brief's measured baseline changed with it, and standings are set against the new bar: re-read the brief with GET /platform-experiments/{id}."},
	{Kind: EventMetricPoint, Subject: "experiment id", Value: "metric name and fraction complete",
		Description: "A sample arrived. The number lives only in the metrics store — read it with GET /experiments/{id}/metrics. The one kind replay cannot return."},
	{Kind: EventJobHeld, Subject: "experiment id", Value: "the code of the queue reason (not_admitted_reason), or baseline_late",
		Description: "This job is held in the queue. It is sent when the job has waited longer than its estimated duration, and for at least the platform's scheduler.held_job_notice_seconds: detail is the sentence that says why (not_admitted_message), never empty, for capacity_unavailable the cluster, what it is short of and how many jobs hold that. Nothing is done to the job: it stays queued, and its owner decides whether to wait, cancel it or ask for less. Sent once per job, with the reason it had then. " +
			"baseline_late is a job of an open platform experiment's baseline, queued or placed and not running yet, that started now would end after baseline_due_at: detail says so with baseline_due_at, sent once per job."},
	{Kind: EventJobDuplicate, Subject: "experiment id", Value: "the hypothesis id it tests",
		Description: "This job was submitted on a hypothesis that another agent's job was testing then: queued, running or completed within the hour. detail names those jobs, whose they are and their status, as the submitter was told in same_hypothesis. Sent once per job, at its submit."},
	{Kind: EventJobUnderperforming, Subject: "experiment id", Value: "the metric name it trails on",
		Description: "This job trails its cohort on the ranking metric by a real margin. An opinion, not a command: nothing stops it but you. Consider POST /experiments/{id}/cancel if it is not worth its allocation. Sent once per job, and not for a job that has already ended."},
	{Kind: EventEnvironmentStatus, Subject: "environment key", Value: "loaded, ready or failed",
		Description: "The build of an environment this agent asked for while it was building (POST /environments: hl env build) was loaded on a cluster, or ended. loaded: the cluster runs its jobs now, while the image is pushed, and any job that cannot run there starts once the push ends, by itself, so a build told loaded is not told ready. detail names the commit and image it asked with and says what to do: once the file failed, GET /environments (hl env status) has the end of its output. Sent once per build and value, to each agent that asked."},
}

func init() {
	for i := range EventKinds {
		EventKinds[i].AgentScope = agentScope(EventKinds[i].Kind)
		EventKinds[i].Default = isDefaultKind(EventKinds[i].Kind)
	}
}

// isDefaultKind reports what a subscription naming no kinds carries: everything an agent's loop
// would otherwise poll for — its jobs and why they are stuck, its allocation, its two stop
// conditions, the ladder, the brief, and the shared pool.
//
// metric.point is the one exclusion, and it is deliberate. It is by far the highest-volume kind,
// it fires for every sample of every job in the run rather than for the subscriber's own, and it
// carries no value — only a pointer to the metrics store. On a run-wide subscription that is a
// firehose whose only effect is to push the connection past its buffer and get it dropped, and a
// stream that drops is worse than one that says less. It is also the single kind Replay cannot
// return, so defaulting it on would make the default subscription the only one with a hole in its
// reconnect. An agent watching one job's progress asks for it by name, which costs one flag and
// says exactly what it means.
func isDefaultKind(kind string) bool { return kind != EventMetricPoint }

// DefaultKinds is the kind set of a subscription that names none, derived from EventKinds so that
// the served vocabulary and the applied default are one list.
func DefaultKinds() map[string]bool {
	kinds := make(map[string]bool, len(EventKinds))
	for _, k := range EventKinds {
		if k.Default {
			kinds[k.Kind] = true
		}
	}
	return kinds
}

// EventFilter scopes a subscription. Empty fields do not filter.
type EventFilter struct {
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
	case EventExperimentStatus, EventExperimentBlocked, EventMetricPoint, EventJobUnderperforming, EventJobHeld, EventJobDuplicate:
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
	case EventExperimentStatus, EventExperimentBlocked, EventMetricPoint, EventQuotaChanged, EventAgentCut, EventAgentFinished, EventAgentReplaced, EventAgentIdle, EventAgentMessage, EventJobUnderperforming, EventJobHeld,
		EventJobDuplicate, EventEnvironmentStatus:
		return true
	default:
		return false
	}
}

// IsHypothesisActivity reports the kinds whose subject is a hypothesis.
func IsHypothesisActivity(kind string) bool {
	switch kind {
	case EventHypothesisNew, EventHypothesisStatus, EventHypothesisLink, EventFindingNew, EventCommentNew:
		return true
	default:
		return false
	}
}

func agentScope(kind string) string {
	switch {
	case kind == EventExperimentStatus:
		return "terminal statuses of your own jobs, and their returns to the queue, which carry why"
	case kind == EventExperimentBlocked:
		return "your own jobs, and only a reason that waiting will not clear"
	case isAgentOwned(kind):
		return "your own"
	case kind == EventHypothesisNew:
		return "someone else's hypothesis linking to one you registered"
	case kind == EventCommentNew:
		return "someone else's, on a hypothesis you registered or on the objective"
	case IsHypothesisActivity(kind):
		return "someone else's, on a hypothesis you registered"
	case kind == EventStageBoundary:
		return "advances only; your own cut is agent.cut"
	case kind == EventCapacityIdle:
		return "none: it is for whoever steers the run"
	default:
		return "every one"
	}
}

// Matches decides whether one event belongs on one subscription. Live fan-out and replay both
// go through it, so a reconnecting client cannot receive a differently-scoped set than it was
// receiving before the connection broke.
//
// An agent filter scopes the stream to what that agent has to act on, so it never needs to poll
// its own resources: its jobs' terminal statuses, their requeues and the queue reasons waiting will not clear,
// its allocation and cut, ladder advances, the run's status and brief, and other people's
// activity on hypotheses it registered. A job waiting its turn for capacity is no news.
// Whether a hypothesis event concerns one of the agent's hypotheses is a graph question, answered
// by EventsStore.Concerns; Matches only drops the agent's own echo. Without an agent the stream is
// the whole platform experiment, or with Decisions what of it may need a decision: a job that
// was queued, admitted or started is on its way and asks nothing of anyone.
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
		case e.Kind == EventExperimentStatus:
			return ExperimentStatus(e.Value).IsTerminal()
		case e.Kind == EventExperimentBlocked:
			return !QueueReasonClearsByWaiting(e.Value)
		case e.Kind == EventEnvironmentStatus:
			return e.Value == string(EnvironmentFailed)
		default:
			return true
		}
	}
	switch {
	case e.Kind == EventExperimentStatus:
		return e.AgentID == f.AgentID && (ExperimentStatus(e.Value).IsTerminal() || requeued(e))
	case e.Kind == EventExperimentBlocked:
		return e.AgentID == f.AgentID && !QueueReasonClearsByWaiting(e.Value)
	case isAgentOwned(e.Kind):
		return e.AgentID == f.AgentID
	case IsHypothesisActivity(e.Kind):
		return e.AgentID != f.AgentID
	case e.Kind == EventStageBoundary:
		return e.AgentID == ""
	case e.Kind == EventCapacityIdle:
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

// Stamp is what Emit hands a write to another store: the position to record on what it writes,
// and a way to tell whether an earlier write's events were sent.
type Stamp struct {
	// Position is the id of Emit's transaction.
	Position int64
	// Aborted reports whether the transaction with id position aborted. The events it emitted
	// were then never sent, though its write to the other store may have committed: a write that
	// finds such a position on what it would have written records its own and emits the events
	// again.
	Aborted func(position int64) (bool, error)
	// RequireOpen refuses the write with ErrPlatformExperimentClosed once the platform experiment
	// is closed, and keeps a close from committing until Emit's transaction ends.
	RequireOpen func(platformExperimentID string) error
}
