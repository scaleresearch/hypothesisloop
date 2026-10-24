package platformapi

import "time"

// Change is one announcement of a write: what kind of thing changed, which thing, its new value,
// one qualifying detail and the two ids a subscription is scoped by.
type Change struct {
	Kind                 string `json:"kind"`
	Subject              string `json:"subject"`
	Value                string `json:"value"`
	Detail               string `json:"detail"`
	PlatformExperimentID string `json:"platform_experiment_id"`
	AgentID              string `json:"agent_id"`
}

// Event is one change as GET /watch delivers it, described as small as it can be described. Never
// a copy of the row: a client that wants detail follows with a normal GET.
type Event struct {
	// ID is the change's place in the stream: every change with a smaller id committed before it.
	// A client reads on from the id of the last change it read.
	ID int64 `json:"id"`
	Change
	// OccurredAt is when the write that made the change began.
	OccurredAt time.Time `json:"occurred_at"`
}

// Subscription is what one stream is subscribed to, derived from the filter the server matches
// against, never from what the client asked for.
type Subscription struct {
	ProjectID            string   `json:"project_id,omitempty"`
	PlatformExperimentID string   `json:"platform_experiment_id"`
	ExperimentID         string   `json:"experiment_id"`
	AgentID              string   `json:"agent_id"`
	Kinds                []string `json:"kinds"`
	// Decisions is set on a stream narrowed to what may need a decision.
	Decisions bool `json:"decisions,omitempty"`
	// After is the change id the stream reads on from.
	After int64 `json:"after"`
}

// Progress is the frame that ends every read of a stream: a client that saved Next reads on with
// after=Next and misses nothing and repeats nothing.
type Progress struct {
	Next     int64 `json:"next"`
	CaughtUp bool  `json:"caught_up"`
}

// Frame is any frame of GET /watch: the first is the subscription, then each change, each read
// ending with its progress. Exactly one of them is set.
type Frame struct {
	Subscription *Subscription `json:"subscription,omitempty"`
	*Event
	*Progress
}

// ResetKind is the kind of the line hl watch prints in place of the changes a saved place no
// longer holds: whoever reads it re-reads state.
const ResetKind = "watch.reset"

// WatchSubprotocol is the WebSocket subprotocol GET /watch speaks. A client offers it together
// with its token, "hypothesisloop.watch, <token>", and the server selects this one: a browser
// refuses a handshake that selects none of what it offered.
const WatchSubprotocol = "hypothesisloop.watch"

// WatchPingInterval is how often an idle GET /watch repeats its last progress frame, so that a job
// sitting RUNNING for an hour with nothing to say still shows either end the path between them is
// alive. A client that hears nothing for much longer reads the stream as dead.
const WatchPingInterval = 20 * time.Second

