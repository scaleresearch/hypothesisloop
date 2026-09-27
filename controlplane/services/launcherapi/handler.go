package launcherapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

type Store interface {
	ListLauncherAgents(ctx context.Context, launcher string, windDown time.Duration) ([]db.LauncherAgent, error)
	ListLauncherCoordinators(ctx context.Context, launcher string, windDown time.Duration) ([]db.LauncherCoordinator, error)
	// The three writes take a hosted agent's id, or none for a platform experiment's coordinator.
	AskInstance(ctx context.Context, platformExpID, agentID, name, launch string) error
	ObserveInstance(ctx context.Context, platformExpID, agentID, name, state string, progress *string, progressLimit time.Duration) error
	EndInstance(ctx context.Context, platformExpID, agentID, name string, how db.InstanceEnd, exitLog string, limit time.Duration, maxRestarts int) (after domain.AgentInstance, recorded bool, err error)
	SetLauncherHolds(ctx context.Context, launcher string, holds bool) error
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
	store  Store
	cfg    Settings
	logger *zap.Logger
}

func NewHandler(store Store, cfg Settings, logger *zap.Logger) *Handler {
	if store == nil || cfg.Image == "" || cfg.APIURL == "" || cfg.SigningSecret == "" || cfg.BootLimit <= 0 || cfg.ProgressLimit <= 0 || cfg.MaxNewPerAnswer <= 0 || logger == nil {
		panic("launcherapi.NewHandler: store, logger and every setting are required")
	}
	return &Handler{store: store, cfg: cfg, logger: logger}
}

// Observed is what a launcher found on its provider account before it asked: every instance
// whose name starts with its own prefix, by the name this API gave it.
type Observed struct {
	LaunchDigest string             `json:"launch_digest" doc:"Lowercase hex digest of everything the launcher itself starts an instance with: command, size and the environment it adds. A change replaces every running instance and gives each agent its restarts back."`
	Instances    []ObservedInstance `json:"instances" doc:"Every instance the launcher's provider lists for it, an empty list when there is none."`
	// The two below say how long one pass of this launcher can take, which the boot limit has
	// to cover.
	ReconcileIntervalSeconds int `json:"reconcile_interval_seconds" doc:"How long the launcher waits between two questions."`
	CreatePauseMilliseconds  int `json:"create_pause_milliseconds" doc:"How long the launcher waits between two creates of one pass."`
}

type ObservedInstance struct {
	Name  string `json:"name" doc:"The name from an earlier answer, without the launcher's own prefix."`
	State string `json:"state" enum:"starting,running,exited"`
	Log   string `json:"log,omitempty" doc:"For an exited instance, the end of what its command wrote."`
	// Progress is a pointer so that "could not be read" differs from "read, and none printed".
	Progress *string `json:"progress,omitempty" maxLength:"64" doc:"For a running instance, the progress marker its command last printed: what follows hl-progress on the newest such line of its output, empty when it has printed none. Left out when the launcher could not read the output. The platform compares it with the one before and reads nothing else from it."`
}

const (
	stateStarting = domain.InstanceStarting
	stateRunning  = domain.InstanceRunning
	stateExited   = "exited"
)

var launchDigestPattern = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

// maxRequestBytes holds a log tail for each of several thousand instances.
const maxRequestBytes = 16 << 20

// Desired is one launcher's complete desired state. Instances should exist and be running.
// Ending names instances whose run is over: they are told to stop and are otherwise left
// alone for a last turn, neither created nor replaced nor destroyed. An
// instance in neither list should not exist.
type Desired struct {
	Instances []Instance `json:"instances"`
	Ending    []string   `json:"ending"`
}

