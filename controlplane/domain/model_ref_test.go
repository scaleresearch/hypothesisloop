package domain

import (
	"strings"
	"testing"
)

// The provider is the text before the first slash, and the id all of the rest, slashes of its own
// included.
func TestAModelRefIsItsProviderAndTheIDAfterTheFirstSlash(t *testing.T) {
	for ref, want := range map[string][2]string{
		"anthropic/claude-sonnet-5":          {ModelProviderAnthropic, "claude-sonnet-5"},
		"anthropic/claude-opus-5[1m]":        {ModelProviderAnthropic, "claude-opus-5[1m]"},
		"openrouter/moonshotai/kimi-k2":      {ModelProviderOpenRouter, "moonshotai/kimi-k2"},
		"openrouter/openai/gpt-5-nano":       {ModelProviderOpenRouter, "openai/gpt-5-nano"},
		"openrouter/deepseek/r1:free":        {ModelProviderOpenRouter, "deepseek/r1:free"},
		"openrouter/qwen/qwen3.7-flash-0905": {ModelProviderOpenRouter, "qwen/qwen3.7-flash-0905"},
		"openai/gpt-6-luna":                  {ModelProviderOpenAI, "gpt-6-luna"},
	} {
		got, err := ParseModelRef(ref)
		if err != nil || got.Provider() != want[0] || got.ID() != want[1] || got.String() != ref {
			t.Errorf("%s: got = %q %q %q, %v, want %q %q", ref, got.Provider(), got.ID(), got, err, want[0], want[1])
		}
	}
}

// A bare name and an unknown provider are refused: there is no default provider.
func TestABareNameOrAnUnknownProviderIsNoModelRef(t *testing.T) {
	for _, ref := range []string{
		"", "claude-sonnet-5", "google/gemini-3", "Anthropic/claude-sonnet-5", "anthropic", "anthropic/", "openrouter/", "/claude-sonnet-5",
		"anthropic/claude sonnet", "anthropic/-leading", "anthropic/$(id)", "anthropic/a\nb", "openrouter//double",
		"anthropic/" + strings.Repeat("m", MaxModelRefLength),
	} {
		if got, err := ParseModelRef(ref); err == nil {
			t.Errorf("%q parsed as %q, want it refused", ref, got)
		}
	}
	if _, err := NewModelRef(ModelProviderOpenRouter, "moonshotai/kimi-k2"); err != nil {
		t.Errorf("NewModelRef: %v", err)
	}
}
