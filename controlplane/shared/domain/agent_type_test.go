package domain

import (
	"strings"
	"testing"
	"time"
)

func testSettings() AgentTypeSettings {
	return AgentTypeSettings{MaxSubagents: 4, PacingSubagents: 2, Effort: "high"}
}

func testCatalog() AgentTypeCatalog {
	typeOf := func(id string, role AgentRole, settings AgentTypeSettings) CurrentAgentType {
		return CurrentAgentType{AgentType: AgentType{ID: id, Name: id, Role: role, CurrentVersion: 1}, Current: AgentTypeVersion{TypeID: id, Version: 1, Settings: settings}}
	}
	custom := testSettings()
	custom.Model, custom.Hyperparameters = "custom-model", map[string]any{"lr": 0.1}
	archived := typeOf("at-archived", AgentRoleResearcher, testSettings())
	archived.ArchivedAt = &time.Time{}
	return AgentTypeCatalog{
		typeOf("generalist", AgentRoleResearcher, testSettings()),
		typeOf("hyperparameter-search", AgentRoleResearcher, testSettings()),
		typeOf("architecture-search", AgentRoleResearcher, testSettings()),
		typeOf("data-search", AgentRoleResearcher, testSettings()),
		typeOf("baseline", AgentRoleBaseline, testSettings()),
		typeOf("planner", AgentRolePlanner, testSettings()),
		typeOf("coordinator", AgentRoleCoordinator, testSettings()),
		typeOf("at-custom", AgentRoleResearcher, custom),
		archived,
	}
}

