package db

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A database whose models were stored bare has each made anthropic/<model> at the next start, an
// agent type version's digest with it, and a model already a reference is left as it is.
func TestAStartMakesEveryBareModelStoredAnAnthropicReference(t *testing.T) {
	pool := builtDatabase(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO agents (id, name, org_id) VALUES ('agent-1', 'agent-1', 'default'), ('agent-2', 'agent-2', 'default')`)
	for id, coordinator := range map[string]string{"pe-bare": "claude-opus-5-5", "pe-ref": "openrouter/moonshotai/kimi-k2"} {
		model := "claude-sonnet-5"
		if id == "pe-ref" {
			model = "openrouter/moonshotai/kimi-k2"
		}
		agents := `[{"agent_type":"generalist","model":"` + model + `","count":1},{"count":2}]`
		proposal := `{"goal_version":1,"agents":[{"model":"` + model + `"},{"agent_type":"generalist"}]}`
		exec(`INSERT INTO platform_experiments (id, project_id, name, budget_accelerator_hours, starts_at, ends_at, default_quota_tier, created_by_kind, coordinator_model, agents, proposal)
VALUES ($1, 'default', $1, 1, now(), now() + interval '1 hour', 'guaranteed', 'staff', $2, $3, $4)`, id, coordinator, agents, proposal)
	}
	exec(`INSERT INTO experiment_signups (platform_experiment_id, agent_id, quota_tier, added_by_kind, agent_type, model)
VALUES ('pe-bare', 'agent-1', 'guaranteed', 'staff', 'generalist', 'claude-haiku-4-5'), ('pe-bare', 'agent-2', 'guaranteed', 'staff', 'generalist', NULL)`)
	settings := testTypeSettings()
	settings.Model = "claude-sonnet-5"
	bare, err := NewStore(pool, 3).Create(ctx, "default", "Bare", "", domain.AgentRoleExecutor, testPrompt, settings, *testActor)
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if built, err := EnsureSchema(ctx, pool); err != nil || built {
			t.Fatalf("start: %v, %v", built, err)
		}
	}

	var signups []*string
	if err := pool.pool.QueryRow(ctx, `SELECT array_agg(model ORDER BY agent_id) FROM experiment_signups WHERE platform_experiment_id = 'pe-bare'`).Scan(&signups); err != nil {
		t.Fatal(err)
	}
	if len(signups) != 2 || *signups[0] != "anthropic/claude-haiku-4-5" || signups[1] != nil {
		t.Errorf("signups' models = %v, want the bare one a reference and the type's left none", signups)
	}
	for id, want := range map[string][2]string{
		"pe-bare": {"anthropic/claude-opus-5-5", "anthropic/claude-sonnet-5"},
		"pe-ref":  {"openrouter/moonshotai/kimi-k2", "openrouter/moonshotai/kimi-k2"},
	} {
		var coordinator string
		var agents, proposal []byte
		if err := pool.pool.QueryRow(ctx, `SELECT coordinator_model, agents, proposal FROM platform_experiments WHERE id = $1`, id).Scan(&coordinator, &agents, &proposal); err != nil {
			t.Fatal(err)
		}
		var entries []map[string]any
		var proposed struct {
			GoalVersion int              `json:"goal_version"`
			Agents      []map[string]any `json:"agents"`
		}
		if err := json.Unmarshal(agents, &entries); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(proposal, &proposed); err != nil {
			t.Fatal(err)
		}
		if coordinator != want[0] || len(entries) != 2 || entries[0]["model"] != want[1] || entries[0]["agent_type"] != "generalist" || entries[1]["model"] != nil || entries[1]["count"] != 2.0 ||
			proposed.GoalVersion != 1 || len(proposed.Agents) != 2 || proposed.Agents[0]["model"] != want[1] || proposed.Agents[1]["model"] != nil {
			t.Errorf("%s: coordinator %s, agents %s, proposal %s, want %v", id, coordinator, agents, proposal, want)
		}
	}
	got, err := NewStore(pool, 3).Get(ctx, "default", bare.ID)
	if err != nil {
		t.Fatal(err)
	}
	settings.Model = "anthropic/claude-sonnet-5"
	digest, _ := domain.AgentTypeDigest(testPrompt, settings)
	if got.Current.Settings.Model != "anthropic/claude-sonnet-5" || got.Current.Digest != digest || got.Current.Version != 1 {
		t.Errorf("agent type version: model %s, digest %s at version %d, want the reference with its digest %s, still version 1",
			got.Current.Settings.Model, got.Current.Digest, got.Current.Version, digest)
	}
}
