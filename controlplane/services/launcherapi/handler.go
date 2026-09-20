package launcherapi

import (
	"context"
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
	SetAgentInstanceName(ctx context.Context, platformExpID, agentID, name string) error
	RecordAgentInstanceExit(ctx context.Context, platformExpID, agentID, name, exitLog string, maxRestarts int) (bool, error)
	RecordAgentInstanceStuck(ctx context.Context, platformExpID, agentID, name, why string, bootLimit time.Duration, maxRestarts int) (bool, error)
	ListLauncherCoordinators(ctx context.Context, launcher string, windDown time.Duration) ([]db.LauncherCoordinator, error)
	SetCoordinatorInstanceName(ctx context.Context, platformExpID, name string) error
	RecordCoordinatorInstanceExit(ctx context.Context, platformExpID, name, exitLog string, maxRestarts int) (bool, error)
	RecordCoordinatorInstanceStuck(ctx context.Context, platformExpID, name, why string, bootLimit time.Duration, maxRestarts int) (bool, error)
}

type Handler struct {
	store Store
	// image is what every hosted agent boots, and apiURL where it reaches this API.
	image  string
	apiURL string
	// signingSecret derives each agent's own token, handed out inside its environment.
	signingSecret string
	// bootLimit is how long a machine may be reported as starting, counted from when it was
	// first asked for, before that is recorded as an exit.
	bootLimit time.Duration
	logger    *zap.Logger
}

func NewHandler(store Store, image, apiURL, signingSecret string, bootLimit time.Duration, logger *zap.Logger) *Handler {
	if store == nil || image == "" || apiURL == "" || signingSecret == "" || bootLimit <= 0 || logger == nil {
		panic("launcherapi.NewHandler: store, image, apiURL, signingSecret, bootLimit and logger are required")
	}
	return &Handler{store: store, image: image, apiURL: apiURL, signingSecret: signingSecret, bootLimit: bootLimit, logger: logger}
}

// Observed is what a launcher found on its provider account before it asked: every instance of
// its namespace, by the name this API gave it.
type Observed struct {
	LaunchDigest string             `json:"launch_digest" doc:"Lowercase hex digest of everything the launcher itself starts an instance with: command, size and the environment it adds. A change replaces every running instance."`
	Instances    []ObservedInstance `json:"instances" doc:"Every instance the launcher's provider lists for it, an empty list when there is none."`
	// CreateFailures are the instances of the last answer the launcher could not create.
	CreateFailures []CreateFailure `json:"create_failures,omitempty" doc:"Each instance of the last answer that the provider refused to create, with what it said. A name the provider lists is in instances and not here."`
}

type CreateFailure struct {
	Name   string `json:"name" doc:"The name from the answer, without the launcher's own prefix."`
	Reason string `json:"reason" doc:"What the provider answered, with every value of the instance's environment taken out."`
}

type ObservedInstance struct {
	Name  string `json:"name" doc:"The name from an earlier answer, without the launcher's own prefix."`
	State string `json:"state" enum:"starting,running,exited"`
	Log   string `json:"log,omitempty" doc:"For an exited instance, the end of what its command wrote."`
}