// kept is one thing a launcher keeps a machine for: an agent, or a platform experiment's
// coordinator, whose agentID is empty. Both are evaluated by the same steps.
type kept struct {
	platformExperimentID, agentID string
	instance                      domain.AgentInstance
	ending                        bool
	// launch is launchOf the image this one boots and the launcher's digest.
	launch   string
	describe func(restarts int) (Instance, error)
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
// left, and which get another machine after theirs ended. One that cannot be evaluated is
// logged and left out and the rest of the list stands. It also records whether the launcher
// holds machines once it has acted on the answer, which is what its removal waits for.
func (h *Handler) reconcile(ctx context.Context, launcher string, observed Observed) (*Desired, error) {
	agents, err := h.store.ListLauncherAgents(ctx, launcher, domain.AgentWindDown)
	if err != nil {
		return nil, err
	}
	coordinators, err := h.store.ListLauncherCoordinators(ctx, launcher, domain.AgentWindDown)
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
			platformExperimentID: agent.PlatformExperimentID, agentID: agent.AgentID, instance: *agent.Instance, ending: agent.Ended != "",
			launch: launchOf(image, observed.LaunchDigest),
			describe: func(restarts int) (Instance, error) {
				return instanceFor(signup, restarts, image, h.cfg.APIURL, h.cfg.SigningSecret, observed.LaunchDigest)
			},
		})
	}
	for _, coordinator := range coordinators {
		pe, model, image := coordinator.PlatformExperimentID, coordinator.Model, h.imageOf(coordinator.AgentImage)
		all = append(all, kept{
			platformExperimentID: pe, instance: coordinator.Instance, ending: coordinator.Ending,
			launch: launchOf(image, observed.LaunchDigest),
			describe: func(restarts int) (Instance, error) {
				return coordinatorInstanceFor(pe, model, restarts, image, h.cfg.APIURL, h.cfg.SigningSecret, observed.LaunchDigest)
			},
		})
	}

	out := &Desired{Instances: []Instance{}, Ending: []string{}}
	asked := 0
	for _, one := range all {
		log := h.logger.With(zap.String("launcher", launcher), zap.String("platformExpID", one.platformExperimentID), zap.String("agentID", one.agentID))
		current := one.instance
		if one.ending {
			// Named as it was created, whatever config says now: a new name would have its
			// machine destroyed in the middle of its last turn.
			if current.Name != "" {
				out.Ending = append(out.Ending, current.Name)
			}
			continue
		}
		// Restarts were spent under one launch. Under another none is, and one that was stopped
		// is asked for again: what stopped it may have been the image or the launcher's config.
		restarts := current.Restarts
		if current.Launch != one.launch {
			restarts = 0
		} else if current.Stopped {
			continue
		}
		instance, err := one.describe(restarts)
		if err != nil {
			log.Error("hosted instance cannot be described", zap.Error(err))
			continue
		}
		// What the launcher reports counts only for the machine that is still the one wanted. A
		// name that config has since changed is replaced and its end is nobody's fault.
		if current.Name == instance.Name {
			after, ended, err := h.observe(ctx, one, seen, instance.Env["HL_TOKEN"])
			if err != nil {
				log.Error("record instance outcome failed", zap.Error(err))
				continue
			}
			if ended {
				log.Warn("hosted instance ended", zap.String("instance", current.Name), zap.Int("restarts", after.Restarts), zap.Bool("stopped", after.Stopped), zap.String("why", after.ExitLog))
				if after.Stopped {
					continue
				}
				current.Name = ""
				if instance, err = one.describe(after.Restarts); err != nil {
					log.Error("hosted instance cannot be described", zap.Error(err))
					continue
				}
			}
		}
		if instance.Name != current.Name {
			// A launcher creates one machine after another, so an answer asks for a bounded
			// number of new ones and the rest wait for the next: a pass must end, and its
			// machines be reported, well inside the boot limit.
			if asked == h.cfg.MaxNewPerAnswer {
				continue
			}
			asked++
			if err := h.store.AskInstance(ctx, one.platformExperimentID, one.agentID, instance.Name, one.launch); err != nil {
				log.Error("record instance name failed", zap.Error(err))
				continue
			}
		}
		out.Instances = append(out.Instances, instance)
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
//   - exited: at once, with the end of its output.
//   - reported before and not now: at once. The provider took it away.
//   - never reported: once the boot limit has passed since it was asked for. No machine could
//     be created, and the launcher's own log says what its provider answered.
//   - starting: once the boot limit has passed since it was first reported.
//   - running: once the progress limit has passed since its command last printed a progress
//     marker the platform had not seen, or since it was first reported when it never printed
//     one. A report whose marker the launcher could not read ends nothing.
//
// The limits are counted by the store on the database's clock. token is the one this API
// minted for the machine, which its output must not carry into the database.
func (h *Handler) observe(ctx context.Context, one kept, seen map[string]ObservedInstance, token string) (domain.AgentInstance, bool, error) {
	name := one.instance.Name
	end := func(how db.InstanceEnd, exitLog string) (domain.AgentInstance, bool, error) {
		limit := h.cfg.BootLimit
		if how == db.InstanceSilent {
			limit = h.cfg.ProgressLimit
		}
		return h.store.EndInstance(ctx, one.platformExperimentID, one.agentID, name, how, exitLog, limit, domain.MaxAgentInstanceRestarts)
	}
	reported, listed := seen[name]
	switch {
	case !listed && one.instance.SeenAt != nil:
		return end(db.InstanceGone, "the machine was running and its provider no longer lists it")
	case !listed:
		return end(db.InstanceNeverSeen, fmt.Sprintf("no machine was listed %s after it was asked for, which is the boot limit: its launcher could not create it, and the launcher's log says what the provider answered", h.cfg.BootLimit))
	case reported.State == stateExited:
		exitLog := reported.Log
		if token != "" {
			exitLog = strings.ReplaceAll(exitLog, token, "<HL_TOKEN>")
		}
		return end(db.InstanceExited, lastBytes(exitLog, domain.MaxAgentInstanceLogBytes))
	}
	if err := h.store.ObserveInstance(ctx, one.platformExperimentID, one.agentID, name, reported.State, reported.Progress, h.cfg.ProgressLimit); err != nil {
		return domain.AgentInstance{}, false, err
	}
	switch {
	case reported.State == stateStarting:
		return end(db.InstanceStillStarting, fmt.Sprintf("the machine was still starting %s after it was first listed, which is the boot limit", h.cfg.BootLimit))
	case reported.State == stateRunning && reported.Progress != nil:
		return end(db.InstanceSilent, fmt.Sprintf("the machine was running and its command reported no progress for %s, which is the progress limit", h.cfg.ProgressLimit))
	}
	return domain.AgentInstance{}, false, nil
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

func (o Observed) validate() error {
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
	}
	if o.ReconcileIntervalSeconds <= 0 || o.CreatePauseMilliseconds < 0 {
		return fmt.Errorf("reconcile_interval_seconds must be positive and create_pause_milliseconds must not be negative")
	}
	return nil
}

// fits refuses a launcher whose pass can outlast the boot limit or the progress limit. A running
// machine's progress reaches the platform once a pass, so the progress limit has to cover two
// of them as well.
//
// The boot limit: An answer asks for at most
// MaxNewPerAnswer new machines, which the launcher creates one after another with its pause
// between them and reports with its next question, one interval later. A machine not reported
// within the boot limit of being asked for spends a restart, so the limit has to cover that.
func (h *Handler) fits(o Observed) error {
	pause := time.Duration(o.CreatePauseMilliseconds) * time.Millisecond
	interval := time.Duration(o.ReconcileIntervalSeconds) * time.Second
	pass := time.Duration(h.cfg.MaxNewPerAnswer)*pause + interval
	for _, limit := range []struct {
		key   string
		value time.Duration
	}{{"boot_limit_seconds", h.cfg.BootLimit}, {"progress_limit_seconds", h.cfg.ProgressLimit}} {
		if 2*pass > limit.value {
			return fmt.Errorf("this launcher's pass does not fit the platform's agents.%s: %d new instances per answer x create_pause %s + reconcile_interval %s = %s, and twice that must be at most agents.%s %s. Lower the launcher's create_pause_milliseconds or reconcile_interval_seconds, or change the platform's agents.max_new_instances_per_answer or agents.%s",
				limit.key, h.cfg.MaxNewPerAnswer, pause, interval, pass, limit.key, limit.value, limit.key)
		}
	}
	return nil
}

// RegisterHuma registers the launcher-facing operation on doc. Paths are relative to the
// /internal/launchers mount.
func RegisterHuma(doc *apidocs.Doc, h *Handler) {
	apidocs.Register(doc, apidocs.AudienceLauncher, huma.Operation{
		OperationID: "launcher-reconcile", Method: "POST", Path: "/{name}/reconcile",
		Summary: "Report a launcher's instances and receive the ones it should keep", Tags: []string{"agent-launcher"},
		MaxBodyBytes: maxRequestBytes,
		Description: "The launcher sends what its provider lists, for an exited instance the end of its output, for a running one its progress marker, and how it paces its passes. The answer is its complete desired state. " +
			"instances: each {name, image, env} should exist and be running: one per hosted agent, and one per platform experiment whose coordinator the platform runs. " +
			"ending: names whose agent's run is over; each is told to stop and is otherwise left to finish, neither created nor replaced nor destroyed. " +
			"Any other instance of the launcher should not exist, and an answer asks for a bounded number of instances that do not exist yet: the rest follow in the next. " +
			"The platform gives up on an instance that exited, that the provider listed and no longer does, that was not listed within the platform's boot limit of being asked for, " +
			"that stayed starting for the boot limit, or that runs while the progress marker it reports has not changed for the platform's progress limit. That is recorded on its agent, and the launcher never replaces one on its own: " +
			"the answer either asks for the agent under a new name or no longer names it. The answer is a list and carries no rule; ask again for the next one.",
	}, func(ctx context.Context, in *struct {
		Name string `path:"name"`
		Body Observed
	}) (*struct{ Body *Desired }, error) {
		if err := in.Body.validate(); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		if err := h.fits(in.Body); err != nil {
			return nil, huma.Error409Conflict(err.Error())
		}
		desired, err := h.reconcile(ctx, in.Name, in.Body)
		if err != nil {
			// Never the answer itself: it holds every agent's token.
			return nil, auth.Internal(h.logger, "launcher reconcile failed", err, zap.String("launcher", in.Name))
		}
		return &struct{ Body *Desired }{Body: desired}, nil
	})
}
