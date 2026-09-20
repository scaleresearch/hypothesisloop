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
}

type Handler struct {
	store Store
	// image is what every hosted agent boots, and apiURL where it reaches this API.
	image  string
	apiURL string
	// signingSecret derives each agent's own token, handed out inside its environment.
	signingSecret string
	logger        *zap.Logger
}

func NewHandler(store Store, image, apiURL, signingSecret string, logger *zap.Logger) *Handler {
	if store == nil || image == "" || apiURL == "" || signingSecret == "" || logger == nil {
		panic("launcherapi.NewHandler: store, image, apiURL, signingSecret and logger are required")
	}
	return &Handler{store: store, image: image, apiURL: apiURL, signingSecret: signingSecret, logger: logger}
}

// Observed is what a launcher found on its provider account before it asked: every instance of
// its namespace, by the name this API gave it.
type Observed struct {
	LaunchDigest string             `json:"launch_digest" doc:"Lowercase hex digest of everything the launcher itself starts an instance with: command, size and the environment it adds. A change replaces every running instance."`
	Instances    []ObservedInstance `json:"instances" doc:"Every instance the launcher's provider lists for it, an empty list when there is none."`
}

type ObservedInstance struct {
	Name  string `json:"name" doc:"The name from an earlier answer, without the launcher's own prefix."`
	State string `json:"state" enum:"starting,running,exited"`
	Log   string `json:"log,omitempty" doc:"For an exited instance, the end of what its command wrote."`
}

const stateExited = "exited"

var launchDigestPattern = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

// maxRequestBytes holds a log tail for each of several thousand instances.
const maxRequestBytes = 16 << 20

// Desired is one launcher's complete desired state. Instances should exist and be running.
// Ending names instances whose agent's run is over: they are told to stop and are otherwise left
// alone while the agent takes its last turn, neither created nor replaced nor destroyed. An
// instance in neither list should not exist.
type Desired struct {
	Instances []Instance `json:"instances"`
	Ending    []string   `json:"ending"`
}

// reconcile records what the launcher observed and evaluates everything else: which agents are
// the launcher's, which of them still run, which have domain.AgentWindDown left, and which get
// another machine after theirs exited. One agent that cannot be evaluated is logged and left out
// and the rest of the list stands.
func (h *Handler) reconcile(ctx context.Context, launcher string, observed Observed) (*Desired, error) {
	agents, err := h.store.ListLauncherAgents(ctx, launcher, domain.AgentWindDown)
	if err != nil {
		return nil, err
	}
	exitLog := map[string]string{}
	for _, instance := range observed.Instances {
		if instance.State == stateExited {
			exitLog[instance.Name] = instance.Log
		}
	}

	out := &Desired{Instances: []Instance{}, Ending: []string{}}
	for _, agent := range agents {
		log := h.logger.With(zap.String("launcher", launcher), zap.String("platformExpID", agent.PlatformExperimentID), zap.String("agentID", agent.AgentID))
		if agent.Instance == nil {
			log.Error("launcher list holds an agent that is not hosted")
			continue
		}
		if agent.Ending {
			// Named as it was created, whatever config says now: a new name would have its
			// machine destroyed in the middle of the agent's last turn.
			if agent.Instance.Name != "" {
				out.Ending = append(out.Ending, agent.Instance.Name)
			}
			continue
		}
		if tail, exited := exitLog[agent.Instance.Name]; exited && agent.Instance.Name != "" {
			recorded, err := h.store.RecordAgentInstanceExit(ctx, agent.PlatformExperimentID, agent.AgentID, agent.Instance.Name, lastBytes(tail, domain.MaxAgentInstanceLogBytes), domain.MaxAgentInstanceRestarts)
			if err != nil {
				log.Error("record instance exit failed", zap.Error(err))
				continue
			}
			if recorded {
				stopped := agent.Instance.Restarts >= domain.MaxAgentInstanceRestarts
				log.Warn("hosted agent's instance exited", zap.String("instance", agent.Instance.Name), zap.Int("restartsBefore", agent.Instance.Restarts), zap.Bool("stopped", stopped))
				if stopped {
					continue
				}
				agent.Instance.Restarts++
				agent.Instance.Name = ""
			}
		}
		instance, err := instanceFor(agent.AgentSignup, h.image, h.apiURL, h.signingSecret, observed.LaunchDigest)
		if err != nil {
			log.Error("hosted agent's instance cannot be described", zap.Error(err))
			continue
		}
		if instance.Name != agent.Instance.Name {
			if err := h.store.SetAgentInstanceName(ctx, agent.PlatformExperimentID, agent.AgentID, instance.Name); err != nil {
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
			"instances: each {name, image, env} should exist and be running. " +
			"ending: names whose agent's run is over; each is told to stop and is otherwise left to finish, neither created nor replaced nor destroyed. " +
			"Any other instance of the launcher should not exist. An exited instance is recorded on its agent and is never started again by the launcher: " +
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
