package launcherapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	contract "github.com/scaleresearch/hypothesisloop/contract/launcherapi"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

type Store interface {
	ListLauncherAgents(ctx context.Context, launcher string, windDown time.Duration) ([]db.LauncherAgent, error)
	ListLauncherCoordinators(ctx context.Context, launcher string, windDown time.Duration) ([]db.LauncherCoordinator, error)
	ReleaseInstances(ctx context.Context, launcher string, windDown, coordinatorWindDown time.Duration) error
	// The three writes take a hosted agent's id, or none for a platform experiment's coordinator.
	AskInstance(ctx context.Context, platformExpID, agentID, name, launch string) error
	ObserveInstance(ctx context.Context, platformExpID, agentID, name, state string, progress *string, progressLimit time.Duration, activity, machine string) error
	EndInstance(ctx context.Context, platformExpID, agentID, name string, how db.InstanceEnd, exitLog string, limit time.Duration, maxRestarts int) (after domain.AgentInstance, recorded bool, err error)
	SetLauncherHolds(ctx context.Context, launcher string, holds bool) error
	ListLauncherMachines(ctx context.Context, launcher string) ([]db.LauncherMachine, error)
}

// Metrics is where the harness states the launchers report are kept: the run's health series.
type Metrics interface {
	RecordHarnessState(ctx context.Context, at time.Time, states []domain.HarnessState) error
}

// MetricsDB is Metrics in the metrics store at URL.
type MetricsDB struct{ URL string }

func (m MetricsDB) RecordHarnessState(ctx context.Context, at time.Time, states []domain.HarnessState) error {
	return metricsdb.RecordHarnessState(ctx, m.URL, at, states)
}

// Settings is what the platform's config decides about hosted machines.
type Settings struct {
	// Image is what a hosted machine boots unless its platform experiment names its own, and
	// APIURL where it reaches this API.
	Image  string
	APIURL string
	// SigningSecret derives each agent's own token, handed out inside its environment.
	SigningSecret string
	// BootLimit is how long a machine may go unreported after it was asked for, or stay starting
	// after it was first reported, before that is recorded as an exit.
	BootLimit time.Duration
	// ProgressLimit is how long a running machine may go without its command printing a new
	// progress marker before that is recorded as an exit.
	ProgressLimit time.Duration
	// MaxNewPerAnswer is how many machines one answer asks a launcher to create. The rest wait
	// for the next answer, so that a pass ends well inside BootLimit.
	MaxNewPerAnswer int
}

type Handler struct {
	store   Store
	metrics Metrics
	cfg     Settings
	logger  *zap.Logger
}

func NewHandler(store Store, metrics Metrics, cfg Settings, logger *zap.Logger) *Handler {
	if store == nil || metrics == nil || cfg.Image == "" || cfg.APIURL == "" || cfg.SigningSecret == "" || cfg.BootLimit <= 0 || cfg.ProgressLimit <= 0 || cfg.MaxNewPerAnswer <= 0 || logger == nil {
		panic("launcherapi.NewHandler: store, metrics, logger and every setting are required")
	}
	return &Handler{store: store, metrics: metrics, cfg: cfg, logger: logger}
}

// Observed and ObservedInstance are the launcher contract's (contract/launcherapi).
type (
	Observed         = contract.Observed
	ObservedInstance = contract.ObservedInstance
)

const (
	stateStarting = contract.StateStarting
	stateRunning  = contract.StateRunning
	stateExited   = contract.StateExited
	stateUnknown  = contract.StateUnknown
)

var launchDigestPattern = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

// maxRequestBytes holds a log tail for each of several thousand instances.
const maxRequestBytes = 16 << 20

// Desired and Refusal are the launcher contract's (contract/launcherapi).
type (
	Desired = contract.Desired
	Refusal = contract.Refusal
)

const refusalPassDoesNotFit = contract.RefusalPassDoesNotFit