const (
	stateStarting = "starting"
	stateExited   = "exited"
	// stateNotCreated is no state a launcher reports of an instance: it is what a create failure
	// reads as beside the listed instances.
	stateNotCreated = "not created"
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
// coordinator. Both are evaluated by the same steps and differ in where their outcome is kept.
type kept struct {
	platformExperimentID string
	// agentID is empty for a coordinator.
	agentID  string
	instance domain.AgentInstance
	ending   bool
	describe func(restarts int) (Instance, error)
	setName  func(ctx context.Context, name string) error
	exited   func(ctx context.Context, name, exitLog string) (bool, error)
	stuck    func(ctx context.Context, name, why string) (bool, error)
}

// reconcile records what the launcher observed and evaluates everything else: which agents and
// coordinators are the launcher's, which of them still run, which have domain.AgentWindDown
// left, and which get another machine after theirs exited. One that cannot be evaluated is
// logged and left out and the rest of the list stands.
func (h *Handler) reconcile(ctx context.Context, launcher string, observed Observed) (*Desired, error) {
	agents, err := h.store.ListLauncherAgents(ctx, launcher, domain.AgentWindDown)
	if err != nil {
		return nil, err
	}
	coordinators, err := h.store.ListLauncherCoordinators(ctx, launcher, domain.AgentWindDown)
	if err != nil {
		return nil, err
	}
	// What the launcher says of a name: its machine as the provider lists it, or that none could
	// be created. A listed name is the machine's word, whatever an earlier create answered.
	seen := map[string]ObservedInstance{}
	for _, failure := range observed.CreateFailures {
		seen[failure.Name] = ObservedInstance{Name: failure.Name, State: stateNotCreated, Log: failure.Reason}
	}
	for _, instance := range observed.Instances {
		seen[instance.Name] = instance
	}

	var all []kept
	for _, agent := range agents {
		if agent.Instance == nil {
			h.logger.Error("launcher list holds an agent that is not hosted", zap.String("launcher", launcher), zap.String("platformExpID", agent.PlatformExperimentID), zap.String("agentID", agent.AgentID))
			continue
		}
		pe, id, signup := agent.PlatformExperimentID, agent.AgentID, agent.AgentSignup
		all = append(all, kept{
			platformExperimentID: pe, agentID: id, instance: *agent.Instance, ending: agent.Ending,
			describe: func(restarts int) (Instance, error) {
				return instanceFor(signup, restarts, h.image, h.apiURL, h.signingSecret, observed.LaunchDigest)
			},
			setName: func(ctx context.Context, name string) error { return h.store.SetAgentInstanceName(ctx, pe, id, name) },
			exited: func(ctx context.Context, name, exitLog string) (bool, error) {
				return h.store.RecordAgentInstanceExit(ctx, pe, id, name, exitLog, domain.MaxAgentInstanceRestarts)
			},
			stuck: func(ctx context.Context, name, why string) (bool, error) {
				return h.store.RecordAgentInstanceStuck(ctx, pe, id, name, why, h.bootLimit, domain.MaxAgentInstanceRestarts)
			},
		})
	}
	for _, coordinator := range coordinators {
		pe, model := coordinator.PlatformExperimentID, coordinator.Model
		all = append(all, kept{
			platformExperimentID: pe, instance: coordinator.Instance, ending: coordinator.Ending,
			describe: func(restarts int) (Instance, error) {
				return coordinatorInstanceFor(pe, model, restarts, h.image, h.apiURL, h.signingSecret, observed.LaunchDigest)
			},
			setName: func(ctx context.Context, name string) error { return h.store.SetCoordinatorInstanceName(ctx, pe, name) },
			exited: func(ctx context.Context, name, exitLog string) (bool, error) {
				return h.store.RecordCoordinatorInstanceExit(ctx, pe, name, exitLog, domain.MaxAgentInstanceRestarts)
			},
			stuck: func(ctx context.Context, name, why string) (bool, error) {
				return h.store.RecordCoordinatorInstanceStuck(ctx, pe, name, why, h.bootLimit, domain.MaxAgentInstanceRestarts)
			},
		})
	}

	out := &Desired{Instances: []Instance{}, Ending: []string{}}
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
		if reported, listed := seen[current.Name]; listed && current.Name != "" {
			// A machine that exited, one that never got past starting and one that could not be
			// created end the same way: the outcome is recorded, and another machine is asked
			// for while a restart is left. The last two are given the boot limit first.
			var recorded bool
			var err error
			switch reported.State {
			case stateExited:
				recorded, err = one.exited(ctx, current.Name, lastBytes(reported.Log, domain.MaxAgentInstanceLogBytes))
			case stateStarting:
				recorded, err = one.stuck(ctx, current.Name, fmt.Sprintf("the machine was still starting %s after it was asked for, which is the boot limit", h.bootLimit))
			case stateNotCreated:
				recorded, err = one.stuck(ctx, current.Name, fmt.Sprintf("no machine could be created in the %s after it was asked for, which is the boot limit. The last attempt failed with: %s",
					h.bootLimit, lastBytes(reported.Log, domain.MaxAgentInstanceLogBytes)))
			}
			if err != nil {
				log.Error("record instance outcome failed", zap.Error(err))
				continue
			}
			if recorded {
				stopped := current.Restarts >= domain.MaxAgentInstanceRestarts
				log.Warn("hosted instance ended", zap.String("instance", current.Name), zap.String("reported", reported.State), zap.Int("restartsBefore", current.Restarts), zap.Bool("stopped", stopped))
				if stopped {
					continue
				}
				current.Restarts++
				current.Name = ""
			}
		}
		instance, err := one.describe(current.Restarts)
		if err != nil {
			log.Error("hosted instance cannot be described", zap.Error(err))
			continue
		}
		if instance.Name != current.Name {
			if err := one.setName(ctx, instance.Name); err != nil {
				log.Error("record instance name failed", zap.Error(err))
				continue
			}
		}
		out.Instances = append(out.Instances, instance)
	}
	return out, nil
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
	}
	for _, failure := range o.CreateFailures {
		if failure.Name == "" || len(failure.Name) > MaxInstanceNameLength {
			return fmt.Errorf("a create failure needs a name of at most %d characters", MaxInstanceNameLength)
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
		Description: "The launcher sends what its provider lists, and for an exited instance the end of its output. The answer is its complete desired state. " +
			"instances: each {name, image, env} should exist and be running: one per hosted agent, and one per platform experiment whose coordinator the platform runs. " +
			"ending: names whose agent's run is over; each is told to stop and is otherwise left to finish, neither created nor replaced nor destroyed. " +
			"create_failures names what the provider refused to create since the last answer. " +
			"Any other instance of the launcher should not exist. An exited instance, and one reported as starting or as not created for longer than the platform's boot limit, " +
			"is recorded on its agent and is never replaced by the launcher on its own: " +
			"the answer either asks for the agent under a new name or no longer names it. The answer is a list and carries no rule; ask again for the next one.",
	}, func(ctx context.Context, in *struct {
		Name string `path:"name"`
		Body Observed
	}) (*struct{ Body *Desired }, error) {
		if err := in.Body.validate(); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		desired, err := h.reconcile(ctx, in.Name, in.Body)
		if err != nil {
			// Never the answer itself: it holds every agent's token.
			h.logger.Error("launcher reconcile failed", zap.String("launcher", in.Name), zap.Error(err))
			return nil, huma.Error500InternalServerError(auth.InternalError)
		}
		return &struct{ Body *Desired }{Body: desired}, nil
	})
}
