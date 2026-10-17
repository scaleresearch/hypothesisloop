package db

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// scratchDatabase creates an empty database and drops it afterwards, for tests that build a
// schema of their own.
func scratchDatabase(t *testing.T) *Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := NewPool(ctx, Config{DSN: serverDSN(), MaxConns: 1, MinConns: 1, Timeouts: testTimeouts})
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
	pool, err := NewPool(ctx, Config{DSN: dsn.String(), MaxConns: 4, MinConns: 1, Timeouts: testTimeouts})
	if err != nil {
		t.Fatalf("connect scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// builtDatabase is a scratch database built as TestMain builds the shared one, for tests whose
// seeding would retire the shared one's built-ins or replace its rules.
func builtDatabase(t *testing.T) *Pool {
	t.Helper()
	pool := scratchDatabase(t)
	if err := build(context.Background(), pool); err != nil {
		t.Fatalf("build: %v", err)
	}
	return pool
}

func requireNoCheckNamespace(t *testing.T, pool *Pool) {
	t.Helper()
	var leftover bool
	if err := pool.pool.QueryRow(context.Background(), `SELECT to_regnamespace($1) IS NOT NULL`, schemaCheckNamespace).Scan(&leftover); err != nil || leftover {
		t.Fatalf("the namespace of the shape check: got = %v, %v, want it rolled back", leftover, err)
	}
}

func TestAnEmptyDatabaseIsBuiltFromSchemaSQLOnce(t *testing.T) {
	pool := scratchDatabase(t)
	ctx := context.Background()
	if built, err := EnsureSchema(ctx, pool); err != nil || !built {
		t.Fatalf("first start: got = %v, %v, want built", built, err)
	}
	if built, err := EnsureSchema(ctx, pool); err != nil || built {
		t.Fatalf("next start: got = %v, %v, want the schema found as it is", built, err)
	}
	requireNoCheckNamespace(t, pool)
	var org string
	if err := pool.pool.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = 'default'`).Scan(&org); err != nil || org != "default" {
		t.Fatalf("the default project: got = %q, %v, want it in org default", org, err)
	}
}

func TestReplicasStartingAtOnceBuildTheSchemaOnce(t *testing.T) {
	pool := scratchDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	built := make([]bool, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range built {
		wg.Go(func() { built[i], errs[i] = EnsureSchema(ctx, pool) })
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || built[0] == built[1] {
		t.Fatalf("got = %v, %v, want one replica to build and the other to find it built", built, errs)
	}
}

// Every way a database can drift from schema.sql is refused, and the refusal names it.
func TestADatabaseOfAnotherShapeIsRefused(t *testing.T) {
	for name, drift := range map[string]struct{ ddl, named string }{
		"an extra column":    {`ALTER TABLE agents ADD COLUMN note TEXT`, "unexpected column agents.note text YES"},
		"a missing index":    {`DROP INDEX idx_agents_org`, "missing index CREATE INDEX idx_agents_org ON agents USING btree (org_id)"},
		"another default":    {`ALTER TABLE agents ALTER COLUMN created_at SET DEFAULT '2026-01-01'`, "missing column agents.created_at timestamp with time zone NO now()"},
		"an extra table":     {`CREATE TABLE stray (id TEXT)`, "unexpected column stray.id text YES"},
		"a function":         {`CREATE FUNCTION stray() RETURNS INTEGER LANGUAGE SQL AS 'SELECT 1'`, "unexpected function stray()"},
		"another constraint": {`ALTER TABLE agents ADD CONSTRAINT agents_named CHECK (name <> '')`, "unexpected constraint agents agents_named CHECK"},
		"another column order": {`ALTER TABLE users DROP COLUMN name; ALTER TABLE users ADD COLUMN name TEXT NOT NULL DEFAULT ''`,
			"unexpected column order users: id, staff, created_at, name"},
	} {
		t.Run(name, func(t *testing.T) {
			pool := scratchDatabase(t)
			ctx := context.Background()
			if _, err := EnsureSchema(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.pool.Exec(ctx, drift.ddl); err != nil {
				t.Fatal(err)
			}
			if _, err := EnsureSchema(ctx, pool); err == nil || !strings.Contains(err.Error(), drift.named) {
				t.Fatalf("got = %v, want a refusal naming %q", err, drift.named)
			}
			requireNoCheckNamespace(t, pool)
		})
	}
}