func TestAPromptHoldsWhatItsRoleFillsAndNothingElse(t *testing.T) {
	const base = "{{rules}}\n{{hl_commands}}\n"
	for name, c := range map[string]struct {
		role   AgentRole
		prompt string
		want   string
	}{
		"every placeholder of a researcher": {AgentRoleResearcher, base + "{{workdir}} {{agent_id}} {{platform_experiment_id}} {{agent_type}} {{hyperparameters}} {{repository_url}}", ""},
		"a coordinator's own":               {AgentRoleCoordinator, base + "{{workdir}} {{platform_experiment_id}} {{repository_url}}", ""},
		"literal braces":                    {AgentRoleResearcher, base + `{"lr": 1} and {a}`, ""},
		"no rules":                          {AgentRoleResearcher, "{{hl_commands}}", "must hold {{rules}}"},
		"no command listing":                {AgentRolePlanner, "{{rules}}", "must hold {{hl_commands}}"},
		"an unknown placeholder":            {AgentRoleResearcher, base + "{{flavor_brief}}", "{{flavor_brief}}"},
		"a coordinator has no agent id":     {AgentRoleCoordinator, base + "{{agent_id}}", "{{agent_id}}"},
		"a baseline has no hyperparameters": {AgentRoleBaseline, base + "{{hyperparameters}}", "{{hyperparameters}}"},
		"a malformed placeholder":           {AgentRoleResearcher, base + "{{ workdir }}", "outside a placeholder"},
		"an unclosed placeholder":           {AgentRoleResearcher, base + "{{workdir", "outside a placeholder"},
		"python's format braces":            {AgentRoleResearcher, base + "{workdir}}", "outside a placeholder"},
		"empty":                             {AgentRoleResearcher, "  \n", "empty"},
		"not UTF-8":                         {AgentRoleResearcher, base + "\xff", "UTF-8"},
		"too long":                          {AgentRoleResearcher, base + strings.Repeat("a", MaxAgentPromptBytes), "the limit is"},
	} {
		err := ValidatePrompt(c.role, c.prompt)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestComposeFillsTheAssignmentOnceAndLeavesTheHarnessItsOwn(t *testing.T) {
	got, err := ComposePrompt("{{rules}}|{{agent_id}}|{{platform_experiment_id}}|{{agent_type}}|{{repository_url}}|{{hyperparameters}}|{{hl_commands}}|{{workdir}}", Assignment{
		Rules: "\n rules {{workdir}}\n", AgentID: "ag-1", PlatformExperimentID: "pe-1", AgentType: "generalist", RepositoryURL: "https://x/r.git",
		Hyperparameters: map[string]any{"b": "\u00e9", "a": []any{1.0, 0.5, 1e-05, true, nil}},
	})
	want := `rules {{workdir}}|ag-1|pe-1|generalist|https://x/r.git|{"a": [1, 0.5, 1e-05, true, null], "b": "\u00e9"}|{{hl_commands}}|{{workdir}}`
	if err != nil || got != want {
		t.Errorf("got  %q\nwant %q (%v)", got, want, err)
	}
	if got, _ := ComposePrompt("{{hyperparameters}}", Assignment{}); got != "{}" {
		t.Errorf("no hyperparameters: got %q, want {}", got)
	}
}

func TestFloatsAreWrittenAsPythonReprs(t *testing.T) {
	for f, want := range map[float64]string{
		0.001: "0.001", 1e-4: "0.0001", 1.5e-5: "1.5e-05", 1234567.5: "1234567.5", 3: "3", 1e22: "1e+22", 2.5e16: "25000000000000000",
	} {
		if got, err := pythonJSON(f); err != nil || got != want {
			t.Errorf("%v: got %q, want %q", f, got, want)
		}
	}
}

func TestSettingsAreBounded(t *testing.T) {
	for name, c := range map[string]struct {
		change func(*AgentTypeSettings)
		ok     bool
	}{
		"the defaults":              {func(*AgentTypeSettings) {}, true},
		"a model":                   {func(s *AgentTypeSettings) { s.Model = "claude-opus-5[1m]" }, true},
		"a model no agent can run":  {func(s *AgentTypeSettings) { s.Model = "a b" }, false},
		"no subagent":               {func(s *AgentTypeSettings) { s.MaxSubagents = 0 }, false},
		"too many subagents":        {func(s *AgentTypeSettings) { s.MaxSubagents = 11 }, false},
		"pacing above the most":     {func(s *AgentTypeSettings) { s.PacingSubagents = 5 }, false},
		"an effort the SDK has not": {func(s *AgentTypeSettings) { s.Effort = "extreme" }, false},
		"hyperparameters too large": {func(s *AgentTypeSettings) { s.Hyperparameters = map[string]any{"a": strings.Repeat("a", 5000)} }, false},
	} {
		s := testSettings()
		c.change(&s)
		if err := s.Validate(); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok %v", name, err, c.ok)
		}
	}
}

func TestADigestChangesWithPromptOrSettings(t *testing.T) {
	a, _ := AgentTypeDigest("p", testSettings())
	b, _ := AgentTypeDigest("q", testSettings())
	s := testSettings()
	s.MaxSubagents = 5
	c, _ := AgentTypeDigest("p", s)
	again, _ := AgentTypeDigest("p", testSettings())
	if a == b || a == c || a != again {
		t.Errorf("digests %s %s %s %s: want each change to move it and nothing else", a, b, c, again)
	}
}

// A built-in id can never be one an org's type is given, which starts with at-.
func TestABuiltinIDStaysOutOfTheNamespaceOfOrgTypes(t *testing.T) {
	for _, id := range []string{"generalist", "hyperparameter-search", "a1"} {
		if err := ValidateBuiltinAgentTypeID(id); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
	org, err := NewID(AgentTypeIDPrefix)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{org, "at-tuner", "Generalist", "1st", "a b", "", strings.Repeat("a", 41)} {
		if err := ValidateBuiltinAgentTypeID(id); err == nil {
			t.Errorf("%q accepted as a built-in id", id)
		}
	}
}

func TestAnOrgMakesResearcherAndBaselineTypesOnly(t *testing.T) {
	for _, role := range AgentRoles {
		err := CheckOrgAgentRole(role)
		if want := role == AgentRoleResearcher || role == AgentRoleBaseline; (err == nil) != want {
			t.Errorf("%s: got = %v, want allowed %v", role, err, want)
		}
	}
}

func TestTheModelAnAgentRunsOnIsItsOwnElseItsTypesElseTheDefault(t *testing.T) {
	catalog := testCatalog()
	for _, c := range []struct{ agentType, model, want string }{
		{"at-custom", "own", "own"},
		{"at-custom", "", "custom-model"},
		{"generalist", "", "platform"},
	} {
		if got := ResolveModel(catalog, c.agentType, c.model, "platform"); got != c.want {
			t.Errorf("%s %q: got = %s, want %s", c.agentType, c.model, got, c.want)
		}
	}
}
