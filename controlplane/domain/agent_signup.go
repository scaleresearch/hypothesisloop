package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/agentsandbox"
	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
)

// AgentSignup is one agent's place in one platform experiment, and for an agent the platform
// runs, everything its machine is started with. All of it is fixed when the agent joins.
type AgentSignup struct {
	PlatformExperimentID string     `json:"platform_experiment_id"`
	AgentID              string     `json:"agent_id"`
	Name                 string     `json:"name"`
	Role                 SignupRole `json:"role"`
	QuotaTier            QuotaTier  `json:"quota_tier,omitempty"`
	// Sandboxed means the platform runs the agent: its launcher keeps one machine for it while the
	// platform experiment is running and the agent is not cut, and for one that measures the
	// baseline also while it is open.
	InAgentSandbox bool `json:"in_agentsandbox"`
	// AgentType is the id of the agent type it runs, fixed when it joins: it runs the type's current
	// version. Model, empty, and Hyperparameters, nil, are the type's, read whenever it is served.
	AgentType       string         `json:"agent_type" doc:"The agent type it runs, by id. It runs the type's current version, told with agent.instructions when that changes."`
	Hyperparameters map[string]any `json:"hyperparameters" nullable:"true" doc:"Its own hyperparameters, null when it runs with its agent type's."`
	Model           string         `json:"model" doc:"Its own model, empty when it runs on its agent type's or, for a type that names none, the platform's default."`
	SignedUpAt      time.Time      `json:"signed_up_at"`
	// Actor is whose credential added the agent, unverified with no id on a signup from before
	// this was kept. A coordinator removes only the agents its own token added.
	Actor *Actor `json:"actor,omitempty"`
	// FinishedAt is when the agent said it has nothing left to do, and FinishedReason what it
	// gave as why. From then its machine is no longer wanted.
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	FinishedReason string     `json:"finished_reason,omitempty"`
	// Replaces is the researcher whose place the platform added this one in, keeping its
	// platform experiment's researchers, and ReplacedBy the one added in this one's place.
	Replaces   string `json:"replaces,omitempty" doc:"The researcher that finished or lost its machine whose place the platform added this one in, with its agent type, model and hyperparameters."`
	ReplacedBy string `json:"replaced_by,omitempty" doc:"The researcher the platform added in this one's place. A replaced researcher takes no place and is never started again."`
	// Ended is why the agent's run in the platform experiment is over, empty while it goes on.
	// A sandboxed agent's machine is told to stop for this and for nothing else, and the agent
	// reads it to learn why.
	Ended RunEnd `json:"-"`
	// CutReason is why the platform cut the agent other than at a stage boundary, empty for a stage
	// cut and for an agent that was not cut.
	CutReason string `json:"cut_reason,omitempty" doc:"Why the platform cut it other than at a stage boundary: max_agents_lowered for one of the newest researchers above a max_agents lowered below those at work. Absent for a stage cut and an agent that was not cut."`
	// AtWork is whether the agent is a researcher at work, the one count max_agents and the pool
	// hold to, and EndedReason why a researcher's place is free: one of the EndedReason constants.
	AtWork      bool   `json:"at_work" doc:"Whether it is a researcher at work: of role competitor, not finished, not cut, not replaced and, run by the platform, with a machine. The one count max_agents and the researchers pool hold to."`
	EndedReason string `json:"ended_reason,omitempty" enum:"finished,cut,replaced,stopped" doc:"Why it no longer holds a place among the researchers: finished, cut (at a stage boundary or by a lowered max_agents), replaced (the platform added a researcher in its place) or stopped (its machine stopped with no restart left). Absent while it holds one, and for an agent whose run is over only with the close."`
	// Instance is what became of a sandboxed agent's machine, nil for an agent the platform does
	// not run.
	Instance *AgentInstance `json:"instance,omitempty"`
	// Activity is what the agent on the machine asked for last said it is doing.
	Activity *AgentActivity `json:"activity,omitempty"`
	// InFlight is what of the agent's own is queued or running, nil where it was not read.
	InFlight *InFlight `json:"in_flight,omitempty"`
}

// SignupAction is something the reader of an agent's signup may do to the agent now.
type SignupAction string