// kept is one thing a launcher keeps a machine for: an agent, or a platform experiment's
// coordinator, whose agentID is empty. Both are evaluated by the same steps.
type kept struct {
	platformExperimentID, agentID string
	// role is its role in the run's health series, and agentType the agent type it runs.
	role, agentType string
	instance        domain.AgentInstance
	ending          bool
	// launch is launchOf the image this one boots and the launcher's digest.
	launch string
	// describe takes how many machines of this one ended before.
	describe func(ended int) (Instance, error)
}

// launchOf identifies what a machine is started with on the platform's and the launcher's side.
// Restarts are spent under one launch: a machine that cannot run under an image or a launcher
// config says nothing about the next.
func launchOf(image, launchDigest string) string {
	digest := sha256.Sum256([]byte(image + "\x00" + launchDigest))
	return hex.EncodeToString(digest[:16])
}

// imageOf is the image a platform experiment's machines boot: the one it names, or the
// platform's own.
func (h *Handler) imageOf(agentImage string) string {
	if agentImage != "" {
		return agentImage
	}
	return h.cfg.Image
}

// reconcile records what the launcher observed and evaluates everything else: which agents and
// coordinators are the launcher's, which of them still run, which have domain.AgentWindDown
// (a coordinator domain.CoordinatorWindDown) left, and which get another machine after theirs ended. One that cannot be evaluated is
// logged and left out and the rest of the list stands. It also records whether the launcher
// holds machines once it has acted on the answer, which is what its removal waits for.
//
// With a refusal everything is recorded and evaluated the same and no new machine is asked for:
// the answer carries the refusal beside the machines to keep and the ones that end.
func (h *Handler) reconcile(ctx context.Context, launcher string, observed Observed, refusal *Refusal) (*Desired, error) {
	// A machine this answer no longer names is taken down by the launcher: that is written
	// down first, as the platform's own doing, so no later answer reads it as lost.
	if err := h.store.ReleaseInstances(ctx, launcher, domain.AgentWindDown, domain.CoordinatorWindDown); err != nil {
		return nil, err
	}
	agents, err := h.store.ListLauncherAgents(ctx, launcher, domain.AgentWindDown)
	if err != nil {
		return nil, err
	}
	coordinators, err := h.store.ListLauncherCoordinators(ctx, launcher, domain.CoordinatorWindDown)
	if err != nil {
		return nil, err
	}
	seen := map[string]ObservedInstance{}
	for _, instance := range observed.Instances {
		seen[instance.Name] = instance
	}

	var all []kept
	for _, agent := range agents {
		if agent.Instance == nil {
			h.logger.Error("launcher list holds an agent that is not hosted", zap.String("launcher", launcher), zap.String("platformExpID", agent.PlatformExperimentID), zap.String("agentID", agent.AgentID))
			continue
		}
		signup, image := agent.AgentSignup, h.imageOf(agent.AgentImage)
		all = append(all, kept{
			platformExperimentID: agent.PlatformExperimentID, agentID: agent.AgentID, role: string(agent.Role), agentType: agent.AgentType, instance: *agent.Instance, ending: agent.Ended != "",
			launch: launchOf(image, observed.LaunchDigest),
			describe: func(ended int) (Instance, error) {
				return instanceFor(signup, ended, image, h.cfg.APIURL, h.cfg.SigningSecret, observed.LaunchDigest)
			},
		})
	}
	for _, coordinator := range coordinators {
		pe, image := coordinator.PlatformExperimentID, h.imageOf(coordinator.AgentImage)
		all = append(all, kept{
			platformExperimentID: pe, role: domain.CoordinatorHealthRole, agentType: domain.CoordinatorFlavor, instance: coordinator.Instance, ending: coordinator.Ending,
			launch: launchOf(image, observed.LaunchDigest),
			describe: func(ended int) (Instance, error) {
				return coordinatorInstanceFor(pe, ended, image, h.cfg.APIURL, h.cfg.SigningSecret, observed.LaunchDigest)
			},
		})
	}

	out := &Desired{Instances: []Instance{}, Ending: []string{}, Image: h.cfg.Image, Refusal: refusal}
	asked := 0
	var states []domain.HarnessState
	for _, one := range all {
		log := h.logger.With(zap.String("launcher", launcher), zap.String("platformExpID", one.platformExperimentID), zap.String("agentID", one.agentID))
		current := one.instance
		// A machine is named after how many ended before it, whether or not their end spent a
		// restart, so every one that ends is followed by another name.
		describe := func(restarts, renewals int) (Instance, error) {
			return one.describe(restarts + renewals)
		}
		if one.ending {
			// Named as it was created, whatever config says now: a new name would have its
			// machine destroyed in the middle of its last turn.
			if current.Name != "" {
				out.Ending = append(out.Ending, current.Name)
			}
			continue
		}
		// The restart budget is spent under one launch. Under another it is whole, and one that
		// was stopped is asked for again: what stopped it may have been the image or the
		// launcher's config.
		if current.Launch == one.launch && current.Stopped {
			continue
		}
		instance, err := describe(current.Restarts, current.Renewals)
		if err != nil {
			log.Error("hosted instance cannot be described", zap.Error(err))
			continue
		}
		// What the launcher reports counts only for the machine that is still the one wanted. A
		// name that config has since changed is replaced and its end is nobody's fault.
		if current.Name == instance.Name {
			if reported := seen[current.Name]; reported.State == stateRunning && reported.HarnessState != "" {
				state, err := parseHarnessState(reported.HarnessState)
				if err != nil {
					log.Warn("hosted instance reported a harness state that cannot be read", zap.String("instance", current.Name), zap.Error(err))
				} else {
					state.PlatformExperimentID, state.AgentID, state.Role, state.AgentType = one.platformExperimentID, one.agentID, one.role, one.agentType
					states = append(states, state)
				}
			}
			after, ended, err := h.observe(ctx, one, seen, instance.Env["HL_TOKEN"])
			if err != nil {
				log.Error("record instance outcome failed", zap.Error(err))
				out.Instances = append(out.Instances, instance)
				continue
			}
			if ended {
				log.Warn("hosted instance ended", zap.String("instance", current.Name), zap.Int("restarts", after.Restarts), zap.Bool("stopped", after.Stopped), zap.String("why", after.ExitLog))
				if after.Stopped {
					continue
				}
				current.Name = ""
				if instance, err = describe(after.Restarts, after.Renewals); err != nil {
					log.Error("hosted instance cannot be described", zap.Error(err))
					continue
				}
			}
		}
		if instance.Name != current.Name {
			// The machine recorded is the desired one until a new name is: an answer without it
			// would have the launcher destroy a machine the platform still asks for.
			keepCurrent := func() {
				if current.Name != "" {
					out.Instances = append(out.Instances, Instance{Name: current.Name, Image: instance.Image, Env: instance.Env})
				}
			}
			// A launcher creates one machine after another, so an answer asks for a bounded
			// number of new ones and the rest wait for the next: a pass must end, and its
			// machines be reported, well inside the boot limit. A refused launcher is asked for
			// none.
			if refusal != nil || asked == h.cfg.MaxNewPerAnswer {
				keepCurrent()
				continue
			}
			asked++
			if err := h.store.AskInstance(ctx, one.platformExperimentID, one.agentID, instance.Name, one.launch); err != nil {
				log.Error("record instance name failed", zap.Error(err))
				keepCurrent()
				continue
			}
		}
		out.Instances = append(out.Instances, instance)
	}
	// The states are health, which an answer does not wait for: one that is not written is
	// followed by the next pass's.
	if len(states) > 0 {
		if err := h.metrics.RecordHarnessState(ctx, time.Now().UTC(), states); err != nil {
			h.logger.Error("record harness states failed", zap.String("launcher", launcher), zap.Error(err))
		}
	}
	// Written before the answer leaves: a launcher is removed only once a question of it listed
	// no machine and was answered with none, and an answer it never got asked for nothing.
	if err := h.store.SetLauncherHolds(ctx, launcher, len(observed.Instances) > 0 || len(out.Instances) > 0); err != nil {
		return nil, err
	}
	return out, nil
}

