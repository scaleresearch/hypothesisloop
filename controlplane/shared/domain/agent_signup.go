package domain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// AgentSignup is one agent's place in one platform experiment, and for an agent the platform
// runs, everything its machine is started with. All of it is fixed when the agent joins.
type AgentSignup struct {
	PlatformExperimentID string     `json:"platform_experiment_id"`
	AgentID              string     `json:"agent_id"`
	Name                 string     `json:"name"`
	Role                 SignupRole `json:"role"`
	QuotaTier            QuotaTier  `json:"quota_tier,omitempty"`
	// Hosted means the platform runs the agent: its launcher keeps one machine for it while the
	// platform experiment is running and the agent is not cut, and for one that measures the
	// baseline also while it is open.
	Hosted bool `json:"hosted"`
	// Launcher is the launcher that runs a hosted agent, written when it joins so that a later
	// change of the configured launcher moves new agents only.
	Launcher string `json:"launcher,omitempty"`
	// Flavor and Model are resolved when the agent joins: what it asked for, or the platform's
	// default at that moment. Neither is ever empty on a stored signup.
	Flavor          string         `json:"flavor"`
	Hyperparameters map[string]any `json:"hyperparameters"`
	Model           string         `json:"model"`
	SignedUpAt      time.Time      `json:"signed_up_at"`
	// Actor is whose credential added the agent, unverified with no id on a signup from before
	// this was kept. A coordinator removes only the agents its own token added.
	Actor *Actor `json:"actor,omitempty"`
	// FinishedAt is when the agent said it has nothing left to do, and FinishedReason what it
	// gave as why. From then its machine is no longer wanted.
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	FinishedReason string     `json:"finished_reason,omitempty"`
	// Ended is why the agent's run in the platform experiment is over, empty while it goes on.
	// A hosted agent's machine is told to stop for this and for nothing else, and the agent
	// reads it to learn why.
	Ended RunEnd `json:"-"`
	// Instance is what became of a hosted agent's machine, nil for an agent the platform does
	// not run.
	Instance *AgentInstance `json:"instance,omitempty"`
	// Activity is what the agent on the machine asked for last said it is doing.
	Activity *AgentActivity `json:"activity,omitempty"`
	// InFlight is what of the agent's own is queued or running, nil where it was not read.
	InFlight *InFlight `json:"in_flight,omitempty"`
}

// InFlight counts what of an agent's own is queued or running on the platform now.
type InFlight struct {
	Jobs int `json:"jobs" doc:"Its jobs queued or running, those waiting for their environment's build included. 0: nothing of its own is on the platform, and an agent waiting then waits for nothing it started."`
}

// AgentActivity is what a hosted agent or coordinator is doing now, as its harness last said it
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

// RunEnd is why an agent's run in a platform experiment is over.
type RunEnd string

const (
	RunEndClosed   RunEnd = "closed"
	RunEndCut      RunEnd = "cut"
	RunEndFinished RunEnd = "finished"
	// RunEndPlanned is a planner whose answer to its draft is stored: a proposal, or why there
	// is none. A revision takes it back.
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
		return released + "its planner answered the draft, and a revision of the draft starts a new machine"
	}
	return released + "its run is over"
}

// AgentInstance is the recorded outcome of a hosted agent's machine, as its launcher reported it.
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
	// Launch identifies what Restarts were spent under: the image and the launcher's own launch
	// inputs. A machine asked for under another starts with none spent.
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
	InstanceStarting  = "starting"
	InstanceRunning   = "running"
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

// HostedCoordinator is the coordinator the platform runs for one platform experiment: one more
// machine of its launcher, started as the agents are and holding a coordinator token. It is not
// signed up, so it holds no quota and is not ranked.
type HostedCoordinator struct {
	Launcher string `json:"launcher"`
	Model    string `json:"model"`
	// Instance is what became of its machine.
	Instance AgentInstance `json:"instance"`
	// Activity is what the coordinator last said it is doing.
	Activity *AgentActivity `json:"activity,omitempty"`
}

var launcherNamePattern = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

// ValidateLauncherName holds a launcher's name to what can start the name of every machine it
// rents: with a dash in it, one launcher's names would start with another's.
func ValidateLauncherName(name string) error {
	if !launcherNamePattern.MatchString(name) {
		return fmt.Errorf("launcher name %q must be 1 to 16 lowercase letters and digits", name)
	}
	return nil
}

// CoordinatorFlavor is what the harness is started as to coordinate. It is no flavor an agent
// can join with.
const CoordinatorFlavor = "coordinator"

// PlannerFlavor is what the harness of a draft's planner is started as. It is no member of
// AgentFlavors: the platform signs the planner up, with role SignupRolePlanner.
const PlannerFlavor = "planner"

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

// MaxAgentInstanceRestarts is how many times an exited machine of one hosted agent is asked for
// again within AgentInstanceRestartWindow. The next exit inside that window stops the agent, and
// restarts older than the window are no longer counted, so a long run survives occasional exits.
const MaxAgentInstanceRestarts = 3

// AgentInstanceRestartWindow is the rolling window MaxAgentInstanceRestarts is counted over.
const AgentInstanceRestartWindow = 6 * time.Hour

// AgentProgressIntervalSeconds is how often a healthy agent's harness prints its progress
// marker (agents/experimentator, progress.py), and MinAgentProgressLimitSeconds the shortest
// silence the platform may be configured to give up on.
const (
	AgentProgressIntervalSeconds = 15
	MinAgentProgressLimitSeconds = 8 * AgentProgressIntervalSeconds
)

// MaxAgentSessionBytes bounds the session snapshot an agent or a coordinator stores.
const MaxAgentSessionBytes = 32 << 20

// MaxAgentInstanceLogBytes bounds the log tail kept of an exited machine.
const MaxAgentInstanceLogBytes = 4 << 10

// MaxHyperparametersBytes bounds an agent's hyperparameters as JSON. They travel in every answer
// to the agent's launcher and in the agent's environment.
const MaxHyperparametersBytes = 4 << 10

// AgentFlavors are the specializations an agent can be started as: the briefs the agent image
// holds under prompts/flavors.
var AgentFlavors = []string{"generalist", "hyperparameter-search", "architecture-search", "data-search"}

// DefaultAgentFlavor is what an agent runs when it joins without naming one.
const DefaultAgentFlavor = "generalist"

const maxModelLength = 128

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]*$`)

// ResolveAgentLaunch checks what an agent asked to be started with and fills what it left out, so
// that a signup always says exactly what its agent runs. defaultModel is the platform's own.
func ResolveAgentLaunch(flavor, model, defaultModel string, hyperparameters map[string]any) (resolvedFlavor, resolvedModel string, err error) {
	if flavor == "" {
		flavor = DefaultAgentFlavor
	}
	if !slices.Contains(AgentFlavors, flavor) {
		return "", "", fmt.Errorf("flavor %q is not one an agent can run: %s", flavor, strings.Join(AgentFlavors, ", "))
	}
	if model == "" {
		model = defaultModel
	}
	if len(model) > maxModelLength || !modelPattern.MatchString(model) {
		return "", "", fmt.Errorf("model %q must be at most %d characters of letters, digits and . _ : - [ ]", model, maxModelLength)
	}
	encoded, err := json.Marshal(hyperparameters)
	if err != nil {
		return "", "", fmt.Errorf("hyperparameters cannot be encoded: %w", err)
	}
	if len(encoded) > MaxHyperparametersBytes {
		return "", "", fmt.Errorf("hyperparameters are %d bytes as JSON, the limit is %d", len(encoded), MaxHyperparametersBytes)
	}
	return flavor, model, nil
}

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
