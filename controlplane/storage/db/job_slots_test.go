package db

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A database stored before a job held its slots in rank order has the slot each resolved job
// named in its node selector made the slot of its one node at the next start, the rest of its
// selector kept, and a job on no slot left as it is. A second start changes nothing more.
func TestAStartMovesTheSlotOfEveryStoredJobOutOfItsNodeSelector(t *testing.T) {
	pool := builtDatabase(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO agents (id, name, org_id) VALUES ('agent-1', 'agent-1', 'default')`)
	exec(`INSERT INTO platform_experiments (id, project_id, name, budget_accelerator_hours, starts_at, ends_at, default_quota_tier, created_by_kind)
VALUES ('pe-1', 'default', 'pe-1', 1, now(), now() + interval '1 hour', 'guaranteed', 'staff')`)
	resolved := map[string]string{
		"on-slot":    `{"cpu":"8","node_selector":{"kubernetes.io/arch":"amd64","` + domain.NodeSlotLabel + `":"h100-1"}}`,
		"slot-alone": `{"cpu":"4","node_selector":{"` + domain.NodeSlotLabel + `":"cpu-0"}}`,
		"on-nodes":   `{"cpu":"8","node_selector":{"zone":"a"}}`,
		"on-slots":   `{"cpu":"8","num_nodes":2,"slots":["h100-0","h100-1"]}`,
		"unresolved": ``,
	}
	for id, spec := range resolved {
		exec(`INSERT INTO experiments (actor_kind, id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, accelerator_type, status, estimated_duration_hours, queued_at, data_access_key_id, data_secret_access_key, resolved_job_spec)
VALUES ('agent', $1, 'agent-1', 'pe-1', 'r@0', 'h', 'd', 'hyp', 'h', 'test-accelerator', 'RUNNING', 1, now(), 'AK', 'SK', NULLIF($2, '')::jsonb)`, id, spec)
	}

	for range 2 {
		if built, err := EnsureSchema(ctx, pool); err != nil || built {
			t.Fatalf("start: %v, %v", built, err)
		}
	}

	want := map[string]*domain.JobSpec{
		"on-slot":    {CPU: "8", NodeSelector: map[string]string{"kubernetes.io/arch": "amd64"}, Slots: []string{"h100-1"}},
		"slot-alone": {CPU: "4", NodeSelector: map[string]string{}, Slots: []string{"cpu-0"}},
		"on-nodes":   {CPU: "8", NodeSelector: map[string]string{"zone": "a"}},
		"on-slots":   {CPU: "8", NumNodes: 2, Slots: []string{"h100-0", "h100-1"}},
		"unresolved": nil,
	}
	for id, want := range want {
		var stored []byte
		if err := pool.pool.QueryRow(ctx, `SELECT resolved_job_spec FROM experiments WHERE id = $1`, id).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		var got *domain.JobSpec
		if stored != nil {
			got = &domain.JobSpec{}
			if err := json.Unmarshal(stored, got); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: resolved job %s, want %+v", id, stored, want)
		}
	}
	placements, err := NewStore(pool, 3).PlacementByID(ctx, []string{"on-slot", "on-slots", "on-nodes"})
	if err != nil {
		t.Fatal(err)
	}
	if placements["on-slot"].Slot != "h100-1" || placements["on-slots"].Slot != "h100-0" || placements["on-nodes"].Slot != "" {
		t.Errorf("placements %+v, want each job's first slot", placements)
	}
}