const (
	SignupActionMessage   SignupAction = "message"
	SignupActionRemove    SignupAction = "remove"
	SignupActionShowToken SignupAction = "show_token"
)

var SignupActions = []SignupAction{SignupActionMessage, SignupActionRemove, SignupActionShowToken}

// InFlight counts what of an agent's own is queued or running on the platform now.
type InFlight struct {
	Jobs    int `json:"jobs" doc:"Its jobs queued or running, those waiting for their environment's build included. 0: nothing of its own is on the platform, and an agent waiting then waits for nothing it started."`
	Queued  int `json:"queued" doc:"Of jobs, those not running yet: queued, placed, or waiting for their environment's build."`
	Running int `json:"running" doc:"Of jobs, those running."`
	// IdleSince is when the agent's stretch with nothing of its own on the platform began, in a
	// running platform experiment that started: the start, its joining, or the last moment a job
	// of its was created or seen running. Nil while a job of its is in flight, and outside a run.
	IdleSince *time.Time `json:"-"`
}

// HypothesisInFlight is one hypothesis of a platform experiment with jobs queued or running or a
// checkout within CheckoutClaim, and the agents whose jobs or checkouts they are.
type HypothesisInFlight struct {
	HypothesisID string   `json:"hypothesis_id"`
	Queued       int      `json:"queued" doc:"Its jobs not running yet: queued, placed, or waiting for their environment's build."`
	Running      int      `json:"running"`
	AgentIDs     []string `json:"-"`
}

// MinutesWithoutJob is how long, at now, the agent has had nothing of its own on the platform:
// 0 while a job of its is in flight, nil outside a running platform experiment that started.
func (f InFlight) MinutesWithoutJob(now time.Time) *int {
	switch {
	case f.Jobs > 0:
		zero := 0
		return &zero
	case f.IdleSince == nil:
		return nil
	}
	minutes := max(0, int(now.Sub(*f.IdleSince).Minutes()))
	return &minutes
}

// AgentTurn is whether a sandboxed agent's harness is in a turn of its model or waits for platform
// updates between turns, as the harness last said it.
type AgentTurn struct {
	State string    `json:"state" enum:"working,waiting" doc:"working: the model is in a turn. waiting: the turn ended and the harness waits for the platform's next update to the agent."`
	Since time.Time `json:"since" doc:"When the harness entered that state."`
}

const (
	TurnWorking = "working"
	TurnWaiting = "waiting"
)

// MessageAllowance is how many more direct messages the reader of it may send an agent in the hour
// (MaxAgentMessagesPerHour): the coordinator's and the owner's count apart, each for its own reader.
type MessageAllowance struct {
	LeftThisHour int        `json:"left_this_hour" doc:"How many more messages the reader may send it now: the coordinator reads the coordinator's count, a person or a key the owner's."`
	NextAt       *time.Time `json:"next_at,omitempty" doc:"When left_this_hour next grows: the oldest of the reader's messages of the last hour leaves it then. Absent while it sent none in the last hour."`
}

// ModelAllowance is what a sandboxed agent's or coordinator's model account has left of the model
// provider's rate limit, as its harness last read it.
type ModelAllowance struct {
	Utilization  *float64   `json:"utilization,omitempty" doc:"The share of the account's rate limit used, 0 to 1. Absent before a reading that says it, and once the window it was read in has reset."`
	ResetsAt     *time.Time `json:"resets_at,omitempty" doc:"When the window that utilization was read in resets, and its use with it. Absent when the reading said none, and once it has passed."`
	RefusedUntil *time.Time `json:"refused_until,omitempty" doc:"Present while the provider refuses the account's calls: when the provider said its limit resets."`
}

// ModelAllowanceOf is what limit says the account has left, at now: a reading whose window has
// reset says nothing of it, and a refusal whose reset has passed is over.
func ModelAllowanceOf(limit RateLimit, now time.Time) *ModelAllowance {
	if limit.ResetsAt != nil && !limit.ResetsAt.After(now) {
		return &ModelAllowance{}
	}
	out := &ModelAllowance{Utilization: limit.Utilization, ResetsAt: limit.ResetsAt}
	if limit.Rejected && limit.ResetsAt != nil {
		out.RefusedUntil = limit.ResetsAt
	}
	return out
}

