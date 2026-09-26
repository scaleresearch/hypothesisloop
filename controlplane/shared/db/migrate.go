package db

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// DefaultSchemaSQLPath is where control-service's Docker image bakes
// controlplane/shared/db/schema.sql (see controlplane/build/Dockerfile.control-service) — the one
// checked-in copy, never duplicated into a Helm chart, a podman-only init script, or
// metrics-service's own image (which reads the same Postgres tables but never owns their schema).
// Overridable via HYPOTHESISLOOP_SCHEMA_SQL for local `go run` against a repo checkout.
const DefaultSchemaSQLPath = "/schema/schema.sql"

// ApplySchema runs schema.sql (every statement in it idempotent by construction — see the file's
// own header) against pool, serialized by a Postgres advisory lock so two processes starting at
// once (two control-service replicas) can't run DDL concurrently and trip Postgres's "tuple
// concurrently updated"/duplicate-object races. Called once at startup by control-service alone
// — the schema is a property of that one binary, not of whichever deployment mechanism (podman,
// Helm, bare `go run`) happens to start it first, and not something every Postgres-touching
// service repeats.
func ApplySchema(ctx context.Context, pool *Pool) error {
	path := os.Getenv("HYPOTHESISLOOP_SCHEMA_SQL")
	if path == "" {
		path = DefaultSchemaSQLPath
	}
	sql, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("db.ApplySchema: read %s: %w", path, err)
	}

	conn, err := pool.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("db.ApplySchema: acquire connection: %w", err)
	}
	defer conn.Release()

	// A fixed, arbitrary lock key shared by every caller of this function — the number itself
	// means nothing, it only has to be the same constant everywhere so every caller contends on
	// the same lock rather than each getting its own.
	const schemaMigrationLockKey = 8125_2026
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", schemaMigrationLockKey); err != nil {
		return fmt.Errorf("db.ApplySchema: acquire advisory lock: %w", err)
	}
	// A fresh context, not the caller's ctx: if ctx is already at (or near) its deadline by the
	// time the schema exec below returns — the common case, since this shares the same 30s budget
	// — unlocking with an expired context fails silently and leaves the lock held on this
	// connection for as long as it stays in the pool, wedging every future ApplySchema call.
	defer func() {
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer unlockCancel()
		conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", schemaMigrationLockKey) //nolint:errcheck
	}()

	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		return fmt.Errorf("db.ApplySchema: exec %s: %w", path, err)
	}
	return nil
}

// legacyMoveLockKey serialises MoveLegacyHypotheses across control-service replicas.
const legacyMoveLockKey = 8125_2027

// MoveLegacyHypotheses moves the rows of the Postgres hypotheses table, which predates the graph
// store, into the graph with importRows, then backfills platform_experiment_id on findings and
// comments from it and drops the table (its triggers and every foreign key pointing at it go with
// it), its trigger functions and its enum. All of it runs in one transaction holding an advisory
// lock, so a second replica starting at the same time waits and then finds the table gone, and a
// crash part way leaves the table for the next start. importRows must be idempotent: after such a
// crash it sees rows it already imported. Without the table it does nothing.
func MoveLegacyHypotheses(ctx context.Context, pool *Pool, importRows func([]*domain.Hypothesis) error) error {
	tx, err := pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db.MoveLegacyHypotheses: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", legacyMoveLockKey); err != nil {
		return fmt.Errorf("db.MoveLegacyHypotheses: lock: %w", err)
	}
	var found bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('hypotheses') IS NOT NULL`).Scan(&found); err != nil {
		return fmt.Errorf("db.MoveLegacyHypotheses: %w", err)
	}
	if !found {
		return nil
	}
	r, err := tx.Query(ctx, `SELECT id, COALESCE(agent_id, ''), source, author, platform_experiment_id, text,
       status::text, created_at, status_changed_at FROM hypotheses ORDER BY created_at`)
	if err != nil {
		return fmt.Errorf("db.MoveLegacyHypotheses: %w", err)
	}
	var rows []*domain.Hypothesis
	for r.Next() {
		h := &domain.Hypothesis{}
		if err := r.Scan(&h.ID, &h.AgentID, &h.Source, &h.Actor.Name, &h.PlatformExperimentID, &h.Text,
			&h.Status, &h.CreatedAt, &h.StatusChangedAt); err != nil {
			r.Close()
			return fmt.Errorf("db.MoveLegacyHypotheses: scan: %w", err)
		}
		rows = append(rows, h)
	}
	r.Close()
	if err := r.Err(); err != nil {
		return fmt.Errorf("db.MoveLegacyHypotheses: %w", err)
	}
	if err := importRows(rows); err != nil {
		return fmt.Errorf("db.MoveLegacyHypotheses: import: %w", err)
	}
	for _, q := range []string{
		`UPDATE hypothesis_findings f SET platform_experiment_id = h.platform_experiment_id
FROM hypotheses h WHERE h.id = f.hypothesis_id AND f.platform_experiment_id IS NULL`,
		`UPDATE hypothesis_comments c SET platform_experiment_id = h.platform_experiment_id
FROM hypotheses h WHERE h.id = c.hypothesis_id AND c.platform_experiment_id IS NULL`,
		`ALTER TABLE hypothesis_findings ALTER COLUMN platform_experiment_id SET NOT NULL`,
		`ALTER TABLE hypothesis_comments ALTER COLUMN platform_experiment_id SET NOT NULL`,
		`DROP TABLE hypotheses CASCADE`,
		`DROP FUNCTION IF EXISTS hypothesisloop_hypotheses_notify(), hypothesisloop_hypotheses_touch(), hypothesisloop_hypotheses_status_notify()`,
		`DROP TYPE IF EXISTS hypothesis_status`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("db.MoveLegacyHypotheses: %s: %w", q, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db.MoveLegacyHypotheses: commit: %w", err)
	}
	return nil
}