// observe holds the machine asked for under one.instance.Name against what the launcher reports
// of that name, and records its end when the report says so:
//   - exited: at once, with the end of its output. One its provider ended at the provider's own
//     lifetime (expired) is recorded as that, which spends no restart.
//   - reported before and not now: at once. The provider took it away. A machine the platform
//     released itself is never here: ReleaseInstances cleared its name when it stopped being
//     asked for.
//   - never reported: once the boot limit has passed since it was asked for. No machine could
//     be created, and the launcher's own log says what its provider answered.
//   - starting: once the boot limit has passed since it was first reported.
//   - running: once the progress limit has passed since its command last printed a progress
//     marker the platform had not seen, or since it was first reported when it never printed
//     one. A report whose marker the launcher could not read is held to the same limit by the
//     same clock, and its end is recorded as progress unreadable, which spends no restart: an
//     outage of the provider's log says nothing of the command, and must not stop every agent.
//   - unknown, of a machine last recorded running: nothing is recorded, so it stays running, and
//     it is ended by the progress limit as a running one is. A host unreachable for a pass ends
//     nothing, and one that stays away is not kept past the limit.
//   - unknown, of a machine never recorded running: as starting.
//
// The limits are counted by the store on the database's clock. token is the one this API
// minted for the machine, which its output must not carry into the database.
func (h *Handler) observe(ctx context.Context, one kept, seen map[string]ObservedInstance, token string) (domain.AgentInstance, bool, error) {
	name := one.instance.Name
	end := func(how db.InstanceEnd, exitLog string) (domain.AgentInstance, bool, error) {
		limit := h.cfg.BootLimit
		if how == db.InstanceSilent || how == db.InstanceProgressUnreadable {
			limit = h.cfg.ProgressLimit
		}
		return h.store.EndInstance(ctx, one.platformExperimentID, one.agentID, name, how, exitLog, limit, domain.MaxAgentInstanceRestarts)
	}
	reported, listed := seen[name]
	switch {
	case !listed && one.instance.SeenAt != nil:
		return end(db.InstanceGone, "the provider lost the machine: it was running, the platform still asks for it, and its provider no longer lists it")
	case !listed:
		return end(db.InstanceNeverSeen, fmt.Sprintf("no machine was listed %s after it was asked for, which is the boot limit: its launcher could not create it, and the launcher's log says what the provider answered", h.cfg.BootLimit))
	case reported.State == stateExited:
		exitLog := reported.Log
		if token != "" {
			exitLog = strings.ReplaceAll(exitLog, token, "<HL_TOKEN>")
		}
		how := db.InstanceExited
		if reported.Expired {
			how = db.InstanceExpired
		}
		return end(how, lastBytes(exitLog, domain.MaxAgentInstanceLogBytes))
	case reported.State == stateUnknown && one.instance.Reported == stateRunning:
		return end(db.InstanceSilent, fmt.Sprintf("the machine was running, its provider stopped saying whether it still does, and its command reported no progress for %s, which is the progress limit", h.cfg.ProgressLimit))
	}
	state := reported.State
	if state == stateUnknown {
		state = stateStarting
	}
	activity := ""
	if reported.State == stateRunning {
		activity = said(reported.Activity, token)
	}
	if err := h.store.ObserveInstance(ctx, one.platformExperimentID, one.agentID, name, state, reported.Progress, h.cfg.ProgressLimit, activity, reported.Machine); err != nil {
		return domain.AgentInstance{}, false, err
	}
	switch {
	case state == stateStarting:
		return end(db.InstanceStillStarting, fmt.Sprintf("the machine was still starting %s after it was first listed, which is the boot limit", h.cfg.BootLimit))
	case reported.State == stateRunning && reported.Progress == nil:
		return end(db.InstanceProgressUnreadable, fmt.Sprintf("the machine was running, no progress of its command was read for %s, which is the progress limit, and its launcher cannot read its output: whether the command still works is not known, so the machine is replaced and no restart is spent", h.cfg.ProgressLimit))
	case reported.State == stateRunning:
		return end(db.InstanceSilent, fmt.Sprintf("the machine was running and its command reported no progress for %s, which is the progress limit", h.cfg.ProgressLimit))
	}
	return domain.AgentInstance{}, false, nil
}

