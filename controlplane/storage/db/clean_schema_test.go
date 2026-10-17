package db

import (
	"context"
	"slices"
	"testing"
	"time"
)

// cleanSchemaVersion is the migration that converts a database to the clean schema.
const cleanSchemaVersion = 4

// A database at the version before the clean schema, holding rows of real runs with the legacy
// values and orphans the conversion cleans, keeps every row of a run and reads as it did.
func TestTheCleanSchemaConversionKeepsEveryRowOfARun(t *testing.T) {
	pool := scratchDatabase(t)
	ctx := context.Background()
	all := embeddedMigrationsWith(t, nil)
	if _, err := migrate(ctx, pool, migrations{steps: all.steps[:cleanSchemaVersion-1]}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.pool.Exec(ctx, `
INSERT INTO users (id, name) VALUES ('u1', 'Ada');
INSERT INTO api_keys (id, key_hash, org_id, created_by, name) VALUES ('k1', 'h1', 'default', 'u1', 'ci');
INSERT INTO agent_types (id, name, role, current_version, created_by_kind) VALUES
  ('generalist', 'Generalist', 'researcher', 1, ''), ('coordinator', 'Coordinator', 'coordinator', 1, 'staff');
INSERT INTO agent_type_versions (type_id, version, prompt, settings, digest, created_by_kind) VALUES
  ('generalist', 1, 'p', '{}', 'd', ''), ('coordinator', 1, 'p', '{}', 'd', 'staff');
INSERT INTO agents (id, name, org_id) VALUES ('a1', 'Researcher 1', 'default'), ('a2', 'Researcher 2', 'default');
INSERT INTO clusters (name, org_id) VALUES ('c1', 'default');

INSERT INTO platform_experiments (id, project_id, name, budget_accelerator_hours, starts_at, ends_at, status, default_quota_tier,
    created_by_kind, closed_by_kind, closed_by_id, closed_at, close_reason, repository,
    coordinator_hosted, coordinator_launcher, coordinator_instance_name, coordinator_instance_asked_at, coordinator_instance_restarts,
    coordinator_instance_restarted_at, coordinator_instance_stopped, coordinator_instance_exit_log, coordinator_instance_end_reason,
    coordinator_instance_activity, coordinator_agent_type_version)
VALUES ('pe1', 'default', 'one', 10, '0001-01-01T00:00:00Z', '2026-01-02T00:00:00Z', 'closed', '',
    '', 'user', 'u1', '2026-01-02T00:00:00Z', 'closed_by_request', 'owner/repo',
    true, 'main', 'coord-m', '2026-01-01T01:00:00Z', 2,
    ARRAY['2026-01-01T02:00:00Z'::timestamptz], true, 'it ended', 'exited',
    'summing up', 1);
INSERT INTO platform_experiments (id, project_id, name, budget_accelerator_hours, starts_at, ends_at, status, default_quota_tier,
    created_by_kind, created_by_id, repository, coordinator_agent_type_version)
VALUES ('pe2', 'default', 'two', 5, '2026-02-01T00:00:00Z', '2026-02-02T00:00:00Z', 'open', 'burst_only', 'key', 'k1', 'owner/other', 1);

INSERT INTO experiment_signups (platform_experiment_id, agent_id, role, quota_tier, added_by_kind, hosted, launcher, flavor, agent_type_version,
    model, instance_name, instance_asked_at, instance_seen_at, instance_restarts, instance_renewals, instance_state, instance_machine,
    instance_progress, instance_launch, instance_end_reason)
VALUES ('pe1', 'a1', 'competitor', '', '', true, 'main', '', NULL,
    'model-1', 'a1-m', '2026-01-01T01:00:00Z', '2026-01-01T01:01:00Z', 1, 3, 'running', 'machine-1',
    'step 4', 'launch-1', 'silent');
INSERT INTO experiment_signups (platform_experiment_id, agent_id, quota_tier, added_by_kind, added_by_id, flavor, model)
VALUES ('pe1', 'a2', 'guaranteed', 'user', 'u1', 'generalist', 'model-1'),
       ('pe2', 'a2', '', 'coordinator', 'pe2', 'generalist', 'model-2');
INSERT INTO agent_quotas (id, agent_id, platform_experiment_id, guaranteed_accelerator_hours, burst_accelerator_hours)
VALUES ('q1', 'a1', 'pe1', 4, 2);
INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index) VALUES ('pe1', 'a2', 1);

INSERT INTO environments (key, org_id, base, dockerfile, image, state) VALUES
  ('env-used', 'default', 'b', 'f', 'img', 'ready'),
  ('env-waited', 'default', 'b', 'f', '', 'building'),
  ('env-nobodys', 'default', 'b', 'f', 'img', 'ready');
INSERT INTO environments (key, org_id, base, dockerfile, image, state, project_id, git_repository) VALUES
  ('env-kept', 'default', 'b', 'f', '', 'building', 'default', 'owner/repo');
INSERT INTO experiments (id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref, hypothesis_id, hypothesis,
    accelerator_type, capacity_tier, status, estimated_duration_hours, actor_kind, actor_id, environment_key)
VALUES ('job1', 'a1', 'pe1', 'owner/repo@abc', 'c', 'd', 'h1', 'a claim', 'cpu', 'burst', 'COMPLETED', 1, 'agent', 'a1', 'env-used'),
       ('job2', 'a1', 'pe1', 'owner/repo@def', 'c', 'd', 'h1', 'a claim', 'cpu', 'guaranteed', 'FAILED', 1, 'agent', 'a1', NULL);
INSERT INTO environment_waiters (environment_key, platform_experiment_id, agent_id, code_ref, image) VALUES
  ('env-waited', 'pe2', 'a2', 'owner/other@1', 'img'),
  ('env-kept', 'pe2', 'ghost', 'owner/other@1', 'img');
INSERT INTO baseline_raises (platform_experiment_id, from_job_id, to_job_id, reason, detail, actor_kind, created_xid) VALUES
  ('pe1', 'job2', 'job1', 'stronger', 'raised', 'coordinator', 7),
  ('pe1', 'job1', 'job-gone', 'stronger', 'raised', 'coordinator', 8);
INSERT INTO agent_messages (platform_experiment_id, agent_id, sender, text, actor_kind, created_xid) VALUES
  ('pe1', 'coordinator', 'owner', 'go on', 'staff', 9), ('pe1', 'a1', 'coordinator', 'try this', 'coordinator', 10);
INSERT INTO planning_requests (platform_experiment_id, message, actor_kind, actor_id) VALUES ('pe1', 'smaller', 'user', 'u1');
INSERT INTO hypothesis_findings (hypothesis_id, platform_experiment_id, experiment_id, agent_id, summary, actor_kind, actor_id)
VALUES ('h1', 'pe1', 'job1', 'a1', 'it held', 'agent', 'a1');
INSERT INTO hypothesis_comments (id, hypothesis_id, platform_experiment_id, source, author, text, actor_kind) VALUES
  ('cm1', 'h1', 'pe1', 'human', 'Grace', 'old note', 'unverified');
INSERT INTO org_disabled_clusters (org_id, cluster_name, disabled_by) VALUES ('default', 'c1', 'staff');
INSERT INTO orgs (id, name) VALUES ('o2', 'two'), ('o3', 'three'), ('o4', 'four');
INSERT INTO org_disabled_clusters (org_id, cluster_name, disabled_by) VALUES ('o2', 'c1', 'u1'), ('o3', 'c1', 'k1'), ('o4', 'c1', 'someone');
INSERT INTO org_github_installations (org_id, installation_id, account_login, connected_by, connected_by_kind, account_type) VALUES
  ('default', 7, 'acme', 'hl-admin', 'staff', ''), ('o2', 7, 'acme', 'u1', 'user', 'Organization'),
  ('o3', 8, 'solo', '', '', '');
`); err != nil {
		t.Fatalf("rows at %04d: %v", cleanSchemaVersion-1, err)
	}
	before := rowCounts(t, pool)

	done, err := migrate(ctx, pool, all)
	if err != nil || !slices.Contains(done.Applied, all.steps[cleanSchemaVersion-1].file()) {
		t.Fatalf("convert: got = %+v, %v", done, err)
	}

	after := rowCounts(t, pool)
	// event_times goes with the triggers that filled it (0009), planning_requests with goal edits
	// (0010): a closed run's revision has nothing left to plan.
	dropped := map[string]int{"environments": 1, "environment_waiters": 1, "baseline_raises": 1, "event_times": before["event_times"], "planning_requests": 1}
	for table, n := range before {
		if after[table] != n-dropped[table] {
			t.Errorf("%s: got %d rows, want %d of %d", table, after[table], n-dropped[table], n)
		}
	}

	var startsAt *time.Time
	var tier, createdBy, closedBy, status string
	one(t, pool, `SELECT starts_at, default_quota_tier, created_by_kind, closed_by_kind || ':' || closed_by_id, status FROM platform_experiments WHERE id = 'pe1'`,
		&startsAt, &tier, &createdBy, &closedBy, &status)
	if startsAt != nil || tier != "guaranteed" || createdBy != "unverified" || closedBy != "user:u1" || status != "closed" {
		t.Errorf("pe1: got starts_at %v, tier %q, created by %q, closed by %q, status %q", startsAt, tier, createdBy, closedBy, status)
	}
	one(t, pool, `SELECT starts_at, default_quota_tier, created_by_kind FROM platform_experiments WHERE id = 'pe2'`, &startsAt, &tier, &createdBy)
	if startsAt == nil || !startsAt.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) || tier != "burst_only" || createdBy != "key" {
		t.Errorf("pe2: got starts_at %v, tier %q, created by %q", startsAt, tier, createdBy)
	}

	for want, query := range map[string]string{
		"guaranteed unverified generalist":  `SELECT quota_tier || ' ' || added_by_kind || ' ' || agent_type FROM experiment_signups WHERE platform_experiment_id = 'pe1' AND agent_id = 'a1'`,
		"guaranteed user generalist":        `SELECT quota_tier || ' ' || added_by_kind || ' ' || agent_type FROM experiment_signups WHERE platform_experiment_id = 'pe1' AND agent_id = 'a2'`,
		"burst_only coordinator generalist": `SELECT quota_tier || ' ' || added_by_kind || ' ' || agent_type FROM experiment_signups WHERE platform_experiment_id = 'pe2' AND agent_id = 'a2'`,
		"a1-m launch-1 running machine-1 step 4 1 3 silent false": `SELECT name || ' ' || launch || ' ' || state || ' ' || machine || ' ' || progress || ' ' || restarts || ' ' || renewals || ' ' || end_reason || ' ' || stopped
			FROM hosted_instances WHERE platform_experiment_id = 'pe1' AND agent_id = 'a1' AND asked_at = '2026-01-01T01:00:00Z' AND seen_at = '2026-01-01T01:01:00Z'`,
		"coord-m 2 true it ended exited summing up 1": `SELECT name || ' ' || restarts || ' ' || stopped || ' ' || exit_log || ' ' || end_reason || ' ' || activity || ' ' || cardinality(restarted_at)
			FROM hosted_instances WHERE platform_experiment_id = 'pe1' AND agent_id = '' AND asked_at = '2026-01-01T01:00:00Z'`,
		"2":                         `SELECT count(*)::text FROM hosted_instances`,
		"burst COMPLETED":           `SELECT capacity_tier || ' ' || status FROM experiments WHERE id = 'job1'`,
		"default owner/repo":        `SELECT project_id || ' ' || git_repository FROM environments WHERE key = 'env-used'`,
		"default owner/other":       `SELECT project_id || ' ' || git_repository FROM environments WHERE key = 'env-waited'`,
		"0":                         `SELECT count(*)::text FROM environments WHERE key = 'env-nobodys'`,
		"a2":                        `SELECT string_agg(agent_id, ',') FROM environment_waiters`,
		"job2 job1":                 `SELECT from_job_id || ' ' || to_job_id FROM baseline_raises`,
		"coordinator,a1":            `SELECT string_agg(agent_id, ',' ORDER BY id) FROM agent_messages`,
		"Grace unverified old note": `SELECT author || ' ' || actor_kind || ' ' || text FROM hypothesis_comments WHERE id = 'cm1'`,
		"key:k1,staff:,unverified:someone,user:u1": `SELECT string_agg(disabled_by_kind || ':' || disabled_by_id, ',' ORDER BY disabled_by_kind) FROM org_disabled_clusters`,
		"Organization staff hl-admin":              `SELECT account_type || ' ' || connected_by_kind || ' ' || connected_by_id FROM org_github_installations WHERE org_id = 'default'`,
		" unverified ":                             `SELECT account_type || ' ' || connected_by_kind || ' ' || connected_by_id FROM org_github_installations WHERE org_id = 'o3'`,
		"unverified staff":                         `SELECT string_agg(created_by_kind, ' ' ORDER BY id DESC) FROM agent_types`,
		"4 2":                                      `SELECT guaranteed_accelerator_hours || ' ' || burst_accelerator_hours FROM agent_quotas WHERE id = 'q1'`,
	} {
		var got string
		one(t, pool, query, &got)
		if got != want {
			t.Errorf("%s: got %q, want %q", query, got, want)
		}
	}
	for _, gone := range []string{"experiment_status", "platform_experiment_status", "capacity_tier"} {
		var found bool
		one(t, pool, `SELECT to_regtype($1) IS NOT NULL`, &found, gone)
		if found {
			t.Errorf("type %s: still there after the conversion", gone)
		}
	}
}