// Event kinds. Every event on the stream carries one of these.
const (
	EventExperimentStatus  = "experiment.status"
	EventExperimentBlocked = "experiment.blocked"
	EventQuotaChanged      = "quota.changed"
	EventStageBoundary     = "stage.boundary"
	EventHypothesisNew     = "hypothesis.new"
	EventHypothesisStatus  = "hypothesis.status"
	EventHypothesisLink    = "hypothesis.link"
	EventHypothesisUnlink  = "hypothesis.unlink"
	EventFindingNew        = "finding.new"
	EventCommentNew        = "comment.new"
	// EventAgentCut says the run ended for one agent. stage.boundary already says the ladder
	// moved, but it names the platform experiment, so an agent still had to GET to learn whether
	// the cut fell on it. This is the same row addressed to the only party it stops.
	EventAgentCut = "agent.cut"
	// EventAgentFinished says an agent told the platform it has nothing left to do. It is the
	// agent's own, so no rival is woken by it; whoever steers the run sees every one.
	EventAgentFinished = "agent.finished"
	// EventAgentReplaced says the platform added an agent in the place of one that finished
	// or lost its machine for good, keeping its platform experiment's team. It is the new
	// agent's own, so no rival is woken by it; whoever steers the run sees every one.
	EventAgentReplaced = "agent.replaced"
	// EventAgentStopped says the platform gave up on an agent's machine, every restart spent, with
	// the model it ran on and why. It is the agent's own, so no rival is woken by it; whoever steers
	// the run sees every one.
	EventAgentStopped = "agent.stopped"
	// EventAgentIdle says no job of an executor of a running platform experiment ran on
	// the platform for a while, and it has none queued or running: its machines are idle. It is the agent's own, so it
	// reaches that agent and whoever steers the run, and no rival. Its value is IdleNew on the
	// first notice of a stretch and IdleContinues on each after it.
	EventAgentIdle = "agent.idle"
	// EventAgentInstructions says the instructions an agent is served changed: its agent type has
	// a new current version, or the platform's rules do. A pointer like the description event,
	// whose text is read from its instructions. It reaches that agent, or the coordinator for its
	// own (CoordinatorRecipient), and whoever steers the run.
	EventAgentInstructions = "agent.instructions"
	// EventCapacityIdle says the capacity of a running platform experiment with time left holds
	// places beyond its queued jobs, whether or not its executors have jobs in flight and
	// whatever the best stands at against the target. It is the run's, so it reaches whoever
	// steers the run and no agent's own stream. Its values are those of agent.idle.
	EventCapacityIdle = "capacity.idle"
	// EventAgentMessage is a direct message from the coordinator or the owner to one agent, or from
	// the owner to the coordinator (CoordinatorRecipient), with its text: the one kind that
	// carries what was said, since it is said to its reader alone.
	EventAgentMessage = "agent.message"
	// EventAgentActivity says what the run's page shows of one agent changed where no other kind
	// tells it: it signed up or left, its machine moved, or its machine said something new of what
	// it does. It is for whoever watches the run's page, so no agent's stream and no decisions
	// stream carries it.
	EventAgentActivity = "agent.activity"
	// EventPlatformExperimentStatus and EventPlatformExperimentDescription describe the run
	// itself: whether it is still open, and whether the description still says what it said. The
	// description event deliberately carries no text.
	EventPlatformExperimentStatus      = "platform_experiment.status"
	EventPlatformExperimentDescription = "platform_experiment.description"
	// EventPlatformExperimentGoal says a person changed the goal, to the draft's planner or to the
	// coordinator, whichever acts on it now. A pointer like the description event: the goal is
	// read from the platform experiment.
	EventPlatformExperimentGoal = "platform_experiment.goal"
	// EventPlatformExperimentProposal says a draft's planner answered its goal, with a proposal
	// or a decline. Nobody acts on it but a person.
	EventPlatformExperimentProposal = "platform_experiment.proposal"
	// EventPlatformExperimentPlan says a person changed fields of the plan that no other kind
	// tells, named in its detail.
	EventPlatformExperimentPlan = "platform_experiment.plan"
	// EventPlatformExperimentBaseline says the baseline of a running platform experiment was
	// raised to a stronger job, with the sentence every agent is told.
	EventPlatformExperimentBaseline = "platform_experiment.baseline"
	// EventPlatformExperimentRemoved says a person removed the platform experiment: it is answered
	// to nobody from then on, its tokens included, so the event is the last word of its stream.
	EventPlatformExperimentRemoved = "platform_experiment.removed"
	// EventJobUnderperforming says only that this job trailed its cohort on the platform
	// experiment's ranking metric by enough margin to be worth a look: an opinion, never an
	// instruction. Trailing is recomputed from the metrics store when a sample makes it true; the
	// job's row records the metric its owner was told about, once, with the change. The agent
	// decides; the existing cancel-experiment call is the only path that ever ends the job.
	EventJobUnderperforming = "job.underperforming"
	// EventJobHeld says a job has stayed QUEUED for longer than it was to run, and why. A queue
	// reason that waiting clears is kept from an agent's stream (experiment.blocked), so without
	// this an agent whose job never starts learns nothing. The job's row records the reason its
	// owner was told, once, with the change.
	EventJobHeld = "job.held"
	// EventJobDuplicate says a job was submitted on a hypothesis another agent's job was already
	// testing: queued, running or completed within the hour. Its submitter is told in the answer
	// to its submit; the event is for whoever steers the run, so two seats do not run one idea.
	// The job's row records the sentence, once, with the change.
	EventJobDuplicate = "job.duplicate"
)
