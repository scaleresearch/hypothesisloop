package agenttypes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// The golden prompts are what the harness composed itself before its prompts moved to the
// platform: served with the same assignment, a built-in says the same until one is edited on
// purpose, golden file and all.
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

func TestEveryRoleHasABuiltinDefault(t *testing.T) {
	var researchers []string
	for _, t := range Catalog() {
		if t.Role == domain.AgentRoleResearcher {
			researchers = append(researchers, t.ID)
		}
	}
	if !slices.Contains(researchers, domain.DefaultAgentType) {
		t.Fatalf("the default type %q is no built-in researcher", domain.DefaultAgentType)
	}
	for _, role := range domain.AgentRoles {
		if _, err := domain.LookupAgentType(Catalog(), domain.AgentTypeOfRole(role), role); err != nil {
			t.Errorf("role %s has no built-in default: %v", role, err)
		}
	}
}

// What live runs taught the built-in prompts, each said where the agent reads it. These were the
// harness's checks of its own prompts before the prompts moved to the platform.
func TestTheBuiltinPromptsKeepWhatRunsTaught(t *testing.T) {
	prompts := map[string]string{}
	for _, b := range mustBuiltins(t) {
		prompts[b.ID] = strings.Join(strings.Fields(b.Prompt), " ")
	}
	for id, said := range map[string][]string{
		// A researcher gated every job on a green local suite and an offline check, and submitted
		// one job in an hour with most of its capacity free.
		domain.DefaultAgentType: {
			"no offline estimate, local measurement, audit or test suite stands in for one",
			"a local build that runs is the only check before submitting",
			"A subagent submits as soon as its code builds",
			"its own jobs (`hl job submit`",
			"goes to every subagent it concerns",
			"keep several independent jobs in flight",
			// Subagents were 87% of a live run's spend: a lead resumed one with new work until it
			// re-read 457K on every call, and pinged live ones for progress about 25 times.
			"goes to a fresh subagent with its own brief, never a resumed one",
			"never a request for its progress",
			"report once, at its end",
			// A researcher tested a person's idea, gained 12% and filed no finding and no verdict.
			"(`hl job summary JOB --text T`)",
			"its summaries and its verdict are yours to give as on your own; the idea stays the person's",
			// The CLI caches that its directory is no git repository: the harness clones first.
			"{{workdir}}, the directory every command starts in, holds the clone of {{repository_url}}",
		},
		domain.PlannerAgentType: {
			// A planner proposed a copy with an A100 sed into it for a brief that said CPU only.
			"`hl platform-experiments propose --check FILE`",
			"stores nothing and lists every problem at once, and `propose` only the final one",
			"a goal that says it needs no accelerator is never proposed on one",
			"Every field of the proposal says what the description says.",
			// Refused for "", cpu and the CPU type in turn.
			"`accelerator_type` is an `accelerator_type` string `hl capacity` prints, copied verbatim",
			"Work that needs no accelerator leaves `accelerator_type` out",
			"A refusal lists the valid values",
			// 3.5 minutes of web searches for a toy goal whose Metric and Control were given.
			"only when the goal leaves the control or the size of the effect to expect unknown",
			"a goal that states its Metric and Control needs no search, or one at most",
			// The UI ticks a point of the goal by its label at the start of a line.
			"keeps that label at the start of its own line in the description",
			"`Metric:`", "`Control:`", "`Win:`", "`Held equal:`", "`Fixed:`", "`For:`",
		},
	} {
		for _, want := range said {
			if !strings.Contains(prompts[id], want) {
				t.Errorf("built-in %s no longer says %q", id, want)
			}
		}
	}
	// Agents ended turns with `true` or an `echo` and waited in sleep loops: every role is told
	// once that ending the turn is the way to wait, the sentence the harness refuses a wait with.
	for id, prompt := range prompts {
		if !strings.Contains(prompt, "To wait, reply with no command in it: your turn ends, and the next update starts another.") {
			t.Errorf("built-in %s does not say how to wait", id)
		}
	}
}

func mustBuiltins(t *testing.T) []domain.BuiltinAgentType {
	t.Helper()
	all, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	return all
}