// said is the sentence an agent printed of what it is doing, as the platform stores it: one
// line, without the token this API minted for the machine, which its output must not carry into
// the database.
func said(activity, token string) string {
	if token != "" {
		activity = strings.ReplaceAll(activity, token, "<HL_TOKEN>")
	}
	return strings.Join(strings.Fields(strings.ToValidUTF8(activity, "")), " ")
}

var instructionsDigestPattern = regexp.MustCompile(fmt.Sprintf(`^[0-9a-f]{%d}$`, domain.InstructionsDigestLength))

// parseHarnessState reads the state line a harness printed (agents/experimentator, progress.py):
// key=value fields, subagents, turn and since required, instructions_digest and version, the
// version of its agent type, once the harness runs instructions, the model_ ones present once the harness
// has read a rate limit, and then model_status among them.
func parseHarnessState(line string) (domain.HarnessState, error) {
	var state domain.HarnessState
	fields := map[string]string{}
	for _, field := range strings.Fields(line) {
		key, value, found := strings.Cut(field, "=")
		if !found {
			return state, fmt.Errorf("%q is no key=value field", field)
		}
		if _, twice := fields[key]; twice {
			return state, fmt.Errorf("%s is given twice", key)
		}
		fields[key] = value
	}
	unixTime := func(key string) (time.Time, error) {
		seconds, err := strconv.ParseInt(fields[key], 10, 64)
		if err != nil || seconds <= 0 {
			return time.Time{}, fmt.Errorf("%s %q is no Unix time", key, fields[key])
		}
		return time.Unix(seconds, 0).UTC(), nil
	}
	modelRead := false
	for key, value := range fields {
		var err error
		switch key {
		case "subagents":
			state.Subagents, err = strconv.Atoi(value)
			if err != nil || state.Subagents < 0 {
				return state, fmt.Errorf("subagents %q is no count", value)
			}
		case "turn":
			switch value {
			case "working":
			case "waiting":
				state.TurnWaiting = true
			default:
				return state, fmt.Errorf("turn %q is neither working nor waiting", value)
			}
		case "since":
			if state.TurnSince, err = unixTime(key); err != nil {
				return state, err
			}
		case "instructions_digest":
			if !instructionsDigestPattern.MatchString(value) {
				return state, fmt.Errorf("instructions_digest %q is no %d lowercase hex characters", value, domain.InstructionsDigestLength)
			}
			state.InstructionsDigest = value
		case "version":
			state.AgentTypeVersion, err = strconv.Atoi(value)
			if err != nil || state.AgentTypeVersion < 1 {
				return state, fmt.Errorf("version %q is no version", value)
			}
		case "model_status":
			switch value {
			case "allowed", "allowed_warning":
			case "rejected":
				state.Model.Rejected = true
			default:
				return state, fmt.Errorf("model_status %q is none of allowed, allowed_warning and rejected", value)
			}
		case "model_type":
		case "model_utilization":
			u, err := strconv.ParseFloat(value, 64)
			if err != nil || u < 0 || u > 1 {
				return state, fmt.Errorf("model_utilization %q is no number from 0 to 1", value)
			}
			state.Model.Utilization = &u
		case "model_resets_at":
			at, err := unixTime(key)
			if err != nil {
				return state, err
			}
			state.Model.ResetsAt = &at
		default:
			return state, fmt.Errorf("%s is no field of a harness state", key)
		}
		modelRead = modelRead || strings.HasPrefix(key, "model_")
	}
	for _, key := range []string{"subagents", "turn", "since"} {
		if _, given := fields[key]; !given {
			return state, fmt.Errorf("%s is required", key)
		}
	}
	if _, given := fields["model_status"]; modelRead && !given {
		return state, fmt.Errorf("model_status is required with the other model_ fields")
	}
	return state, nil
}

