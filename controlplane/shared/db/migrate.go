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

// LegacyHypotheses returns every row of the Postgres hypotheses table, which predates the graph
// store, or found=false once that table has been dropped. Part of the one-time move of hypotheses
// into the graph: the caller copies these rows there and then calls DropLegacyHypotheses.
func LegacyHypotheses(ctx context.Context, pool *Pool) (rows []*domain.Hypothesis, found bool, err error) {
	if err := pool.pool.QueryRow(ctx, `SELECT to_regclass('hypotheses') IS NOT NULL`).Scan(&found); err != nil {
		return nil, false, fmt.Errorf("db.LegacyHypotheses: %w", err)
	}
	if !found {
		return nil, false, nil
	}
	r, err := pool.pool.Query(ctx, `SELECT id, COALESCE(agent_id, ''), source, author, platform_experiment_id, text,
       status::text, created_at, status_changed_at FROM hypotheses ORDER BY created_at`)
	if err != nil {
		return nil, false, fmt.Errorf("db.LegacyHypotheses: %w", err)
	}
	defer r.Close()
	for r.Next() {
		h := &domain.Hypothesis{}
		if err := r.Scan(&h.ID, &h.AgentID, &h.Source, &h.Author, &h.PlatformExperimentID, &h.Text,
			&h.Status, &h.CreatedAt, &h.StatusChangedAt); err != nil {
			return nil, false, fmt.Errorf("db.LegacyHypotheses: scan: %w", err)
		}
		rows = append(rows, h)
	}
	return rows, true, r.Err()
}

// DropLegacyHypotheses finishes the move of hypotheses into the graph store: it backfills
// platform_experiment_id on findings and comments from the old table, then drops the table (its
// triggers and every foreign key pointing at it go with it), its trigger functions and its enum.
// Call it only after LegacyHypotheses' rows are safely in the graph.
func DropLegacyHypotheses(ctx context.Context, pool *Pool) error {
	tx, err := pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db.DropLegacyHypotheses: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
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
			return fmt.Errorf("db.DropLegacyHypotheses: %s: %w", q, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db.DropLegacyHypotheses: commit: %w", err)
	}
	return nil
}
