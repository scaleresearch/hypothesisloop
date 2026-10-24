package models

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/openrouter"
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

// listed is OpenRouter's listing as the catalog client reads it, or its failure.
type listed struct {
	models []openrouter.Model
	err    error
}

func (l listed) Models(context.Context) ([]openrouter.Model, error) { return l.models, l.err }

var kimi = openrouter.Model{ID: "moonshotai/kimi-k2", Name: "MoonshotAI: Kimi K2", ContextLength: 131072, InputUSDPerMTok: 0.6, OutputUSDPerMTok: 2.5}

var chatGPT = domain.ModelCatalog{"gpt-6-luna": {InputUSDPerMTok: 1.25, OutputUSDPerMTok: 10, CacheWriteUSDPerMTok: 1.25, CacheReadUSDPerMTok: 0.125}}

// The openai models of a ChatGPT subscription are its catalog, listed after anthropic's and
// served as listed.
func TestOpenAIsCatalogIsListedAndServed(t *testing.T) {
	models := New(catalog, nil, defaults).WithOpenAI(chatGPT)
	got, err := models.List(context.Background())
	if err != nil || len(got.Models) != 3 || got.Models[2] != (Model{Model: "openai/gpt-6-luna", Provider: "openai", Name: "gpt-6-luna", InputUSDPerMTok: 1.25, OutputUSDPerMTok: 10}) {
		t.Fatalf("listing = %+v, %v", got, err)
	}
	if err := models.Check(context.Background(), "openai/gpt-6-luna"); err != nil {
		t.Errorf("openai/gpt-6-luna: %v", err)
	}
}

func TestTheListingIsTheAnthropicCatalogThenOpenRoutersModels(t *testing.T) {
	odd := openrouter.Model{ID: "has a space", Name: "No reference holds it"}
	got, err := New(catalog, listed{models: []openrouter.Model{kimi, odd}}, defaults).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := &Listing{
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
	got, err := New(nil, listed{models: []openrouter.Model{kimi}}, defaults).List(context.Background())
	if err != nil || len(got.Models) != 1 || got.Models[0].Provider != domain.ModelProviderOpenRouter {
		t.Errorf("openrouter alone: %+v, %v", got, err)
	}
	got, err = New(catalog, nil, defaults).List(context.Background())
	if err != nil || len(got.Models) != 2 || got.Models[1].Model != "anthropic/claude-sonnet-5" {
		t.Errorf("anthropic alone: %+v, %v", got, err)
	}
	if got, err := New(catalog, listed{err: errors.New("unreachable")}, defaults).List(context.Background()); err == nil {
		t.Errorf("openrouter unreachable: %+v, want an error", got)
	}
}

// A choice is refused unless the listing holds it: a bare name, an unknown provider, a provider
// not served and a model its provider does not list alike.
func TestAModelTheListingDoesNotHoldIsRefused(t *testing.T) {
	served := New(catalog, listed{models: []openrouter.Model{kimi}}, defaults)
	anthropicOnly := New(catalog, nil, defaults)
	for _, ok := range []string{"anthropic/claude-sonnet-5", "openrouter/moonshotai/kimi-k2"} {
		if err := served.Check(context.Background(), ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for name, c := range map[string]struct {
		models *Service
		model  string
	}{
		"a bare name":                     {served, "claude-sonnet-5"},
		"an unknown provider":             {served, "google/gemini-3"},
		"openai not served":               {served, "openai/gpt-6-luna"},
		"an openai model not listed":      {New(nil, nil, defaults).WithOpenAI(chatGPT), "openai/gpt-5.4"},
		"an anthropic model not listed":   {served, "anthropic/claude-fable-5-1"},
		"an openrouter model not listed":  {served, "openrouter/moonshotai/kimi-k9"},
		"an openrouter id of another one": {served, "openrouter/kimi-k2"},
		"a provider not served":           {anthropicOnly, "openrouter/moonshotai/kimi-k2"},
		"anthropic not served":            {New(nil, listed{models: []openrouter.Model{kimi}}, defaults), "anthropic/claude-sonnet-5"},
	} {
		err := c.models.Check(context.Background(), c.model)
		var refused *refusal.Refusal
		if !errors.As(err, &refused) || refused.GetStatus() != 422 || refused.Reason != domain.ReasonModelNotServed {
			t.Errorf("%s: %v, want 422 %s", name, err, domain.ReasonModelNotServed)
		}
	}
	err := New(catalog, listed{err: errors.New("unreachable")}, defaults).Check(context.Background(), "openrouter/moonshotai/kimi-k2")
	var refused *refusal.Refusal
	if err == nil || errors.As(err, &refused) {
		t.Errorf("openrouter unreachable: %v, want an error that is no refusal", err)
	}
}