// lastBytes keeps the end of text, cut at a line where one starts inside the limit.
func lastBytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[len(text)-limit:]
	if newline := strings.IndexByte(text, '\n'); newline >= 0 && newline < len(text)-1 {
		text = text[newline+1:]
	}
	return strings.ToValidUTF8(text, "")
}

func validateObserved(o Observed) error {
	if !launchDigestPattern.MatchString(o.LaunchDigest) {
		return fmt.Errorf("launch_digest must be 16 to 64 lowercase hex characters")
	}
	if o.Instances == nil {
		return fmt.Errorf("instances is required: every instance the provider lists, or an empty list")
	}
	for _, instance := range o.Instances {
		if instance.Name == "" || len(instance.Name) > MaxInstanceNameLength {
			return fmt.Errorf("an observed instance needs a name of at most %d characters", MaxInstanceNameLength)
		}
		if instance.Progress != nil && instance.State != stateRunning {
			return fmt.Errorf("progress is reported of a running instance, and %s is %s", instance.Name, instance.State)
		}
		if instance.HarnessState != "" && instance.State != stateRunning {
			return fmt.Errorf("harness_state is reported of a running instance, and %s is %s", instance.Name, instance.State)
		}
		if instance.Expired && instance.State != stateExited {
			return fmt.Errorf("expired is reported of an exited instance, and %s is %s", instance.Name, instance.State)
		}
	}
	if o.ReconcileIntervalSeconds <= 0 || o.CreatePauseMilliseconds < 0 {
		return fmt.Errorf("reconcile_interval_seconds must be positive and create_pause_milliseconds must not be negative")
	}
	return nil
}

