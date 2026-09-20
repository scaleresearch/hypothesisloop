package db

import (
	"context"
	"errors"
	"maps"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// scratchDatabase creates an empty database and drops it afterwards, for tests that apply
// schema.sql to a database of their own.
func scratchDatabase(t *testing.T) *Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := NewPool(ctx, Config{DSN: serverDSN(), MaxConns: 1, MinConns: 1})
	if err != nil {
		t.Skipf("no database at %s: %v", serverDSN(), err)
	}
	t.Cleanup(admin.Close)
	name := "schema_test_" + uuid.New().String()[:8]
	if _, err := admin.pool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.pool.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	dsn, err := url.Parse(serverDSN())
	if err != nil {
		t.Fatal(err)
	}
	dsn.Path = "/" + name
	pool, err := NewPool(ctx, Config{DSN: dsn.String(), MaxConns: 2, MinConns: 1})
	if err != nil {
		t.Fatalf("connect scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func applyTestSchema(t *testing.T, pool *Pool) {
	t.Helper()
	t.Setenv("HYPOTHESISLOOP_SCHEMA_SQL", "schema.sql")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ApplySchema(ctx, pool); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
}

// An active row from before per-job keys would run with empty AWS_* credentials. Applying the
// schema cancels it, and afterwards no active row can be written without a key.
func TestSchemaCancelsActiveRowsWithoutADataKey(t *testing.T) {
	pool := scratchDatabase(t)
	applyTestSchema(t, pool)
	ctx := context.Background()
	createTestAgent(t, pool, "legacy-agent", domain.AgentKindAgent)
	createTestPE(t, pool, "legacy-pe", 10)
	if _, err := pool.pool.Exec(ctx, `ALTER TABLE experiments DROP CONSTRAINT experiments_active_has_data_key`); err != nil {
		t.Fatal(err)
	}
	insert := func(id, status, notAdmitted, key string) error {
		_, err := pool.pool.Exec(ctx, `INSERT INTO experiments
(id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, objective, accelerator_type, status, not_admitted_reason,
 estimated_duration_hours, data_access_key_id, data_secret_access_key)
VALUES ($1, 'legacy-agent', 'legacy-pe', 'r@0', 'h', 'd', 'h', 'claim', 'objective', 'cpu',
        $2, NULLIF($3, ''), 1, $4, $4)`, id, status, notAdmitted, key)
		return err
	}
	for _, row := range []struct{ id, status, notAdmitted, key string }{
		{"legacy-queued", "QUEUED", "capacity_unavailable", ""},
		{"legacy-running", "RUNNING", "", ""},
		{"legacy-done", "COMPLETED", "", ""},
		{"keyed-running", "RUNNING", "", "AK"},
	} {
		if err := insert(row.id, row.status, row.notAdmitted, row.key); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}

	applyTestSchema(t, pool)

	for id, want := range map[string]string{
		"legacy-queued": "REJECTED", "legacy-running": "EVICTED", "legacy-done": "COMPLETED", "keyed-running": "RUNNING",
	} {
		var status, reason string
		if err := pool.pool.QueryRow(ctx, `SELECT status, COALESCE(eviction_reason, '') FROM experiments WHERE id = $1`, id).
			Scan(&status, &reason); err != nil {
			t.Fatal(err)
		}
		if status != want {
			t.Fatalf("%s status = %s, want %s", id, status, want)
		}
		if cancelled := domain.EvictionReason(reason).Code() == domain.EvictionCancelled; cancelled != (want != "COMPLETED" && want != "RUNNING") {
			t.Fatalf("%s eviction reason = %q", id, reason)
		}
	}

	var pgErr *pgconn.PgError
	if err := insert("new-unkeyed", "QUEUED", "capacity_unavailable", ""); !errors.As(err, &pgErr) || pgErr.ConstraintName != "experiments_active_has_data_key" {
		t.Fatalf("insert of an active row without a key = %v, want the constraint to refuse it", err)
	}
}

// Clusters were not registered before tenancy, and a runtime of an unregistered cluster is
// refused. Applying the schema registers the ones jobs are placed on, once.
func TestSchemaRegistersTheClustersJobsArePlacedOnForTheDefaultOrg(t *testing.T) {
	pool := scratchDatabase(t)
	applyTestSchema(t, pool)
	ctx := context.Background()
	createTestAgent(t, pool, "legacy-agent", domain.AgentKindAgent)
	createTestPE(t, pool, "legacy-pe", 10)
	place := func(id, status, cluster string, submitted bool) {
		t.Helper()
		if _, err := pool.pool.Exec(ctx, `INSERT INTO experiments
(id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, objective, accelerator_type, status, cluster_name, submitted_at,
 estimated_duration_hours, data_access_key_id, data_secret_access_key)
VALUES ($1, 'legacy-agent', 'legacy-pe', 'r@0', 'h', 'd', 'h', 'claim', 'objective', 'cpu',
        $2, $3, CASE WHEN $4 THEN now() END, 1, 'AK', 'SK')`, id, status, cluster, submitted); err != nil {
			t.Fatalf("seed job %s: %v", id, err)
		}
	}
	place("running-on-a", "RUNNING", "legacy-a", true)
	place("also-on-a", "SUBMITTED", "legacy-a", true)
	place("running-on-b", "RUNNING", "legacy-b", true)
	place("finished", "COMPLETED", "", true)
	// A row that was never placed still carries the column's default name.
	if _, err := pool.pool.Exec(ctx, `INSERT INTO experiments
(id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, objective, accelerator_type, status, not_admitted_reason,
 estimated_duration_hours, data_access_key_id, data_secret_access_key)
VALUES ('never-placed', 'legacy-agent', 'legacy-pe', 'r@0', 'h', 'd', 'h', 'claim', 'objective', 'cpu',
        'QUEUED', 'awaiting_admission', 1, 'AK', 'SK')`); err != nil {
		t.Fatalf("seed queued job: %v", err)
	}
	registered := func() map[string]string {
		t.Helper()
		rows, err := pool.pool.Query(ctx, `SELECT name, org_id FROM clusters`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var name, org string
			if err := rows.Scan(&name, &org); err != nil {
				t.Fatal(err)
			}
			out[name] = org
		}
		return out
	}
	if got := registered(); len(got) != 0 {
		t.Fatalf("clusters before the upgrade: got = %v, want none", got)
	}

	want := map[string]string{"legacy-a": "default", "legacy-b": "default"}
	for _, pass := range []string{"the upgrade", "the next boot"} {
		applyTestSchema(t, pool)
		if got := registered(); !maps.Equal(got, want) {
			t.Fatalf("clusters after %s: got = %v, want = %v", pass, got, want)
		}
	}

	// Once any cluster is registered the table is the orgs' own: a name a job carries is no
	// longer a reason to register it.
	if _, err := pool.pool.Exec(ctx, `DELETE FROM clusters WHERE name = 'legacy-b'`); err != nil {
		t.Fatal(err)
	}
	applyTestSchema(t, pool)
	if got := registered(); !maps.Equal(got, map[string]string{"legacy-a": "default"}) {
		t.Fatalf("clusters after one was removed: got = %v, want legacy-b to stay removed", got)
	}
}
