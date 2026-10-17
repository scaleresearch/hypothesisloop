package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
)

func instructionsCall(t *testing.T, caller auth.Principal, path string) (int, platformexperiments.Instructions) {
	t.Helper()
	r := authtest.Router(authtest.As(caller))
	doc := NewDoc(r, "test", "1.0.0", "", auth.NewAccess(sessionPlacement), zap.NewNop())
	registerInstructions(doc, platformexperiments.NewComposer(platformexperimentstest.PinnedInstructions{}, platformexperimentstest.Defaults, zap.NewNop()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest("GET", path, nil)))
	var out platformexperiments.Instructions
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, out
}

const (
	agentInstructions       = "/platform-experiments/pe-1/agents/ag-1/instructions"
	coordinatorInstructions = "/platform-experiments/pe-1/coordinator/instructions"
)

func TestAnAgentReadsItsInstructionsWithTheAssignmentFilledAndTheMachinesLeft(t *testing.T) {
	code, got := instructionsCall(t, agentOne, agentInstructions)
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d", agentInstructions, code)
	}
	for _, want := range []string{"agent_id: ag-1", "platform_experiment_id: pe-1", "agent_type: generalist", `hyperparameters: {"lr": 0.001}`,
		"## platform rules (read first)", "{{hl_commands}}", "{{workdir}}, the directory every command starts in, holds the clone of https://github.com/acme/vision.git"} {
		if !strings.Contains(got.Prompt, want) {
			t.Errorf("the prompt holds no %q", want)
		}
	}
	if strings.Contains(got.Prompt, "{{rules}}") || strings.Contains(got.Prompt, "{{agent_id}}") {
		t.Error("the prompt still holds a placeholder the platform fills")
	}
	if got.AgentType != "generalist" || got.Version != 1 || got.RulesVersion != 1 || got.Role != domain.AgentRoleResearcher || got.Settings.Model != "model-1" ||
		got.Settings.Hyperparameters["lr"] != 0.001 || got.Settings.MaxSubagents != 4 || got.Digest == "" || strings.Join(got.Placeholders, ",") != "hl_commands,workdir" {
		t.Errorf("instructions: got = %+v", got)
	}
	code, coordinatorGot := instructionsCall(t, coordinator, coordinatorInstructions)
	if code != http.StatusOK || coordinatorGot.Role != domain.AgentRoleCoordinator || coordinatorGot.Settings.Model != "model-c" ||
		!strings.Contains(coordinatorGot.Prompt, "repository: https://github.com/acme/vision.git") {
		t.Errorf("the coordinator's: %d %+v", code, coordinatorGot)
	}
}

// An agent that names no model or hyperparameters runs its type's, read when it is served, and
// the platform's default model for a type that names none.
func TestAnAgentNamingNothingRunsItsTypesSettings(t *testing.T) {
	code, got := instructionsCall(t, auth.Principal{Kind: auth.KindStaff}, "/platform-experiments/pe-1/agents/ag-2/instructions")
	if code != http.StatusOK || got.Settings.Model != platformexperimentstest.Defaults.Agent || len(got.Settings.Hyperparameters) != 0 || !strings.Contains(got.Prompt, "hyperparameters: {}") {
		t.Errorf("ag-2: %d %+v, want the platform's default model and the type's empty hyperparameters", code, got)
	}
}

func TestInstructionsAreReadByTheirOwnerAndTheOrgAlone(t *testing.T) {
	inOrg := []string{"org-a"}
	for _, c := range []struct {
		name               string
		caller             auth.Principal
		agents, coordinate int
	}{
		{"the agent", agentOne, http.StatusOK, http.StatusForbidden},
		{"another agent of the experiment", agentTwo, http.StatusForbidden, http.StatusForbidden},
		{"the coordinator", coordinator, http.StatusForbidden, http.StatusOK},
		{"a member of the org", auth.Principal{Kind: auth.KindUser, UserID: "u-1", OrgIDs: inOrg}, http.StatusOK, http.StatusOK},
		{"a person of another org", auth.Principal{Kind: auth.KindUser, UserID: "u-2", OrgIDs: []string{"org-b"}}, http.StatusNotFound, http.StatusNotFound},
		{"an agent of another experiment", auth.Principal{Kind: auth.KindAgent, OrgIDs: inOrg, ProjectID: "prj-a", PlatformExperimentID: "pe-2", AgentID: "ag-1"}, http.StatusNotFound, http.StatusForbidden},
	} {
		if code, _ := instructionsCall(t, c.caller, agentInstructions); code != c.agents {
			t.Errorf("%s reading the agent's: %d, want %d", c.name, code, c.agents)
		}
		if code, _ := instructionsCall(t, c.caller, coordinatorInstructions); code != c.coordinate {
			t.Errorf("%s reading the coordinator's: %d, want %d", c.name, code, c.coordinate)
		}
	}
	if code, _ := instructionsCall(t, auth.Principal{Kind: auth.KindStaff}, "/platform-experiments/pe-1/agents/ag-nobody/instructions"); code != http.StatusNotFound {
		t.Errorf("an agent not in the experiment: %d, want 404", code)
	}
}
