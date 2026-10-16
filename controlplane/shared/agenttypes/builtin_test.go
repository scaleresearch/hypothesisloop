package agenttypes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// The golden prompts are what the experimentator composed itself before its prompts moved into the
// database (testdata/generate.py): served with the same assignment, a built-in says the same.
func TestEachBuiltinComposesToWhatTheHarnessComposed(t *testing.T) {
	raw, err := os.ReadFile("testdata/assignment.json")
	if err != nil {
		t.Fatal(err)
	}
	var a struct {
		Rules                string         `json:"rules"`
		AgentID              string         `json:"agent_id"`
		PlatformExperimentID string         `json:"platform_experiment_id"`
		RepositoryURL        string         `json:"repository_url"`
		Hyperparameters      map[string]any `json:"hyperparameters"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	all, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range all {
		want, err := os.ReadFile(filepath.Join("testdata", "golden", b.ID+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := domain.ComposePrompt(b.Prompt, domain.Assignment{
			Rules: a.Rules, AgentID: a.AgentID, PlatformExperimentID: a.PlatformExperimentID, AgentType: b.ID,
			RepositoryURL: a.RepositoryURL, Hyperparameters: a.Hyperparameters,
		})
		if err != nil || got != string(want) {
			t.Errorf("%s composes differently from the harness (%v):\n%s", b.ID, err, firstDifference(got, string(want)))
		}
	}
}

func firstDifference(got, want string) string {
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			return "got  ..." + got[max(0, i-40):min(len(got), i+80)] + "\nwant ..." + want[max(0, i-40):min(len(want), i+80)]
		}
	}
	return "one is a prefix of the other"
}

// Until the harness reads its prompt from the platform, a researcher of a built-in type is started
// with the flavor brief of the same name, which the image must hold.
func TestEveryBuiltinResearcherIsABriefTheAgentImageHolds(t *testing.T) {
	briefs, err := filepath.Glob("../../../agents/experimentator/src/hypothesisloop_agent/prompts/flavors/*.md")
	if err != nil || len(briefs) == 0 {
		t.Fatalf("no flavor briefs found: %v", err)
	}
	var held, researchers []string
	for _, brief := range briefs {
		held = append(held, strings.TrimSuffix(filepath.Base(brief), ".md"))
	}
	for _, t := range Catalog() {
		if t.Role == domain.AgentRoleResearcher {
			researchers = append(researchers, t.ID)
		}
	}
	slices.Sort(held)
	slices.Sort(researchers)
	if !slices.Equal(held, researchers) {
		t.Fatalf("built-in researchers %v, the image holds %v", researchers, held)
	}
	if !slices.Contains(researchers, domain.DefaultAgentFlavor) {
		t.Fatalf("the default type %q is no built-in researcher", domain.DefaultAgentFlavor)
	}
	for _, role := range domain.AgentRoles {
		if _, err := domain.LookupAgentType(Catalog(), domain.DefaultAgentType(role), role); err != nil {
			t.Errorf("role %s has no built-in default: %v", role, err)
		}
	}
}
