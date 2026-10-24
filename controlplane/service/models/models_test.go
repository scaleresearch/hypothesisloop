package models

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

var (
	catalog = domain.ModelCatalog{
		"claude-sonnet-5": {InputUSDPerMTok: 2, OutputUSDPerMTok: 10, CacheWriteUSDPerMTok: 4, CacheReadUSDPerMTok: 0.2},
		"claude-opus-5-5": {InputUSDPerMTok: 4, OutputUSDPerMTok: 20, CacheWriteUSDPerMTok: 8, CacheReadUSDPerMTok: 0.2},
	}
	defaults = domain.ModelDefaults{Agent: "anthropic/claude-sonnet-5", Coordinator: "anthropic/claude-opus-5-5"}
)

// listed is a provider listing models, or failing to, whose calls cost what their answer states.
type listed struct {
	models []domain.ServedModel
	err    error
}

func (l listed) Models(context.Context) ([]domain.ServedModel, error) { return l.models, l.err }

func (listed) Cost(_ string, _ domain.ModelTokens, stated *float64) (float64, error) {
	if stated == nil {
		return 0, errors.New("no cost stated")
	}
	return *stated, nil
}

// served is New over the providers given, each nil one left out.
func served(anthropic, openAI domain.ModelCatalog, openRouter Provider) *Service {
	providers := map[string]Provider{}
	if anthropic != nil {
		providers[domain.ModelProviderAnthropic] = Catalog(anthropic)
	}
	if openAI != nil {
		providers[domain.ModelProviderOpenAI] = Catalog(openAI)
	}
	if openRouter != nil {
		providers[domain.ModelProviderOpenRouter] = openRouter
	}
	return New(providers, defaults)
}

var kimi = domain.ServedModel{ID: "moonshotai/kimi-k2", Name: "MoonshotAI: Kimi K2", ContextLength: 131072, InputUSDPerMTok: 0.6, OutputUSDPerMTok: 2.5}

var chatGPT = domain.ModelCatalog{"gpt-6-luna": {InputUSDPerMTok: 1.25, OutputUSDPerMTok: 10, CacheReadUSDPerMTok: 0.125}}

// The openai models of a ChatGPT subscription are its catalog, listed after anthropic's and
// served as listed.
func TestOpenAIsCatalogIsListedAndServed(t *testing.T) {
	models := served(catalog, chatGPT, nil)
	got, err := models.List(context.Background())
	if err != nil || len(got.Models) != 3 || got.Models[2] != (Model{Model: "openai/gpt-6-luna", Provider: "openai", Name: "gpt-6-luna", InputUSDPerMTok: 1.25, OutputUSDPerMTok: 10}) {
		t.Fatalf("listing = %+v, %v", got, err)
	}
	if err := models.Check(context.Background(), "openai/gpt-6-luna"); err != nil {
		t.Errorf("openai/gpt-6-luna: %v", err)
	}
}

