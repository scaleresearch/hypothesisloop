package models

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// Provider is one model provider the platform serves.
type Provider interface {
	// Models are the models it serves now, a call naming any other refused.
	Models(ctx context.Context) ([]domain.ServedModel, error)
	// Cost is what a call on model cost that counted tokens, the cost its answer states in stated,
	// nil when it states none. A call that cannot be priced is an error.
	Cost(model string, tokens domain.ModelTokens, stated *float64) (float64, error)
}

// Catalog is a provider whose models and prices are the platform's catalog of them.
type Catalog domain.ModelCatalog

func (c Catalog) Models(context.Context) ([]domain.ServedModel, error) {
	served := make([]domain.ServedModel, 0, len(c))
	for _, id := range domain.ModelCatalog(c).Names() {
		served = append(served, domain.ServedModel{ID: id, Name: id, InputUSDPerMTok: c[id].InputUSDPerMTok, OutputUSDPerMTok: c[id].OutputUSDPerMTok})
	}
	return served, nil
}

func (c Catalog) Cost(model string, tokens domain.ModelTokens, _ *float64) (float64, error) {
	price, ok := c[model]
	if !ok {
		return 0, fmt.Errorf("the catalog prices no model %s", model)
	}
	return price.CostUSD(tokens), nil
}

// Model is one model an agent type, an agent or a coordinator may run on.
type Model struct {
	Model            string  `json:"model" doc:"Its reference, <provider>/<model id>: what an agent type, an agent and a coordinator name as their model."`
	Provider         string  `json:"provider" enum:"anthropic,openrouter,openai"`
	Name             string  `json:"name" doc:"What its provider calls it: the model id for an anthropic or openai model."`
	ContextLength    int     `json:"context_length" doc:"The most tokens a call to it holds, 0 when unknown."`
	InputUSDPerMTok  float64 `json:"input_usd_per_mtok" doc:"What its provider charges for input, in US dollars per million tokens."`
	OutputUSDPerMTok float64 `json:"output_usd_per_mtok" doc:"What its provider charges for output, in US dollars per million tokens."`
}

// ModelListing is every model the platform serves, and the ones it runs an agent and a coordinator on
// when they name none.
type ModelListing struct {
	Models           []Model `json:"models"`
	DefaultModel     string  `json:"default_model" doc:"The model an executor runs on when neither it nor its agent type names one."`
	CoordinatorModel string  `json:"coordinator_model" doc:"The model a sandboxed coordinator runs on when neither its platform experiment nor its agent type names one, and a researcher when neither it, its agent type nor its platform experiment's coordinator_model does."`
}

// Service lists the models of the providers the platform serves, refuses a choice of any other and
// prices the calls made on them.
type Service struct {
	providers map[string]Provider
	defaults  domain.ModelDefaults
}

// New serves providers, by provider name: those the platform holds a credential for.
func New(providers map[string]Provider, defaults domain.ModelDefaults) *Service {
	return &Service{providers: providers, defaults: defaults}
}

// List is every model the platform serves, by provider, as each provider lists them now.
func (s *Service) List(ctx context.Context) (*ModelListing, error) {
	listing := &ModelListing{Models: []Model{}, DefaultModel: s.defaults.Agent, CoordinatorModel: s.defaults.Coordinator}
	for _, provider := range slices.Sorted(maps.Keys(s.providers)) {
		served, err := s.providers[provider].Models(ctx)
		if err != nil {
			return nil, fmt.Errorf("models.List: %s: %w", provider, err)
		}
		for _, m := range served {
			ref, err := domain.NewModelRef(provider, m.ID)
			if err != nil {
				// No agent could name it.
				continue
			}
			listing.Models = append(listing.Models, Model{
				Model: ref.String(), Provider: provider, Name: m.Name, ContextLength: m.ContextLength,
				InputUSDPerMTok: m.InputUSDPerMTok, OutputUSDPerMTok: m.OutputUSDPerMTok,
			})
		}
	}
	return listing, nil
}

// Check refuses with 422 model_not_served a model that is no reference, of a provider the platform
// does not serve, or that its provider does not list.
func (s *Service) Check(ctx context.Context, model string) error {
	ref, err := domain.ParseModelRef(model)
	if err != nil {
		return refusal.Invalid(domain.ReasonModelNotServed, err.Error())
	}
	provider, ok := s.providers[ref.Provider()]
	if !ok {
		return refusal.Invalid(domain.ReasonModelNotServed, "the model is not one GET /models lists: this platform serves no "+ref.Provider()+" model")
	}
	served, err := provider.Models(ctx)
	if err != nil {
		return fmt.Errorf("models.Check: %s: %w", ref.Provider(), err)
	}
	if !slices.ContainsFunc(served, func(m domain.ServedModel) bool { return m.ID == ref.ID() }) {
		return refusal.Invalid(domain.ReasonModelNotServed, "the model is not one GET /models lists: "+ref.Provider()+" serves no model of that id that this platform runs an agent on")
	}
	return nil
}

// CheckChoice refuses on field a model a choice names that the platform does not serve, as Check
// does, the message naming the field. An empty one names none: the agent type's or the
// platform's default.
func (s *Service) CheckChoice(ctx context.Context, field, model string) error {
	if model == "" {
		return nil
	}
	err := s.Check(ctx, model)
	var refused *refusal.Refusal
	if errors.As(err, &refused) {
		return refusal.Invalid(refused.Reason, field+" is not served: "+refused.Message).On(field)
	}
	return err
}

// Cost is what a call on model cost that counted tokens, priced by its provider.
func (s *Service) Cost(model domain.ModelRef, tokens domain.ModelTokens, stated *float64) (float64, error) {
	provider, ok := s.providers[model.Provider()]
	if !ok {
		return 0, fmt.Errorf("models.Cost: the platform serves no %s model", model.Provider())
	}
	cost, err := provider.Cost(model.ID(), tokens, stated)
	if err != nil {
		return 0, fmt.Errorf("models.Cost: %s: %w", model, err)
	}
	return cost, nil
}