// TurnOf is the turn state says.
func TurnOf(state HarnessState) *AgentTurn {
	turn := &AgentTurn{State: TurnWorking, Since: state.TurnSince}
	if state.TurnWaiting {
		turn.State = TurnWaiting
	}
	return turn
}

// AgentHealth is how one agent of a platform experiment, or its sandboxed coordinator, is getting on,
// as the run's health series in the metrics store record it.
type AgentHealth struct {
	PlatformExperimentID string `json:"-"`
	AgentID              string `json:"agent_id,omitempty" doc:"The agent. Absent for the coordinator."`
	Role                 string `json:"role" doc:"The agent's role in the run (competitor, baseline, reviewer, planner), or coordinator."`
	// FirstJobSeconds and FirstCompletedSeconds run from when the agent could first submit to its
	// first job submitted and its first job completed; nil before there is one.
	FirstJobSeconds       *float64 `json:"first_job_seconds,omitempty" doc:"Seconds from when it could first submit to its first job. Absent before it submitted one."`
	FirstCompletedSeconds *float64 `json:"first_completed_job_seconds,omitempty" doc:"Seconds from when it could first submit to its first completed job. Absent before one completed."`
	JobsInFlight          int      `json:"jobs_in_flight" doc:"Its jobs queued, submitted or running."`
	Restarts              int      `json:"restarts" doc:"Every machine of its that exited and was asked for again, over the run, whatever image it ran."`
	CoordinatorMessages   int      `json:"coordinator_messages" doc:"Messages the coordinator sent it."`
	// The two below are of a sandboxed agent or coordinator, from the rate limit its harness last
	// read from the model provider (HarnessState).
	RateLimitUtilization *float64   `json:"rate_limit_utilization,omitempty" doc:"The share of its model account's rate limit used, 0 to 1, at the last reading of its machine. Absent before a reading that says it."`
	RateLimitedUntil     *time.Time `json:"rate_limited_until,omitempty" doc:"Present while the model provider refuses its model calls: when the provider said its limit resets. It waits until then and resumes, spending no restart."`
}

// CoordinatorHealthRole is the role the run's health series give its sandboxed coordinator.
const CoordinatorHealthRole = "coordinator"

// AgentActivity is what a sandboxed agent or coordinator is doing now, as its harness last said it
// beside its progress marker. The platform keeps the latest sentence only.
type AgentActivity struct {
	Text string    `json:"text" maxLength:"120" doc:"A short sentence of what the agent is doing now, written by its harness from the tool call in flight: \"thinking\", \"reading train.py\", \"running hl capacity\", \"waiting for platform updates\". It names commands, file names and ids, and carries no argument, file content or secret."`
	At   time.Time `json:"at" doc:"When the platform first read this sentence from the agent's machine, which reports about every 15 seconds. An old time beside a running instance is an agent that has been at the same thing since."`
}

// ActivityOf is the activity stored as text since at, nil for none.
func ActivityOf(text string, at *time.Time) *AgentActivity {
	if text == "" || at == nil {
		return nil
	}
	return &AgentActivity{Text: text, At: *at}
}

// Why an agent no longer holds a place among the researchers (AgentSignup.EndedReason), beside
// RunEndCut and RunEndFinished.
const (
	EndedReplaced = "replaced"
	EndedStopped  = "stopped"
)

// RunEnd is why an agent's run in a platform experiment is over.
type RunEnd string

const (
	RunEndClosed   RunEnd = "closed"
	RunEndCut      RunEnd = "cut"
	RunEndFinished RunEnd = "finished"
	// RunEndPlanned is a planner whose answer to its draft is stored: a proposal, or why there
	// is none. A change of the goal takes it back.
	RunEndPlanned RunEnd = "planned"
)

// ReleasedBecause is what the exit log of a machine says that the platform took down on purpose,
// the wind-down after its run's end having passed. It is no machine the provider lost.
func (e RunEnd) ReleasedBecause() string {
	const released = "the platform released the machine on purpose: "
	switch e {
	case RunEndClosed:
		return released + "its platform experiment closed"
	case RunEndCut:
		return released + "its agent was cut at a stage boundary"
	case RunEndFinished:
		return released + "its agent finished"
	case RunEndPlanned:
		return released + "its planner answered the draft, and a change of the draft's goal starts a new machine"
	}
	return released + "its run is over"
}