// fits is the refusal of a launcher whose pass can outlast the boot limit or the progress limit,
// nil for one that fits. A running
// machine's progress reaches the platform once a pass, so the progress limit has to cover two
// of them as well.
//
// The boot limit: An answer asks for at most
// MaxNewPerAnswer new machines, which the launcher creates one after another with its pause
// between them and reports with its next question, one interval later. A machine not reported
// within the boot limit of being asked for spends a restart, so the limit has to cover that.
func (h *Handler) fits(o Observed) *Refusal {
	pause := time.Duration(o.CreatePauseMilliseconds) * time.Millisecond
	interval := time.Duration(o.ReconcileIntervalSeconds) * time.Second
	pass := time.Duration(h.cfg.MaxNewPerAnswer)*pause + interval
	for _, limit := range []struct {
		key   string
		value time.Duration
	}{{"boot_limit_seconds", h.cfg.BootLimit}, {"progress_limit_seconds", h.cfg.ProgressLimit}} {
		if 2*pass > limit.value {
			return &Refusal{Reason: refusalPassDoesNotFit, Message: fmt.Sprintf("this launcher's pass does not fit the platform's agents.%s: %d new instances per answer x create_pause %s + reconcile_interval %s = %s, and twice that must be at most agents.%s %s. Lower the launcher's create_pause_milliseconds or reconcile_interval_seconds, or change the platform's agents.max_new_instances_per_answer or agents.%s",
				limit.key, h.cfg.MaxNewPerAnswer, pause, interval, pass, limit.key, limit.value, limit.key)}
		}
	}
	return nil
}

// Held and HeldMachine are the launcher contract's (contract/launcherapi).
type (
	Held        = contract.Held
	HeldMachine = contract.HeldMachine
)

