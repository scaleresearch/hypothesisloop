package domain

import (
	"strings"
	"testing"
)

func TestAgentLaunchValuesAreResolvedAndSafeAsEnvironment(t *testing.T) {
	const platformModel = "platform-model-1"
	catalog := testCatalog()
	for _, ok := range []struct{ flavor, model, wantFlavor, wantModel string }{
		{"", "", "generalist", platformModel},
		{"generalist", "claude-sonnet-5", "generalist", "claude-sonnet-5"},
		{"data-search", "claude-opus-5[1m]", "data-search", "claude-opus-5[1m]"},
		{"architecture-search", "vendor.model:v1_2", "architecture-search", "vendor.model:v1_2"},
		{"hyperparameter-search", "", "hyperparameter-search", platformModel},
		{"at-custom", "", "at-custom", "custom-model"},
	} {
		choice, err := ResolveAgentLaunch(catalog, AgentRoleResearcher, ok.flavor, ok.model, platformModel, map[string]any{"batch_size": 64})
		if err != nil || choice.AgentType != ok.wantFlavor || choice.Model != ok.wantModel {
			t.Errorf("flavor %q model %q resolved to %+v, %v", ok.flavor, ok.model, choice, err)
		}
	}
	for _, bad := range [][2]string{
		{"Generalist", ""}, {"../etc", ""}, {"a b", ""}, {"no-such-flavor", ""}, {"generalist.md", ""}, {"planner", ""}, {"at-archived", ""},
		{"", "model with space"}, {"", "$(id)"}, {"", "-leading"}, {"", strings.Repeat("m", 129)}, {"", "a\nb"},
	} {
		if _, err := ResolveAgentLaunch(catalog, AgentRoleResearcher, bad[0], bad[1], platformModel, nil); err == nil {
			t.Errorf("flavor %q model %q accepted", bad[0], bad[1])
		}
	}
	if _, err := ResolveAgentLaunch(catalog, AgentRoleResearcher, "", "", "", nil); err == nil {
		t.Error("no model and no platform default accepted")
	}
	if choice, err := ResolveAgentLaunch(catalog, AgentRoleBaseline, "", "", platformModel, nil); err != nil || choice.AgentType != "baseline" {
		t.Errorf("a baseline agent naming no type: %+v, %v, want the built-in baseline", choice, err)
	}
	choice, err := ResolveAgentLaunch(catalog, AgentRoleResearcher, "at-custom", "", platformModel, nil)
	if err != nil || choice.Hyperparameters["lr"] != 0.1 {
		t.Errorf("an agent given no hyperparameters: %+v, %v, want its type's", choice, err)
	}
}

func TestHyperparametersAreBoundedAsJSON(t *testing.T) {
	fits := map[string]any{"note": strings.Repeat("a", MaxHyperparametersBytes-len(`{"note":""}`))}
	if _, err := ResolveAgentLaunch(testCatalog(), AgentRoleResearcher, "", "m", "m", fits); err != nil {
		t.Errorf("hyperparameters of exactly the limit refused: %v", err)
	}
	over := map[string]any{"note": strings.Repeat("a", MaxHyperparametersBytes)}
	if _, err := ResolveAgentLaunch(testCatalog(), AgentRoleResearcher, "", "m", "m", over); err == nil || !strings.Contains(err.Error(), "the limit is 4096") {
		t.Errorf("hyperparameters above the limit: err = %v", err)
	}
}
