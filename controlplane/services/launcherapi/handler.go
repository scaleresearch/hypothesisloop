// Package launcherapi is the one endpoint an agent-launcher calls:
//
//	POST /internal/launchers/{name}/reconcile — the agent instances that launcher should keep
//
// A launcher holds a provider account and nothing else. Which agents exist, in which platform
// experiment, with what environment, and until when, is decided here and handed over as a
// finished list. The launcher is given no rule to evaluate, so every decision in the list can
// be taken back by the next answer.
package launcherapi

import (
	"context"
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

// Desired is one launcher's complete desired state. Instances should exist and be running.
// Ending names instances that are no longer wanted and are left alone while their agent takes
// its last turn: they are not created, replaced or destroyed. An instance in neither list should
// not exist.
type Desired struct {
	Instances []Instance `json:"instances"`
	Ending    []string   `json:"ending"`
}

// desired evaluates everything: which agents are the launcher's, which of them still run, and
// which have domain.AgentWindDown left.
func (h *Handler) desired(ctx context.Context, launcher string) (*Desired, error) {
	agents, err := h.store.ListLauncherAgents(ctx, launcher, domain.AgentWindDown)
	if err != nil {
		return nil, err
	}
	out := &Desired{Instances: []Instance{}, Ending: []string{}}
	for _, agent := range agents {
		instance, err := instanceFor(agent.AgentSignup, h.image, h.apiURL, h.signingSecret)
		if err != nil {
			return nil, err
		}
		if agent.Ending {
			out.Ending = append(out.Ending, instance.Name)
			continue
		}
		out.Instances = append(out.Instances, instance)
	}
	return out, nil
}

// RegisterHuma registers the launcher-facing operation on doc. Paths are relative to the
// /internal/launchers mount.
func RegisterHuma(doc *apidocs.Doc, h *Handler) {
	apidocs.Register(doc, apidocs.AudienceLauncher, huma.Operation{
		OperationID: "launcher-reconcile", Method: "POST", Path: "/{name}/reconcile",
		Summary: "The agent instances a launcher should keep", Tags: []string{"agent-launcher"},
		Description: "Returns this launcher's complete desired state. instances: each {name, image, env} should exist and be running. " +
			"ending: names that are no longer wanted and are left to finish, neither created nor replaced nor destroyed. " +
			"Any other instance of the launcher should not exist. The answer is a list and carries no rule; ask again for the next one.",
	}, func(ctx context.Context, in *struct {
		Name string `path:"name"`
	}) (*struct{ Body *Desired }, error) {
		desired, err := h.desired(ctx, in.Name)
		if err != nil {
			// Never the answer itself: it holds every agent's token.
			h.logger.Error("launcher reconcile failed", zap.String("launcher", in.Name), zap.Error(err))
			return nil, huma.Error500InternalServerError(auth.InternalError)
		}
		return &struct{ Body *Desired }{Body: desired}, nil
	})
}
