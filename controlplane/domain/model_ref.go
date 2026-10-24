package domain

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The providers whose models the platform serves, each by the name a model reference starts with.
const (
	ModelProviderAnthropic  = "anthropic"
	ModelProviderOpenRouter = "openrouter"
	ModelProviderOpenAI     = "openai"
)

// ModelProviders is every provider a model reference may name.
var ModelProviders = []string{ModelProviderAnthropic, ModelProviderOpenRouter, ModelProviderOpenAI}

// MaxModelRefLength bounds a model reference.
const MaxModelRefLength = 160

// modelIDPattern is a model id as a provider names it: OpenRouter's hold a slash of their own.
var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]~-]*$`)

// ModelRef is a model the platform stores, serves or resolves: <provider>/<model id>, the provider
// the text before the first slash and the id the model's name at that provider.
type ModelRef struct {
	provider, id string
}

// ParseModelRef refuses a bare model name, an unknown provider and an id no provider names.
func ParseModelRef(s string) (ModelRef, error) {
	provider, id, found := strings.Cut(s, "/")
	if !found || !slices.Contains(ModelProviders, provider) {
		return ModelRef{}, fmt.Errorf("a model is named <provider>/<model id>, the provider one of %s, such as %s/claude-sonnet-5",
			strings.Join(ModelProviders, ", "), ModelProviderAnthropic)
	}
	if len(s) > MaxModelRefLength || !modelIDPattern.MatchString(id) {
		return ModelRef{}, fmt.Errorf("a model reference is at most %d characters, its model id letters, digits and . _ : / ~ - [ ]", MaxModelRefLength)
	}
	return ModelRef{provider: provider, id: id}, nil
}

// NewModelRef is the reference to the model of id at provider.
func NewModelRef(provider, id string) (ModelRef, error) {
	return ParseModelRef(provider + "/" + id)
}

// Provider is the provider that serves the model.
func (r ModelRef) Provider() string { return r.provider }

// ID is the model's name at its provider, the one a call to the provider states.
func (r ModelRef) ID() string { return r.id }

func (r ModelRef) String() string { return r.provider + "/" + r.id }
