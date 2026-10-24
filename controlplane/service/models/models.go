package models

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/openrouter"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// OpenRouter lists the models OpenRouter serves. *openrouter.Catalog satisfies it.
type OpenRouter interface {
	Models(ctx context.Context) ([]openrouter.Model, error)
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

// Listing is every model the platform serves, and the ones it runs an agent and a coordinator on
// when they name none.
type Listing struct {
	Models           []Model `json:"models"`
	DefaultModel     string  `json:"default_model" doc:"The model an executor runs on when neither it nor its agent type names one."`
	CoordinatorModel string  `json:"coordinator_model" doc:"The model a sandboxed coordinator runs on when neither its platform experiment nor its agent type names one, and a researcher when neither it, its agent type nor its platform experiment's coordinator_model does."`
}

// Service lists the models of the providers the platform serves and refuses a choice of any other.
type Service struct {
	anthropic  domain.ModelCatalog
	openRouter OpenRouter
	openAI     domain.ModelCatalog
	defaults   domain.ModelDefaults
}

// New lists anthropic's catalog and openRouter's models, each nil when the platform does not serve
// that provider.
func New(anthropic domain.ModelCatalog, openRouter OpenRouter, defaults domain.ModelDefaults) *Service {
	return &Service{anthropic: anthropic, openRouter: openRouter, defaults: defaults}
}

// WithOpenAI lists and serves openAI's catalog too, the openai models of a ChatGPT subscription.
func (s *Service) WithOpenAI(openAI domain.ModelCatalog) *Service {
	s.openAI = openAI
	return s
}

// List is every model the platform serves: anthropic's catalog, then openai's, then OpenRouter's
// models that take tools at a price it states, as OpenRouter lists them now.
func (s *Service) List(ctx context.Context) (*Listing, error) {
	listing := &Listing{Models: []Model{}, DefaultModel: s.defaults.Agent, CoordinatorModel: s.defaults.Coordinator}
	for _, priced := range []struct {
		provider string
		catalog  domain.ModelCatalog
	}{{domain.ModelProviderAnthropic, s.anthropic}, {domain.ModelProviderOpenAI, s.openAI}} {
		for _, id := range priced.catalog.Names() {
			price := priced.catalog[id]
			listing.Models = append(listing.Models, Model{
				Model: priced.provider + "/" + id, Provider: priced.provider, Name: id,
				InputUSDPerMTok: price.InputUSDPerMTok, OutputUSDPerMTok: price.OutputUSDPerMTok,
			})
		}
	}
	if s.openRouter == nil {
		return listing, nil
	}
	served, err := s.openRouter.Models(ctx)
	if err != nil {
		return nil, fmt.Errorf("models.List: %w", err)
	}
	for _, m := range served {
		ref, err := domain.NewModelRef(domain.ModelProviderOpenRouter, m.ID)
		if err != nil {
			// No agent could name it.
			continue
		}
		listing.Models = append(listing.Models, Model{
			Model: ref.String(), Provider: domain.ModelProviderOpenRouter, Name: m.Name, ContextLength: m.ContextLength,
			InputUSDPerMTok: m.InputUSDPerMTok, OutputUSDPerMTok: m.OutputUSDPerMTok,
		})
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
	switch ref.Provider() {
	case domain.ModelProviderAnthropic, domain.ModelProviderOpenAI:
		catalog := map[string]domain.ModelCatalog{domain.ModelProviderAnthropic: s.anthropic, domain.ModelProviderOpenAI: s.openAI}[ref.Provider()]
		if catalog == nil {
			return unserved(ref)
		}
		if _, ok := catalog[ref.ID()]; !ok {
			return refusal.Invalid(domain.ReasonModelNotServed, "the model is not one GET /models lists: the "+ref.Provider()+" models served are "+strings.Join(catalog.Names(), ", "))
		}
	case domain.ModelProviderOpenRouter:
		if s.openRouter == nil {
			return unserved(ref)
		}
		served, err := s.openRouter.Models(ctx)
		if err != nil {
			return fmt.Errorf("models.Check: %w", err)
		}
		if !slices.ContainsFunc(served, func(m openrouter.Model) bool { return m.ID == ref.ID() }) {
			return refusal.Invalid(domain.ReasonModelNotServed, "the model is not one GET /models lists: OpenRouter serves no model of that id that takes tools")
		}
	}
	return nil
}

func unserved(ref domain.ModelRef) error {
	return refusal.Invalid(domain.ReasonModelNotServed, "the model is not one GET /models lists: this platform serves no "+ref.Provider()+" model")
}