// RegisterHuma registers the launcher-facing operations on doc. Paths are relative to the
// /internal/launchers mount.
func RegisterHuma(doc *apidocs.Doc, h *Handler) {
	apidocs.Register(doc, apidocs.AudienceLauncher, huma.Operation{
		OperationID: "launcher-machines", Method: "GET", Path: "/{name}/machines",
		Summary: "List the machines the platform holds for a launcher", Tags: []string{"agent-launcher"},
		Description: "Every machine the platform holds for the launcher, an agent's or a coordinator's: the name it is asked for under and the provider's id the launcher last reported it by. It changes nothing. " +
			"The platform gives up on a machine it saw that the launcher no longer reports, so a launcher whose provider's list leaves one of these out reads it by its id before it reports, and leaves it out only when the provider says it is gone.",
	}, func(ctx context.Context, in *struct {
		Name string `path:"name"`
	}) (*struct{ Body *Held }, error) {
		machines, err := h.store.ListLauncherMachines(ctx, in.Name)
		if err != nil {
			return nil, auth.Internal(h.logger, "list launcher machines failed", err, zap.String("launcher", in.Name))
		}
		out := &Held{Machines: make([]HeldMachine, 0, len(machines))}
		for _, m := range machines {
			out.Machines = append(out.Machines, HeldMachine{Name: m.Name, Machine: m.Machine})
		}
		return &struct{ Body *Held }{Body: out}, nil
	})

	apidocs.Register(doc, apidocs.AudienceLauncher, huma.Operation{
		OperationID: "launcher-reconcile", Method: "POST", Path: "/{name}/reconcile",
		Summary: "Report a launcher's instances and receive the ones it should keep", Tags: []string{"agent-launcher"},
		MaxBodyBytes: maxRequestBytes,
		Description: "The launcher sends what its provider lists, for an exited instance the end of its output, for a running one its progress marker, what its agent says it is doing (activity) and what its agent's harness last said of itself (harness_state: subagents, turn and the model provider's rate limit), and how it paces its passes. The answer is its complete desired state. " +
			"instances: each {name, image, env, ends_at} should exist and be running: one per hosted agent, and one per platform experiment whose coordinator the platform runs. " +
			"ends_at is the latest moment its platform experiment can still have a machine, wind-down included, and the launcher does nothing with it: it stops an instance when its name is in ending. " +
			"ending: names whose agent's run is over; each is told to stop and is otherwise left to finish, neither created nor replaced, and destroyed once its command has exited. " +
			"image: the image an instance boots unless its platform experiment names its own, which a launcher with a warm pool warms while no instance is asked for. " +
			"Any other instance of the launcher should not exist, and an answer asks for a bounded number of instances that do not exist yet: the rest follow in the next. " +
			"The platform gives up on an instance that exited, that the provider listed and no longer does, that was not listed within the platform's boot limit of being asked for, " +
			"that stayed starting for the boot limit, or that runs while the progress marker it reports has not changed for the platform's progress limit. An instance reported unknown is held to the progress limit if it was last reported running and to the boot limit otherwise. An exited instance reported with expired, which its provider ended at the provider's own lifetime, is given up on as expired, and a running one reported without progress, whose output the launcher could not read, as progress_unreadable once no progress was recorded for the progress limit: neither spends one of the agent's restarts or stops it. That is recorded on its agent, and the launcher never replaces one on its own: " +
			"the answer either asks for the agent under a new name or no longer names it. The answer is a list and carries no rule; ask again for the next one. " +
			"A launcher whose pass can outlast the platform's boot limit or progress limit is answered 200 with refusal {reason: pass_does_not_fit, message}: the two lists stand and are acted on, so instances whose run is over are still stopped and destroyed, no instance that does not exist yet is asked for, and the launcher creates none until an answer carries no refusal.",
	}, func(ctx context.Context, in *struct {
		Name string `path:"name"`
		Body Observed
	}) (*struct{ Body *Desired }, error) {
		if err := validateObserved(in.Body); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		desired, err := h.reconcile(ctx, in.Name, in.Body, h.fits(in.Body))
		if err != nil {
			// Never the answer itself: it holds every agent's token.
			return nil, auth.Internal(h.logger, "launcher reconcile failed", err, zap.String("launcher", in.Name))
		}
		return &struct{ Body *Desired }{Body: desired}, nil
	})
}
