// Package launcherapi is the wire contract between the control plane and the agent launchers
// that pull desired state from /internal/launchers/{name}. It imports nothing of the platform.
package launcherapi

// The states a launcher reports an instance in (ObservedInstance.State).
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StateExited   = "exited"
	StateUnknown  = "unknown"
)

// RefusalPassDoesNotFit is the reason of a refusal of a launcher whose pass can outlast the
// platform's boot limit or progress limit.
const RefusalPassDoesNotFit = "pass_does_not_fit"

// Observed is what a launcher found on its provider account before it asked: every instance
// whose name starts with its own prefix, by the name this API gave it
// (POST /internal/launchers/{name}/reconcile).
type Observed struct {
	LaunchDigest string             `json:"launch_digest" doc:"Lowercase hex digest of everything the launcher itself starts an instance with: command, size and the environment it adds. A change replaces every running instance and gives each agent its restarts back."`
	Instances    []ObservedInstance `json:"instances" doc:"Every instance the launcher's provider lists for it, an empty list when there is none."`
	// The two below say how long one pass of this launcher can take, which the boot limit has
	// to cover.
	ReconcileIntervalSeconds int `json:"reconcile_interval_seconds" doc:"How long the launcher waits between two questions."`
	CreatePauseMilliseconds  int `json:"create_pause_milliseconds" doc:"How long the launcher waits between two creates of one pass."`
}

// ObservedInstance is one name of a launcher's namespace as its provider lists it, without the
// launcher's prefix.
type ObservedInstance struct {
	Name    string `json:"name" doc:"The name from an earlier answer, without the launcher's own prefix."`
	Machine string `json:"machine" minLength:"1" maxLength:"128" doc:"The provider's id of the machine the name is reported by. The platform keeps it while it holds the name and answers it from GET /{name}/machines."`
	State   string `json:"state" enum:"starting,running,exited,unknown" doc:"unknown: the provider lists the instance and does not say whether its command runs, as when its host is unreachable."`
	Log     string `json:"log,omitempty" doc:"For an exited instance, the end of what its command wrote."`
	// Expired tells an instance its provider ended at the provider's own lifetime from one whose
	// command ended.
	Expired bool `json:"expired,omitempty" doc:"For an exited instance, true when its provider ended it at the provider's own limit on how long it rents one. Its end is recorded as expired, spends no restart and never stops the agent, which is asked for again under a new name."`
	// Progress is a pointer so that "could not be read" differs from "read, and none printed".
	Progress     *string `json:"progress,omitempty" maxLength:"64" doc:"For a running instance, the progress marker its command last printed: what follows hl-progress on the newest such line of its output, empty when it has printed none. Left out when the launcher could not read the output: an instance with no progress recorded for the platform's progress limit is then replaced with no restart spent. The platform compares it with the one before and reads nothing else from it."`
	Activity     string  `json:"activity,omitempty" maxLength:"120" doc:"For a running instance, what its agent last said it is doing: what follows hl-activity on the newest such line of the output the progress marker is read from, left out when it has printed none. It rides on the report the launcher already makes. The platform keeps the latest with the time it first read it and answers it as activity on the agent, the coordinator or the draft."`
	HarnessState string  `json:"harness_state,omitempty" maxLength:"200" doc:"For a running instance, what its agent's harness last said of itself: what follows hl-state on the newest such line of the same output, space-separated key=value fields: subagents (how many it has running), turn (working or waiting), since (Unix seconds the turn has been so), and once it has read the model provider's rate limit model_status (allowed, allowed_warning or rejected) and when known model_type, model_utilization (0 to 1) and model_resets_at (Unix seconds). Left out when it has printed none. The platform writes it to the run's health series and reads nothing else from it."`
}

// Desired is one launcher's complete desired state. Instances should exist and be running.
// Ending names instances whose run is over: they are told to stop and are otherwise left
// alone for a last turn, neither created nor replaced, and destroyed once it has exited. An
// instance in neither list should not exist. Both lists are always sent: an answer without one
// is not an empty list, and acting on it would destroy every agent.
//
// Refusal is set while the platform refuses the launcher's own configuration. The lists stand
// and are acted on as in any answer, so every machine whose run is over is still ended, and
// nothing new is rented: Instances then names only machines asked for before.
//
// Image is the platform's image, which a launcher can warm ahead of the next machine while
// nothing is asked for.
type Desired struct {
	Instances []Instance `json:"instances"`
	Ending    []string   `json:"ending"`
	Image     string     `json:"image,omitempty" doc:"The image an instance boots unless its platform experiment names its own. A launcher with a warm pool keeps it for this image while no instance is asked for."`
	Refusal   *Refusal   `json:"refusal,omitempty" doc:"Present while the platform refuses this launcher's configuration. The launcher creates no instance, and stops and destroys by the two lists as always."`
}

// Instance is one machine a launcher keeps: what it boots and the environment its agent starts
// with. Name is its identity, and the launcher puts its own prefix in front of it. The launcher
// stops the machine when its name is listed as ending.
type Instance struct {
	Name  string            `json:"name"`
	Image string            `json:"image"`
	Env   map[string]string `json:"env"`
}

type Refusal struct {
	Reason  string `json:"reason" enum:"pass_does_not_fit" doc:"pass_does_not_fit: one pass of the launcher can outlast the platform's boot limit or progress limit."`
	Message string `json:"message" doc:"What to change, in the launcher's config or the platform's."`
}

// Held is the machines the platform holds for a launcher (GET /internal/launchers/{name}/machines).
type Held struct {
	Machines []HeldMachine `json:"machines"`
}

// HeldMachine is a machine the platform holds for a launcher.
type HeldMachine struct {
	Name    string `json:"name" doc:"The name it is asked for under, without the launcher's own prefix."`
	Machine string `json:"machine" doc:"The provider's id the launcher last reported the name by."`
}
