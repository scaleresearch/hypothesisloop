package db

import (
	"context"
	"io/fs"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
)

// scratchDatabase creates an empty database and drops it afterwards, for tests that build a
// schema of their own.
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
	pool, err := NewPool(ctx, Config{DSN: dsn.String(), MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Fatalf("connect scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// embeddedMigrationsWith is the embedded migrations with the files in extra added or replaced.
func embeddedMigrationsWith(t *testing.T, extra map[string]string) migrations {
	t.Helper()
	files := fstest.MapFS{}
	err := fs.WalkDir(migrationFiles, "migrations", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(migrationFiles, path)
		files[strings.TrimPrefix(path, "migrations/")] = &fstest.MapFile{Data: body}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range extra {
		files[name] = &fstest.MapFile{Data: []byte(body)}
	}
	all, err := loadMigrations(files)
	if err != nil {
		t.Fatal(err)
	}
	return all
}

func ledger(t *testing.T, pool *Pool) []string {
	t.Helper()
	rows, err := pool.pool.Query(context.Background(), `SELECT lpad(version::text, 4, '0') || '_' || name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func files(all migrations) []string {
	var out []string
	for _, m := range all.steps {
		out = append(out, m.file())
	}
	return out
}

// A database built by the idempotent schema.sql every database ran before migrations, as of the
// commit migrations began at.
func legacyDatabase(t *testing.T) *Pool {
	t.Helper()
	pool := scratchDatabase(t)
	legacy, err := os.ReadFile("testdata/schema_7c630417.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.pool.Exec(context.Background(), string(legacy)); err != nil {
		t.Fatalf("build the legacy schema: %v", err)
	}
	return pool
}

func TestAFreshDatabaseGetsEveryMigrationOnce(t *testing.T) {
	pool := scratchDatabase(t)
	ctx := context.Background()
	all := embeddedMigrationsWith(t, nil)
	done, err := Migrate(ctx, pool)
	if err != nil || done.Adopted || !slices.Equal(done.Applied, files(all)) {
		t.Fatalf("first start: got = %+v, %v, want every migration applied", done, err)
	}
	if got := ledger(t, pool); !slices.Equal(got, files(all)) {
		t.Fatalf("schema_migrations: got = %v, want %v", got, files(all))
	}
	if done, err := Migrate(ctx, pool); err != nil || done.Adopted || len(done.Applied) != 0 {
		t.Fatalf("next start: got = %+v, %v, want nothing to do", done, err)
	}
	var org string
	if err := pool.pool.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = 'default'`).Scan(&org); err != nil || org != "default" {
		t.Fatalf("the default project: got = %q, %v, want it in org default", org, err)
	}
}

// A database from before migrations keeps its rows, is recorded at the baseline without running
// it (its DDL would fail on objects that exist), and takes the migrations after it.
func TestADatabaseFromBeforeMigrationsIsAdoptedAtTheBaseline(t *testing.T) {
	pool := legacyDatabase(t)
	ctx := context.Background()
	if _, err := pool.pool.Exec(ctx, `INSERT INTO agents (id, name, org_id) VALUES ('kept', 'Kept', 'default')`); err != nil {
		t.Fatal(err)
	}
	migrations := embeddedMigrationsWith(t, map[string]string{
		"0002_agent_note.sql": `ALTER TABLE agents ADD COLUMN note TEXT NOT NULL DEFAULT 'new';`,
	})
	done, err := migrate(ctx, pool, migrations)
	if err != nil || !done.Adopted || !slices.Equal(done.Applied, []string{"0002_agent_note"}) {
		t.Fatalf("adoption: got = %+v, %v, want adopted and 0002 applied", done, err)
	}
	if got := ledger(t, pool); !slices.Equal(got, []string{"0001_baseline", "0002_agent_note"}) {
		t.Fatalf("schema_migrations: got = %v", got)
	}
	var note string
	if err := pool.pool.QueryRow(ctx, `SELECT note FROM agents WHERE id = 'kept'`).Scan(&note); err != nil || note != "new" {
		t.Fatalf("the row from before: got = %q, %v, want it kept with the new column", note, err)
	}
	var leftover bool
	if err := pool.pool.QueryRow(ctx, `SELECT to_regnamespace($1) IS NOT NULL`, adoptionCheckSchema).Scan(&leftover); err != nil || leftover {
		t.Fatalf("the schema of the adoption check: got = %v, %v, want it rolled back", leftover, err)
	}
	if done, err := migrate(ctx, pool, migrations); err != nil || done.Adopted || len(done.Applied) != 0 {
		t.Fatalf("next start: got = %+v, %v, want nothing to do", done, err)
	}
}

func TestADatabaseShortOfTheBaselineIsNotAdopted(t *testing.T) {
	pool := legacyDatabase(t)
	ctx := context.Background()
	if _, err := pool.pool.Exec(ctx, `ALTER TABLE experiments DROP COLUMN environment_key`); err != nil {
		t.Fatal(err)
	}
	_, err := Migrate(ctx, pool)
	if err == nil || !strings.Contains(err.Error(), "experiments.environment_key text") || !strings.Contains(err.Error(), "git show 7c630417") {
		t.Fatalf("got = %v, want a refusal naming the missing column and how to bring the database to the baseline", err)
	}
	var found bool
	if err := pool.pool.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&found); err != nil || found {
		t.Fatalf("schema_migrations after the refusal: got = %v, %v, want none", found, err)
	}
}

func TestAMigrationChangedAfterItWasAppliedIsRefused(t *testing.T) {
	pool := scratchDatabase(t)
	ctx := context.Background()
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	baseline, err := fs.ReadFile(migrationFiles, "migrations/0001_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	edited := embeddedMigrationsWith(t, map[string]string{"0001_baseline.sql": string(baseline) + "\n-- edited\n"})
	if _, err := migrate(ctx, pool, edited); err == nil || !strings.Contains(err.Error(), "0001_baseline changed after it was applied") {
		t.Fatalf("got = %v, want a refusal naming the changed migration", err)
	}
}

func TestADatabaseNewerThanTheBinaryIsRefused(t *testing.T) {
	pool := scratchDatabase(t)
	ctx := context.Background()
	newer := embeddedMigrationsWith(t, map[string]string{"0002_from_the_future.sql": `CREATE TABLE future (id INTEGER);`})
	if _, err := migrate(ctx, pool, newer); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, pool); err == nil || !strings.Contains(err.Error(), "at migration 0002_from_the_future, newer than") {
		t.Fatalf("got = %v, want a refusal naming the newer migration", err)
	}
}

func TestReplicasStartingAtOnceApplyEachMigrationOnce(t *testing.T) {
	pool := scratchDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	results := make([]Migrated, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = Migrate(ctx, pool)
		}()
	}
	wg.Wait()
	all := files(embeddedMigrationsWith(t, nil))
	for i, err := range errs {
		if err != nil {
			t.Fatalf("replica %d: got = %v, want nil", i, err)
		}
	}
	if applied := len(results[0].Applied) + len(results[1].Applied); applied != len(all) {
		t.Fatalf("applied: got = %+v, want %d migrations between the two", results, len(all))
	}
	if got := ledger(t, pool); !slices.Equal(got, all) {
		t.Fatalf("schema_migrations: got = %v, want %v", got, all)
	}
}

func TestMigrationFilesAreNumberedWithNoGap(t *testing.T) {
	for name, dir := range map[string]fstest.MapFS{
		"a gap":                      {"0001_a.sql": {}, "0003_c.sql": {}},
		"a bad name":                 {"0001_a.sql": {}, "2_b.sql": {}},
		"no 0001":                    {"0002_b.sql": {}},
		"no file":                    {},
		"upper case":                 {"0001_Add.sql": {}},
		"a duplicate":                {"0001_a.sql": {}, "0001_b.sql": {}},
		"a snapshot of no migration": {"0001_a.sql": {}, "snapshots/0002.sql": {}},
		"a snapshot misnamed":        {"0001_a.sql": {}, "snapshots/0001_a.sql": {}},
	} {
		if _, err := loadMigrations(dir); err == nil {
			t.Errorf("%s: got = nil, want an error", name)
		}
	}
}

// schemaFingerprint is every column with its type, nullability and default, every constraint,
// index and trigger of the public schema, each as one line.
func schemaFingerprint(t *testing.T, pool *Pool) []string {
	t.Helper()
	rows, err := pool.pool.Query(context.Background(), `
SELECT 'column ' || table_name || '.' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || COALESCE(column_default, '')
FROM information_schema.columns WHERE table_schema = 'public' AND table_name <> 'schema_migrations'
UNION ALL
SELECT 'constraint ' || conrelid::regclass || ' ' || conname || ' ' || pg_get_constraintdef(oid)
FROM pg_constraint WHERE connamespace = 'public'::regnamespace AND conrelid <> 'schema_migrations'::regclass
UNION ALL
SELECT 'index ' || indexdef FROM pg_indexes WHERE schemaname = 'public' AND tablename <> 'schema_migrations'
UNION ALL
SELECT 'trigger ' || pg_get_triggerdef(t.oid) FROM pg_trigger t
JOIN pg_class c ON c.oid = t.tgrelid WHERE c.relnamespace = 'public'::regnamespace AND NOT t.tgisinternal
UNION ALL
SELECT 'function ' || p.proname || ' ' || md5(pg_get_functiondef(p.oid)) FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	return out
}

func requireSameSchema(t *testing.T, version int, all migrations, snap snapshot) {
	t.Helper()
	ctx := context.Background()
	chain, built := scratchDatabase(t), scratchDatabase(t)
	if _, err := migrate(ctx, chain, migrations{steps: all.steps[:version]}); err != nil {
		t.Fatalf("migrations up to %04d: %v", version, err)
	}
	done, err := migrate(ctx, built, migrations{steps: all.steps[:version], snapshots: []snapshot{snap}})
	if err != nil || done.Snapshot != version {
		t.Fatalf("snapshot %04d: got = %+v, %v", version, done, err)
	}
	want, got := schemaFingerprint(t, chain), schemaFingerprint(t, built)
	for _, line := range want {
		if !slices.Contains(got, line) {
			t.Errorf("snapshot %04d lacks %s", version, line)
		}
	}
	for _, line := range got {
		if !slices.Contains(want, line) {
			t.Errorf("snapshot %04d has %s, which the migrations up to it do not make", version, line)
		}
	}
}

// Every embedded snapshot builds the schema its migrations build one by one, so a fresh
// database and an existing one end alike.
func TestEverySnapshotIsTheSchemaOfItsMigrations(t *testing.T) {
	all := embeddedMigrationsWith(t, nil)
	for _, snap := range all.snapshots {
		requireSameSchema(t, snap.version, all, snap)
	}
}

// A fresh database is built from the newest snapshot and takes the migrations after it; one from
// before migrations is adopted at the baseline and takes every migration after it in order.
func TestAFreshDatabaseStartsFromTheNewestSnapshotAndAnExistingOneConvertsInOrder(t *testing.T) {
	ctx := context.Background()
	baseline, err := fs.ReadFile(migrationFiles, "migrations/0001_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	all := embeddedMigrationsWith(t, map[string]string{
		"0002_agent_note.sql": `ALTER TABLE agents ADD COLUMN note TEXT NOT NULL DEFAULT '';`,
		"0003_agent_rank.sql": `ALTER TABLE agents ADD COLUMN rank INTEGER;`,
		"snapshots/0002.sql":  string(baseline) + "\nALTER TABLE agents ADD COLUMN note TEXT NOT NULL DEFAULT '';\n",
	})
	requireSameSchema(t, 2, all, all.snapshots[0])

	fresh := scratchDatabase(t)
	done, err := migrate(ctx, fresh, all)
	if err != nil || done.Adopted || done.Snapshot != 2 || !slices.Equal(done.Applied, []string{"0003_agent_rank"}) {
		t.Fatalf("fresh: got = %+v, %v, want snapshot 0002 then 0003", done, err)
	}
	if got := ledger(t, fresh); !slices.Equal(got, files(all)) {
		t.Fatalf("fresh schema_migrations: got = %v, want %v", got, files(all))
	}

	existing := legacyDatabase(t)
	done, err = migrate(ctx, existing, all)
	if err != nil || !done.Adopted || done.Snapshot != 0 || !slices.Equal(done.Applied, []string{"0002_agent_note", "0003_agent_rank"}) {
		t.Fatalf("existing: got = %+v, %v, want adopted then 0002 and 0003", done, err)
	}
	if got := ledger(t, existing); !slices.Equal(got, files(all)) {
		t.Fatalf("existing schema_migrations: got = %v, want %v", got, files(all))
	}
}