func TestTheListingIsTheAnthropicCatalogThenOpenRoutersModels(t *testing.T) {
	odd := domain.ServedModel{ID: "has a space", Name: "No reference holds it"}
	got, err := served(catalog, nil, listed{models: []domain.ServedModel{kimi, odd}}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := &ModelListing{
		Models: []Model{
			{Model: "anthropic/claude-opus-5-5", Provider: "anthropic", Name: "claude-opus-5-5", InputUSDPerMTok: 4, OutputUSDPerMTok: 20},
			{Model: "anthropic/claude-sonnet-5", Provider: "anthropic", Name: "claude-sonnet-5", InputUSDPerMTok: 2, OutputUSDPerMTok: 10},
			{Model: "openrouter/moonshotai/kimi-k2", Provider: "openrouter", Name: "MoonshotAI: Kimi K2", ContextLength: 131072, InputUSDPerMTok: 0.6, OutputUSDPerMTok: 2.5},
		},
		DefaultModel: "anthropic/claude-sonnet-5", CoordinatorModel: "anthropic/claude-opus-5-5",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listing = %+v, want %+v", got, want)
	}
}

// A provider the platform holds no credential for lists nothing, and OpenRouter not answering is
// an error, never a shorter listing.
func TestAProviderNotServedListsNothingAndAFailedListingIsAnError(t *testing.T) {
	got, err := served(nil, nil, listed{models: []domain.ServedModel{kimi}}).List(context.Background())
	if err != nil || len(got.Models) != 1 || got.Models[0].Provider != domain.ModelProviderOpenRouter {
		t.Errorf("openrouter alone: %+v, %v", got, err)
	}
	got, err = served(catalog, nil, nil).List(context.Background())
	if err != nil || len(got.Models) != 2 || got.Models[1].Model != "anthropic/claude-sonnet-5" {
		t.Errorf("anthropic alone: %+v, %v", got, err)
	}
	if got, err := served(catalog, nil, listed{err: errors.New("unreachable")}).List(context.Background()); err == nil {
		t.Errorf("openrouter unreachable: %+v, want an error", got)
	}
}

// A choice is refused unless the listing holds it: a bare name, an unknown provider, a provider
// not served and a model its provider does not list alike.
func TestAModelTheListingDoesNotHoldIsRefused(t *testing.T) {
	both := served(catalog, nil, listed{models: []domain.ServedModel{kimi}})
	anthropicOnly := served(catalog, nil, nil)
	for _, ok := range []string{"anthropic/claude-sonnet-5", "openrouter/moonshotai/kimi-k2"} {
		if err := both.Check(context.Background(), ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for name, c := range map[string]struct {
		models *Service
		model  string
	}{
		"a bare name":                     {both, "claude-sonnet-5"},
		"an unknown provider":             {both, "google/gemini-3"},
		"openai not served":               {both, "openai/gpt-6-luna"},
		"an openai model not listed":      {served(nil, chatGPT, nil), "openai/gpt-5.4"},
		"an anthropic model not listed":   {both, "anthropic/claude-fable-5-1"},
		"an openrouter model not listed":  {both, "openrouter/moonshotai/kimi-k9"},
		"an openrouter id of another one": {both, "openrouter/kimi-k2"},
		"a provider not served":           {anthropicOnly, "openrouter/moonshotai/kimi-k2"},
		"anthropic not served":            {served(nil, nil, listed{models: []domain.ServedModel{kimi}}), "anthropic/claude-sonnet-5"},
	} {
		err := c.models.Check(context.Background(), c.model)
		var refused *refusal.Refusal
		if !errors.As(err, &refused) || refused.GetStatus() != 422 || refused.Reason != domain.ReasonModelNotServed {
			t.Errorf("%s: %v, want 422 %s", name, err, domain.ReasonModelNotServed)
		}
	}
	err := served(catalog, nil, listed{err: errors.New("unreachable")}).Check(context.Background(), "openrouter/moonshotai/kimi-k2")
	var refused *refusal.Refusal
	if err == nil || errors.As(err, &refused) {
		t.Errorf("openrouter unreachable: %v, want an error that is no refusal", err)
	}
}

// A choice of no model names none, and one the platform does not serve is refused on its field,
// which its message names without repeating the model sent.
func TestAChoiceIsCheckedOnItsField(t *testing.T) {
	models := served(catalog, nil, nil)
	if err := models.CheckChoice(context.Background(), "agents[0].model", ""); err != nil {
		t.Errorf("no model: %v, want nil", err)
	}
	err := models.CheckChoice(context.Background(), "agents[0].model", "anthropic/claude-fable-5-1")
	var refused *refusal.Refusal
	if !errors.As(err, &refused) || refused.Reason != domain.ReasonModelNotServed || len(refused.Problems) != 1 || refused.Problems[0].Field != "agents[0].model" ||
		!strings.HasPrefix(refused.Message, "agents[0].model is not served: ") || refused.Problems[0].Message != refused.Message || strings.Contains(refused.Message, "fable") {
		t.Errorf("a model not served: %v, want model_not_served on agents[0].model, named in its message", err)
	}
}

// A call is priced by its provider: a catalog's at its price, and one whose provider cannot price
// it is an error.
func TestACallIsPricedByItsProvider(t *testing.T) {
	models := served(catalog, nil, listed{models: []domain.ServedModel{kimi}})
	tokens := domain.ModelTokens{Input: 1_000_000, Output: 1_000_000, CacheWrite: 1_000_000, CacheRead: 1_000_000}
	opus, _ := domain.NewModelRef(domain.ModelProviderAnthropic, "claude-opus-5-5")
	if cost, err := models.Cost(opus, tokens, nil); err != nil || cost != 4+20+8+0.2 {
		t.Errorf("anthropic: cost = %v, %v, want 32.2", cost, err)
	}
	k2, _ := domain.NewModelRef(domain.ModelProviderOpenRouter, kimi.ID)
	stated := 0.5
	if cost, err := models.Cost(k2, tokens, &stated); err != nil || cost != 0.5 {
		t.Errorf("openrouter: cost = %v, %v, want the stated 0.5", cost, err)
	}
	if cost, err := models.Cost(k2, tokens, nil); err == nil {
		t.Errorf("openrouter with no cost stated: cost = %v, want an error", cost)
	}
	fable, _ := domain.NewModelRef(domain.ModelProviderAnthropic, "claude-fable-5-1")
	gpt, _ := domain.NewModelRef(domain.ModelProviderOpenAI, "gpt-6-luna")
	for _, unpriced := range []domain.ModelRef{fable, gpt} {
		if cost, err := models.Cost(unpriced, tokens, nil); err == nil {
			t.Errorf("%s: cost = %v, want an error", unpriced, cost)
		}
	}
}
