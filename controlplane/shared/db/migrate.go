package db

import (
	"context"
	"fmt"
	"os"
	"time"
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
