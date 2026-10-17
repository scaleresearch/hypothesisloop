// Package agentsandbox is what the launcher of sandboxed agents and the platform say to each other:
// what the launcher observed on its provider account, and the machines the platform decides it
// keeps.
package agentsandbox

// Observed is what the launcher found on its provider account: every instance whose name starts
// with its own prefix, by the name the decider gave it.
type Observed struct {
	// LaunchDigest is a digest of everything the launcher itself starts an instance with.
	LaunchDigest string
	Instances    []ObservedInstance
	// Pool is every machine of the launcher's warm pool it found. The platform holds them (Held)
	// until they are reported no more, so a pool machine its provider's list leaves out is read
	// by its id, by this launcher or the next one, and never warmed again.
	Pool []PoolMachine
}

// PoolMachine is one machine of the launcher's warm pool: its name without the launcher's
// prefix, and the provider's id.
type PoolMachine struct {
	Name    string
	Machine string
}

// ObservedInstance is one name of the launcher's namespace as its provider lists it, without the
// launcher's prefix.
type ObservedInstance struct {
	Name string
	// Machine is the provider's id of the machine the name is reported by.
	Machine string
	State   InstanceState
	// Log is the end of what an exited instance's command wrote, at most MaxExitLogBytes.
	Log string
	// Expired tells an exited instance its provider ended at the provider's own lifetime.
	Expired bool
	// Progress is the progress marker a running instance's command last printed, "" when it
	// printed none, nil when its output could not be read.
	Progress *string
	// Activity is what a running instance's agent last said it is doing.
	Activity string
	// HarnessState is what a running instance's harness last said of itself (parseHarnessState).
	HarnessState string
}

// InstanceState is what the launcher found of an instance on its provider.
type InstanceState string

const (
	// InstanceStarting is an instance created and not yet running its command.
	InstanceStarting InstanceState = "starting"
	InstanceRunning  InstanceState = "running"
	// InstanceExited is an instance whose command ended, its output still readable.
	InstanceExited InstanceState = "exited"
	// InstanceUnknown is an instance that exists and its provider does not say it runs.
	InstanceUnknown InstanceState = "unknown"
)

// MaxExitLogBytes bounds ObservedInstance.Log.
const MaxExitLogBytes = 4 << 10

// Desired is the launcher's complete desired state. Instances should exist and be running.
// Ending names instances whose run is over: they are told to stop and are otherwise left alone
// for a last turn, neither created nor replaced, and destroyed once it has exited. An instance in
// neither list should not exist. Image is the platform's image, which the launcher warms ahead
// of the next machine while nothing is asked for.
type Desired struct {
	Instances []Instance
	Ending    []string
	Image     string
}

// Instance is one machine the launcher keeps: what it boots and the environment its agent starts
// with. Name is its identity, and the launcher puts its own prefix in front of it.
type Instance struct {
	Name  string
	Image string
	Env   map[string]string
}

// HeldMachine is a machine the platform holds: the name it is asked for under, or a pool
// machine's name, without the launcher's prefix, and the provider's id the launcher last reported
// the name by.
type HeldMachine struct {
	Name    string
	Machine string
}

// The lines a sandboxed agent's harness prints while it works, which the launcher reads from its
// output: a progress marker, a sentence of what the agent is doing, and what the harness observes
// of itself. Each is its prefix and a space before what it says: the launcher takes a marker and a
// state of at most their bound of characters, and cuts a sentence to its own.
const (
	ProgressPrefix    = "hl-progress"
	ActivityPrefix    = "hl-activity"
	StatePrefix       = "hl-state"
	MaxProgressMarker = 64
	MaxActivityLength = 120
	MaxStateLength    = 200
	// ProgressIntervalSeconds is how often a healthy harness prints its progress marker.
	ProgressIntervalSeconds = 15
)

// The environment a sandboxed agent's machine is started with, which hl reads: where the
// platform is and the caller's token.
const (
	EnvAPIURL = "API_URL"
	EnvToken  = "HL_TOKEN"
)
