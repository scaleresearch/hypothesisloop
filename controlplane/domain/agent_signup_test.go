package domain

import (
	"strings"
	"testing"
)

func TestAgentLaunchValuesAreCheckedAndLeftToTheTypeWhenOmitted(t *testing.T) {
	catalog := testCatalog()
	for _, ok := range []struct{ agentType, model, wantAgentType string }{
		{"", "", "generalist"},
		{"generalist", "anthropic/claude-sonnet-5", "generalist"},
		{"data-search", "anthropic/claude-opus-5[1m]", "data-search"},
		{"architecture-search", "openrouter/vendor/model:v1_2", "architecture-search"},
		{"hyperparameter-search", "", "hyperparameter-search"},
		{"at-custom", "", "at-custom"},
	} {
		choice, err := ResolveAgentLaunch(catalog, AgentRoleExecutor, ok.agentType, ok.model, map[string]any{"batch_size": 64})
		if err != nil || choice.AgentType != ok.wantAgentType || choice.Model != ok.model {
			t.Errorf("agent type %q model %q resolved to %+v, %v", ok.agentType, ok.model, choice, err)
		}
	}
	for _, bad := range [][2]string{
		{"Generalist", ""}, {"../etc", ""}, {"a b", ""}, {"no-such-type", ""}, {"generalist.md", ""}, {"planner", ""}, {"at-archived", ""},
		{"", "model with space"}, {"", "$(id)"}, {"", "-leading"}, {"", "anthropic/" + strings.Repeat("m", 151)}, {"", "a\nb"},
		{"", "claude-sonnet-5"}, {"", "google/gemini-3"}, {"", "anthropic/"}, {"", "anthropic/a b"},
	} {
		if _, err := ResolveAgentLaunch(catalog, AgentRoleExecutor, bad[0], bad[1], nil); err == nil {
			t.Errorf("agent type %q model %q accepted", bad[0], bad[1])
		}
	}
	if choice, err := ResolveAgentLaunch(catalog, AgentRoleBaseline, "", "", nil); err != nil || choice.AgentType != "baseline" {
		t.Errorf("a baseline agent naming no type: %+v, %v, want the built-in baseline", choice, err)
	}
	choice, err := ResolveAgentLaunch(catalog, AgentRoleExecutor, "at-custom", "", nil)
	if err != nil || choice.Hyperparameters != nil || choice.Model != "" {
		t.Errorf("an agent given no model and no hyperparameters: %+v, %v, want both left to its type", choice, err)
	}
}

// An agent's own hyperparameters win; left out, the type's current version gives them. The model
// is the one its ModelChoice resolves.
func TestEffectiveSettingsFollowTheTypeWhereTheAgentNamesNothing(t *testing.T) {
	version := AgentTypeSettings{Model: "type-model", MaxSubagents: 2, Effort: "high", Hyperparameters: map[string]any{"lr": 0.1}}
	if got := EffectiveAgentSettings(version, "type-model", nil); got.Model != "type-model" || got.Hyperparameters["lr"] != 0.1 || got.Effort != "high" {
		t.Errorf("nothing of its own: got = %+v, want the type's", got)
	}
	if got := EffectiveAgentSettings(version, "own", map[string]any{}); got.Model != "own" || len(got.Hyperparameters) != 0 {
		t.Errorf("its own: got = %+v, want them", got)
	}
}

func TestTheInstructionsDigestHoldsTheRulesVersion(t *testing.T) {
	settings := AgentTypeSettings{Model: "m", MaxSubagents: 1, Effort: "high"}
	one, err1 := InstructionsDigest("prompt", settings, 1)
	two, err2 := InstructionsDigest("prompt", settings, 2)
	if err1 != nil || err2 != nil || one == two {
		t.Errorf("rules 1 and 2: %s %v, %s %v, want two digests", one, err1, two, err2)
	}
}

func TestHyperparametersAreBoundedAsJSON(t *testing.T) {
	fits := map[string]any{"note": strings.Repeat("a", MaxHyperparametersBytes-len(`{"note":""}`))}
	if _, err := ResolveAgentLaunch(testCatalog(), AgentRoleExecutor, "", "anthropic/m", fits); err != nil {
		t.Errorf("hyperparameters of exactly the limit refused: %v", err)
	}
	over := map[string]any{"note": strings.Repeat("a", MaxHyperparametersBytes)}
	if _, err := ResolveAgentLaunch(testCatalog(), AgentRoleExecutor, "", "anthropic/m", over); err == nil || !strings.Contains(err.Error(), "the limit is 4096") {
		t.Errorf("hyperparameters above the limit: err = %v", err)
	}
}