// AgentInstance is the recorded outcome of a sandboxed agent's machine, as its launcher reported it.
type AgentInstance struct {
	// Name is the name the machine was last asked for under, without the launcher's prefix.
	// Empty until the launcher first asked, and after an exit until it asks again.
	Name string `json:"name,omitempty"`
	// Restarts counts the machines that exited and were asked for again, ever. Only those of the
	// last AgentInstanceRestartWindow count towards MaxAgentInstanceRestarts.
	Restarts int `json:"restarts"`
	// Renewals counts the machines that ended through no fault of their command and were asked
	// for again with no restart spent: EndReason "expired" or "progress_unreadable".
	Renewals int `json:"-"`
	// Stopped means the last machine exited with no restart left in the window: the platform no longer asks
	// for one, and the agent takes no further turn.
	Stopped  bool       `json:"stopped"`
	ExitedAt *time.Time `json:"exited_at,omitempty"`
	// ExitLog is the end of what the last exited machine's command wrote.
	ExitLog string `json:"exit_log,omitempty"`
	// SeenAt is when the launcher first reported the machine asked for under Name.
	SeenAt *time.Time `json:"seen_at,omitempty"`
	// Launch identifies what the restart budget was spent under: the image and the launcher's own
	// launch inputs. A machine asked for under another starts with the whole budget, and Restarts
	// keeps counting.
	Launch string `json:"-"`
	// State is what the machine is now: "waiting" while none is asked for because the platform
	// experiment has not started, or of a coordinator because its open platform experiment has
	// not yet waited for it, "requested" while its launcher has not listed the one
	// asked for, "starting" and "running" as the launcher last reported it, "stopped" once no
	// further machine is asked for: its restarts ran out (Stopped), or its run is over. It is
	// worked out from the fields around it when written.
	State string `json:"state" enum:"waiting,requested,starting,running,stopped"`
	// Reported is the state the launcher last reported of the machine asked for under Name.
	Reported string `json:"-"`
	// AskedAt is when the machine named Name was asked of the launcher.
	AskedAt *time.Time `json:"asked_at,omitempty"`
	// ProgressAt is when the platform last saw the machine's command make progress.
	ProgressAt *time.Time `json:"progress_at,omitempty"`
	// EndReason is why the last machine that ended was given up on: "exited" (its command
	// ended), "gone" (its provider no longer lists it), "never_seen" (no machine came to be),
	// "still_starting" (it did not come up in time), "silent" (it ran and made no progress),
	// "progress_unreadable" (it ran and its launcher could not read its output for as long as a
	// silent one is given), "expired" (its provider ended it at the provider's own limit on how
	// long it rents one) or "released" (the platform took it down because its run was over, and
	// ExitLog says which end that was). "released" is the platform's own doing, "expired" the
	// provider's and "progress_unreadable" says nothing of the command: none of the three is a
	// failure of the machine, and none spends a restart.
	EndReason string `json:"end_reason,omitempty" enum:"exited,gone,never_seen,still_starting,silent,progress_unreadable,expired,released"`
	// Released means the run the machine was for is over and its wind-down has passed: the
	// platform experiment closed, or the agent was cut. The launcher is no longer asked for the
	// machine and has destroyed it, so what it last reported of it says nothing any more.
	Released bool `json:"-"`
	// Waiting means no machine is asked for yet: the agent's platform experiment is open and its
	// machine is asked for at the start, or the coordinator's is open and has not needed it yet.
	// Whoever reads the roster or the coordinator of an open one sets it.
	Waiting bool `json:"-"`
}

const (
	InstanceWaiting   = "waiting"
	InstanceRequested = "requested"
	InstanceStarting  = string(agentsandbox.InstanceStarting)
	InstanceRunning   = string(agentsandbox.InstanceRunning)
	InstanceStopped   = "stopped"
)