// The conversion refuses a database that still holds a legacy table the previous release moves at
// its start, and changes nothing.
func TestTheCleanSchemaConversionWaitsForTheLegacyTablesToBeMoved(t *testing.T) {
	pool := scratchDatabase(t)
	ctx := context.Background()
	all := embeddedMigrationsWith(t, nil)
	if _, err := migrate(ctx, pool, migrations{steps: all.steps[:cleanSchemaVersion-1]}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.pool.Exec(ctx, `CREATE TABLE human_writes (id BIGINT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate(ctx, pool, all); err == nil {
		t.Fatal("convert with human_writes still there: got = nil, want a refusal")
	}
	var flavor bool
	one(t, pool, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'experiment_signups' AND column_name = 'flavor')`, &flavor)
	if !flavor {
		t.Error("experiment_signups.flavor: gone after a refused conversion, want the schema untouched")
	}
}

func one(t *testing.T, pool *Pool, query string, dest ...any) {
	t.Helper()
	var args []any
	if len(dest) > 1 {
		if s, ok := dest[len(dest)-1].(string); ok {
			args, dest = []any{s}, dest[:len(dest)-1]
		}
	}
	if err := pool.pool.QueryRow(context.Background(), query, args...).Scan(dest...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// rowCounts is the number of rows of every table of the public schema but the ledger.
func rowCounts(t *testing.T, pool *Pool) map[string]int {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename <> 'schema_migrations'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	counts := map[string]int{}
	for _, table := range tables {
		var n int
		if err := pool.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		counts[table] = n
	}
	return counts
}
