package domain

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAgentLaunchValuesAreResolvedAndSafeAsEnvironment(t *testing.T) {
	const platformModel = "platform-model-1"
	for _, ok := range []struct{ flavor, model, wantFlavor, wantModel string }{
		{"", "", "generalist", platformModel},
		{"generalist", "claude-sonnet-5", "generalist", "claude-sonnet-5"},
		{"data-search", "claude-opus-5[1m]", "data-search", "claude-opus-5[1m]"},
		{"architecture-search", "vendor.model:v1_2", "architecture-search", "vendor.model:v1_2"},
		{"hyperparameter-search", "", "hyperparameter-search", platformModel},
	} {
		flavor, model, err := ResolveAgentLaunch(ok.flavor, ok.model, platformModel, map[string]any{"batch_size": 64})
		if err != nil || flavor != ok.wantFlavor || model != ok.wantModel {
			t.Errorf("flavor %q model %q resolved to %q %q, %v", ok.flavor, ok.model, flavor, model, err)
		}
	}
	for _, bad := range [][2]string{
		{"Generalist", ""}, {"../etc", ""}, {"a b", ""}, {"no-such-flavor", ""}, {"generalist.md", ""},
		{"", "model with space"}, {"", "$(id)"}, {"", "-leading"}, {"", strings.Repeat("m", 129)}, {"", "a\nb"},
	} {
		if _, _, err := ResolveAgentLaunch(bad[0], bad[1], platformModel, nil); err == nil {
			t.Errorf("flavor %q model %q accepted", bad[0], bad[1])
		}
	}
	if _, _, err := ResolveAgentLaunch("", "", "", nil); err == nil {
		t.Error("no model and no platform default accepted")
	}
}

func TestHyperparametersAreBoundedAsJSON(t *testing.T) {
	fits := map[string]any{"note": strings.Repeat("a", MaxHyperparametersBytes-len(`{"note":""}`))}
	if _, _, err := ResolveAgentLaunch("", "m", "m", fits); err != nil {
		t.Errorf("hyperparameters of exactly the limit refused: %v", err)
	}
	over := map[string]any{"note": strings.Repeat("a", MaxHyperparametersBytes)}
	if _, _, err := ResolveAgentLaunch("", "m", "m", over); err == nil || !strings.Contains(err.Error(), "the limit is 4096") {
		t.Errorf("hyperparameters above the limit: err = %v", err)
	}
}

// The agent image reads prompts/flavors/<flavor>.md and exits when the file is missing, so the
// set an agent may join with is the set of briefs the image holds.
func TestTheKnownFlavorsAreTheBriefsTheAgentImageHolds(t *testing.T) {
	briefs, err := filepath.Glob("../../../agents/experimentator/src/hypothesisloop_agent/prompts/flavors/*.md")
	if err != nil || len(briefs) == 0 {
		t.Fatalf("no flavor briefs found: %v", err)
	}
	held := []string{}
	for _, brief := range briefs {
		held = append(held, strings.TrimSuffix(filepath.Base(brief), ".md"))
	}
	known := slices.Clone(AgentFlavors)
	slices.Sort(known)
	slices.Sort(held)
	if !slices.Equal(known, held) {
		t.Fatalf("AgentFlavors = %v, the image holds %v", known, held)
	}
	if !slices.Contains(AgentFlavors, DefaultAgentFlavor) {
		t.Fatalf("the default flavor %q is not a known one", DefaultAgentFlavor)
	}
}
