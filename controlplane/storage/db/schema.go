package db

import (
	"context"
	_ "embed"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed schema.sql
var schemaSQL string

// schemaLockKey is a fixed, arbitrary lock key shared by EnsureSchema and the seeding that follows
// it: the number means nothing, it only has to be the same everywhere so every caller contends on
// the same lock.
const schemaLockKey = 8125_2026

// schemaCheckNamespace is where schema.sql is built, inside a transaction that is rolled back, to
// compare an existing database against it.
const schemaCheckNamespace = "hypothesisloop_schema_check"

// EnsureSchema builds schema.sql on a database with no tables, and otherwise refuses unless the
// database has exactly its shape: every column with its type, nullability, default and place,
// every constraint, index, sequence, view, type, trigger and function. Advisory-locked, so
// replicas starting at once build it once. Called once at startup by control-service alone.
func EnsureSchema(ctx context.Context, pool *Pool) (built bool, err error) {
	conn, err := pool.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("db.EnsureSchema: acquire connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", schemaLockKey); err != nil {
		return false, fmt.Errorf("db.EnsureSchema: acquire advisory lock: %w", err)
	}
	// A fresh context, not the caller's: unlocking with an expired one fails silently and leaves
	// the lock held on this connection for as long as it stays in the pool, wedging every later
	// EnsureSchema and seeding.
	defer func() {
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer unlockCancel()
		conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", schemaLockKey) //nolint:errcheck
	}()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("db.EnsureSchema: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var current string
	var empty bool
	if err := tx.QueryRow(ctx, `SELECT current_schema(), NOT EXISTS (SELECT FROM pg_class WHERE relnamespace = current_schema()::regnamespace)`).
		Scan(&current, &empty); err != nil {
		return false, fmt.Errorf("db.EnsureSchema: %w", err)
	}
	if empty {
		if _, err := tx.Exec(ctx, schemaSQL); err != nil {
			return false, fmt.Errorf("db.EnsureSchema: build: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("db.EnsureSchema: build: %w", err)
		}
		return true, nil
	}
	have, err := schemaShape(ctx, tx, current)
	if err != nil {
		return false, fmt.Errorf("db.EnsureSchema: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE SCHEMA `+schemaCheckNamespace+`; SET LOCAL search_path TO `+schemaCheckNamespace); err != nil {
		return false, fmt.Errorf("db.EnsureSchema: %w", err)
	}
	if _, err := tx.Exec(ctx, schemaSQL); err != nil {
		return false, fmt.Errorf("db.EnsureSchema: build schema.sql to compare: %w", err)
	}
	want, err := schemaShape(ctx, tx, schemaCheckNamespace)
	if err != nil {
		return false, fmt.Errorf("db.EnsureSchema: %w", err)
	}
	var differences []string
	for _, line := range want {
		if !slices.Contains(have, line) {
			differences = append(differences, "missing "+line)
		}
	}
	for _, line := range have {
		if !slices.Contains(want, line) {
			differences = append(differences, "unexpected "+line)
		}
	}
	if len(differences) > 0 {
		return false, fmt.Errorf("db.EnsureSchema: the database is not the schema of schema.sql, so this release does not start on it. It differs in:\n%s",
			strings.Join(differences, "\n"))
	}
	if err := tx.Rollback(ctx); err != nil {
		return false, fmt.Errorf("db.EnsureSchema: %w", err)
	}
	return false, nil
}

// schemaShape is every object of namespace, one line each, read with namespace alone on the
// search path so that no name in a definition carries it.
func schemaShape(ctx context.Context, tx pgx.Tx, namespace string) ([]string, error) {
	if _, err := tx.Exec(ctx, `SELECT set_config('search_path', $1, true)`, pgx.Identifier{namespace}.Sanitize()); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
WITH columns AS (
    SELECT table_name, column_name, data_type, is_nullable, column_default,
           row_number() OVER (PARTITION BY table_name ORDER BY ordinal_position) AS place
    FROM information_schema.columns WHERE table_schema = $1
)
SELECT 'column ' || table_name || '.' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || COALESCE(column_default, '')
FROM columns
UNION ALL
SELECT 'column order ' || table_name || ': ' || string_agg(column_name, ', ' ORDER BY place) FROM columns GROUP BY table_name
UNION ALL
SELECT 'constraint ' || c.conrelid::regclass || ' ' || c.conname || ' ' || pg_get_constraintdef(c.oid)
FROM pg_constraint c WHERE c.connamespace = $1::regnamespace
UNION ALL
SELECT 'index ' || replace(pg_get_indexdef(i.indexrelid), ' ON ' || quote_ident($1) || '.', ' ON ')
FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relnamespace = $1::regnamespace
UNION ALL
SELECT 'sequence ' || c.relname || ' ' || format_type(s.seqtypid, NULL) || ' ' || s.seqstart || ' ' || s.seqincrement
FROM pg_sequence s JOIN pg_class c ON c.oid = s.seqrelid WHERE c.relnamespace = $1::regnamespace
UNION ALL
SELECT 'view ' || c.relname || ' ' || pg_get_viewdef(c.oid) FROM pg_class c WHERE c.relnamespace = $1::regnamespace AND c.relkind IN ('v', 'm')
UNION ALL
SELECT 'type ' || t.typname FROM pg_type t
WHERE t.typnamespace = $1::regnamespace
  AND (t.typtype IN ('e', 'd', 'r', 'm') OR t.typtype = 'c' AND (SELECT c.relkind FROM pg_class c WHERE c.oid = t.typrelid) = 'c')
UNION ALL
SELECT 'trigger ' || t.tgname || ' ' || pg_get_triggerdef(t.oid)
FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid WHERE c.relnamespace = $1::regnamespace AND NOT t.tgisinternal
UNION ALL
SELECT 'function ' || p.oid::regprocedure || ' ' || md5(pg_get_functiondef(p.oid)) FROM pg_proc p WHERE p.pronamespace = $1::regnamespace
ORDER BY 1`, namespace)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