// Shown is the instance's State.
func (i AgentInstance) Shown() string {
	switch {
	case i.Stopped || i.Released:
		return InstanceStopped
	case i.Waiting:
		return InstanceWaiting
	case i.Name == "" || i.Reported == "":
		return InstanceRequested
	}
	return i.Reported
}

func (i AgentInstance) MarshalJSON() ([]byte, error) {
	type fields AgentInstance
	out := fields(i)
	out.State = i.Shown()
	return json.Marshal(out)
}

// AgentSandboxCoordinator is the coordinator the platform runs for one platform experiment: one more
// agent sandbox, started as the agents are and holding a coordinator token. It is not
// signed up, so it holds no quota and is not ranked.
type AgentSandboxCoordinator struct {
	Model string `json:"model"`
	// Instance is what became of its machine.
	Instance AgentInstance `json:"instance"`
	// Activity is what the coordinator last said it is doing.
	Activity *AgentActivity `json:"activity,omitempty"`
}

// CoordinatorAgentType is what the harness is started as to coordinate. No agent can join
// with it.
const CoordinatorAgentType = "coordinator"

// PlannerAgentType is what the harness of a draft's planner is started as. No agent joins
// with it: the platform signs the planner up, with role SignupRolePlanner.
const PlannerAgentType = "planner"

// SharesBudget says the agent is given a share of the budget when its platform experiment
// starts: every role but baseline, whose quota is its own from the moment it joins, and planner.
func (s AgentSignup) SharesBudget() bool {
	return s.Role != SignupRoleBaseline && s.Role != SignupRolePlanner
}

// InstanceReleasedSQL is the condition, on the database's clock, under which the machine of a
// run that ended at endedAt (a column or expression, NULL while the run goes on) is released:
// AgentWindDown later, when the launcher's desired state stops naming it.
func InstanceReleasedSQL(endedAt string) string {
	return fmt.Sprintf("COALESCE(%s <= now() - make_interval(secs => %d), false)", endedAt, int(AgentWindDown.Seconds()))
}

// MaxAgentInstanceRestarts is how many times an exited machine of one sandboxed agent is asked for
// again within AgentInstanceRestartWindow. The next exit inside that window stops the agent, and
// restarts older than the window are no longer counted, so a long run survives occasional exits.
const MaxAgentInstanceRestarts = 3

// AgentInstanceRestartWindow is the rolling window MaxAgentInstanceRestarts is counted over.
const AgentInstanceRestartWindow = 6 * time.Hour

// AgentProgressIntervalSeconds is how often a healthy agent's harness prints its progress
// marker (agents/harness, progress.py), and MinAgentProgressLimitSeconds the shortest
// silence the platform may be configured to give up on.
const (
	AgentProgressIntervalSeconds = agentsandbox.ProgressIntervalSeconds
	MinAgentProgressLimitSeconds = 8 * AgentProgressIntervalSeconds
)

// MaxAgentSessionBytes bounds the session snapshot an agent or a coordinator stores.
const MaxAgentSessionBytes = 32 << 20

// ErrNoSession is returned for an agent or coordinator that has stored no session.
var ErrNoSession = errors.New("objectstore: no session is stored")

// MaxHyperparametersBytes bounds an agent's hyperparameters as JSON. They travel in every answer
// to the agent's launcher and in the agent's environment.
const MaxHyperparametersBytes = experimentspec.MaxHyperparametersBytes

// DefaultAgentType is the agent type a researcher runs as when it joins without naming one.
const DefaultAgentType = "generalist"

const maxModelLength = 128

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]*$`)

// AgentName is what an agent created without a name is called in a platform experiment: its
// role and the first number no agent of that experiment carries yet, "Baseline 1" or
// "Researcher 2". taken says whether the experiment already has an agent of that name.
func AgentName(role SignupRole, taken func(name string) bool) string {
	label := map[SignupRole]string{
		SignupRoleCompetitor: "Researcher", SignupRoleBaseline: "Baseline", SignupRoleReviewer: "Reviewer", SignupRolePlanner: "Planner",
	}[role]
	for n := 1; ; n++ {
		if name := fmt.Sprintf("%s %d", label, n); !taken(name) {
			return name
		}
	}
}
