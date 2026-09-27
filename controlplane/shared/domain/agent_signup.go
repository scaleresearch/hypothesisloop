package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
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

// AgentInstance is the recorded outcome of a hosted agent's machine, as its launcher reported it.
type AgentInstance struct {
	// Name is the name the machine was last asked for under, without the launcher's prefix.
	// Empty until the launcher first asked, and after an exit until it asks again.
	Name string `json:"name,omitempty"`
	// Restarts counts the machines that exited and were asked for again.
	Restarts int `json:"restarts"`
	// Stopped means the last machine exited with no restart left: the platform no longer asks
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
	// State is what the machine is now: "requested" while its launcher has not listed the one
	// asked for, "starting" and "running" as the launcher last reported it, "stopped" once no
	// further machine is asked for: its restarts ran out (Stopped), or its run is over. It is
	// worked out from the fields around it when written.
	State string `json:"state" enum:"requested,starting,running,stopped"`
	// Reported is the state the launcher last reported of the machine asked for under Name.
	Reported string `json:"-"`
	// ProgressAt is when the platform last saw the machine's command make progress.
	ProgressAt *time.Time `json:"progress_at,omitempty"`
	// EndReason is why the last machine that ended was given up on: "exited" (its command
	// ended), "gone" (its provider no longer lists it), "never_seen" (no machine came to be),
	// "still_starting" (it did not come up in time) or "silent" (it ran and made no progress).
	EndReason string `json:"end_reason,omitempty" enum:"exited,gone,never_seen,still_starting,silent"`
	// Released means the run the machine was for is over and its wind-down has passed: the
	// platform experiment closed, or the agent was cut. The launcher is no longer asked for the
	// machine and has destroyed it, so what it last reported of it says nothing any more.
	Released bool `json:"-"`
}

const (
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

// MeasuresBaseline says the agent is one whose job measures the platform experiment's baseline.
func (s AgentSignup) MeasuresBaseline() bool {
	return s.Role == SignupRoleBaseline
}

// InstanceReleasedSQL is the condition, on the database's clock, under which the machine of a
// run that ended at endedAt (a column or expression, NULL while the run goes on) is released:
// AgentWindDown later, when the launcher's desired state stops naming it.
func InstanceReleasedSQL(endedAt string) string {
	return fmt.Sprintf("COALESCE(%s <= now() - make_interval(secs => %d), false)", endedAt, int(AgentWindDown.Seconds()))
}

// MaxAgentInstanceRestarts is how many times an exited machine of one hosted agent is asked for
// again. The next exit stops the agent.
const MaxAgentInstanceRestarts = 3

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

// NewAgentID returns the id of an agent the platform creates: time-ordered, and within the
// identifier rule every agent id meets.
func NewAgentID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", errors.New("generate agent id: " + err.Error())
	}
	return "ag-" + strings.ReplaceAll(id.String(), "-", ""), nil
}
