package db

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

//go:embed migrations
var migrationFiles embed.FS

// schemaMigrationLockKey is a fixed, arbitrary lock key shared by Migrate and the seeding that
// follows it: the number means nothing, it only has to be the same everywhere so every caller
// contends on the same lock.
const schemaMigrationLockKey = 8125_2026

// adoptionCheckSchema is where the baseline is built, inside a transaction that is rolled back,
// to compare a database from before migrations against it.
const adoptionCheckSchema = "hypothesisloop_adoption_check"

// legacySchemaRemedy is how a database from before migrations reaches the schema the baseline
// adopts: the last idempotent schema.sql, applied once by hand.
const legacySchemaRemedy = `git show 7c630417:controlplane/shared/db/schema.sql | psql "$DATABASE_URL"`

var (
	migrationFileName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)
	snapshotFileName  = regexp.MustCompile(`^(\d{4})\.sql$`)
)

type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

// snapshot is the schema at a version, built in one step: a fresh database runs the newest one
// in place of the migrations up to it, which an existing database runs in order.
type snapshot struct {
	version int
	sql     string
}

type migrations struct {
	steps     []migration
	snapshots []snapshot
}

func (m migration) file() string { return fmt.Sprintf("%04d_%s", m.version, m.name) }

type appliedMigration struct {
	version  int
	name     string
	checksum string
}

// Migrated is what Migrate did: whether it adopted a database built before migrations at the
// baseline, the snapshot it built a fresh database from (0 for none), the applied migrations this
// binary does not know (a later release's), and the migrations it ran, in order.
type Migrated struct {
	Adopted  bool
	Snapshot int
	Unknown  []string
	Applied  []string
}

// Migrate brings the database to the newest migration this binary embeds. Each pending migration
// runs in its own transaction and is recorded in schema_migrations with the checksum of its file.
// It refuses to start when an applied migration's file has changed since. A database with
// migrations this binary does not know is a later release's, which keeps this one working
// (expand/contract): they are reported in Unknown and left alone. A fresh database is built from the newest snapshot
// (migrations/snapshots/NNNN.sql) when there is one, recorded as having run the migrations up to
// it, and takes the ones after it. A database with the schema and no schema_migrations was built
// by the idempotent schema.sql that came before: it is checked against the baseline and recorded
// as at it, without running it. Advisory-locked, so replicas starting at once apply each
// migration once. Called once at startup by control-service alone.
func Migrate(ctx context.Context, pool *Pool) (Migrated, error) {
	dir, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return Migrated{}, fmt.Errorf("db.Migrate: %w", err)
	}
	all, err := loadMigrations(dir)
	if err != nil {
		return Migrated{}, err
	}
	return migrate(ctx, pool, all)
}

func loadMigrations(dir fs.FS) (migrations, error) {
	var all migrations
	entries, err := fs.ReadDir(dir, ".")
	if err != nil {
		return all, fmt.Errorf("db.Migrate: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() == "snapshots" {
			continue
		}
		parts := migrationFileName.FindStringSubmatch(e.Name())
		if parts == nil {
			return all, fmt.Errorf("db.Migrate: migration file %s is not named NNNN_name.sql", e.Name())
		}
		version, _ := strconv.Atoi(parts[1])
		if want := len(all.steps) + 1; version != want {
			return all, fmt.Errorf("db.Migrate: migration file %s: want version %04d, migrations are numbered from 0001 with no gap", e.Name(), want)
		}
		body, err := fs.ReadFile(dir, e.Name())
		if err != nil {
			return all, fmt.Errorf("db.Migrate: %w", err)
		}
		sum := sha256.Sum256(body)
		all.steps = append(all.steps, migration{version: version, name: parts[2], sql: string(body), checksum: hex.EncodeToString(sum[:])})
	}
	if len(all.steps) == 0 {
		return all, fmt.Errorf("db.Migrate: no migrations embedded")
	}
	snapshots, err := fs.ReadDir(dir, "snapshots")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return all, fmt.Errorf("db.Migrate: %w", err)
	}
	for _, e := range snapshots {
		parts := snapshotFileName.FindStringSubmatch(e.Name())
		if parts == nil {
			return all, fmt.Errorf("db.Migrate: snapshot file %s is not named NNNN.sql", e.Name())
		}
		version, _ := strconv.Atoi(parts[1])
		if version < 1 || version > len(all.steps) {
			return all, fmt.Errorf("db.Migrate: snapshot %s is of no migration there is", e.Name())
		}
		body, err := fs.ReadFile(dir, "snapshots/"+e.Name())
		if err != nil {
			return all, fmt.Errorf("db.Migrate: %w", err)
		}
		all.snapshots = append(all.snapshots, snapshot{version: version, sql: string(body)})
	}
	return all, nil
}

func migrate(ctx context.Context, pool *Pool, all migrations) (Migrated, error) {
	migrations := all.steps
	var done Migrated
	conn, err := pool.pool.Acquire(ctx)
	if err != nil {
		return done, fmt.Errorf("db.Migrate: acquire connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", schemaMigrationLockKey); err != nil {
		return done, fmt.Errorf("db.Migrate: acquire advisory lock: %w", err)
	}
	// A fresh context, not the caller's: unlocking with an expired one fails silently and leaves
	// the lock held on this connection for as long as it stays in the pool, wedging every later
	// Migrate.
	defer func() {
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer unlockCancel()
		conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", schemaMigrationLockKey) //nolint:errcheck
	}()

	var ledger, schema bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL, to_regclass('platform_experiments') IS NOT NULL`).
		Scan(&ledger, &schema); err != nil {
		return done, fmt.Errorf("db.Migrate: %w", err)
	}
	if !ledger {
		if schema {
			if err := checkAdoptable(ctx, conn, migrations[0]); err != nil {
				return done, err
			}
			done.Adopted = true
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `CREATE TABLE schema_migrations (
    version    INTEGER     PRIMARY KEY,
    name       TEXT        NOT NULL,
    checksum   TEXT        NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
				return err
			}
			if done.Adopted {
				return recordMigration(ctx, tx, migrations[0])
			}
			if len(all.snapshots) == 0 {
				return nil
			}
			newest := all.snapshots[len(all.snapshots)-1]
			if _, err := tx.Exec(ctx, newest.sql); err != nil {
				return fmt.Errorf("snapshot %04d: %w", newest.version, err)
			}
			for _, m := range migrations[:newest.version] {
				if err := recordMigration(ctx, tx, m); err != nil {
					return err
				}
			}
			done.Snapshot = newest.version
			return nil
		})
		if err != nil {
			return done, fmt.Errorf("db.Migrate: start the ledger: %w", err)
		}
	}

	rows, err := conn.Query(ctx, `SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return done, fmt.Errorf("db.Migrate: %w", err)
	}
	applied, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (appliedMigration, error) {
		var a appliedMigration
		return a, r.Scan(&a.version, &a.name, &a.checksum)
	})
	if err != nil {
		return done, fmt.Errorf("db.Migrate: %w", err)
	}
	done.Unknown, err = checkApplied(applied, migrations)
	if err != nil {
		return done, err
	}
	for _, m := range migrations[min(len(applied), len(migrations)):] {
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			return recordMigration(ctx, tx, m)
		})
		if err != nil {
			return done, fmt.Errorf("db.Migrate: %s: %w", m.file(), err)
		}
		done.Applied = append(done.Applied, m.file())
	}
	return done, nil
}

func recordMigration(ctx context.Context, tx pgx.Tx, m migration) error {
	_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`, m.version, m.name, m.checksum)
	return err
}

// checkApplied refuses a ledger with a gap or an applied migration whose file changed, and
// returns the applied migrations this binary does not know: a later release's, which this one
// runs against during a rollout.
func checkApplied(applied []appliedMigration, migrations []migration) ([]string, error) {
	var unknown []string
	for i, a := range applied {
		if a.version != i+1 {
			return nil, fmt.Errorf("db.Migrate: schema_migrations has %04d_%s where version %04d belongs", a.version, a.name, i+1)
		}
		if i >= len(migrations) {
			unknown = append(unknown, fmt.Sprintf("%04d_%s", a.version, a.name))
			continue
		}
		if m := migrations[i]; a.checksum != m.checksum {
			return nil, fmt.Errorf("db.Migrate: migration %s changed after it was applied (checksum %s, applied as %s): an applied migration is never edited, add a new one",
				m.file(), m.checksum, a.checksum)
		}
	}
	return unknown, nil
}

// checkAdoptable builds the baseline in a schema of its own inside a transaction it rolls back,
// and refuses unless every column it defines is in the database with the same type and
// nullability.
func checkAdoptable(ctx context.Context, conn *pgxpool.Conn, baseline migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db.Migrate: adoption check: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var current string
	if err := tx.QueryRow(ctx, `SELECT current_schema()`).Scan(&current); err != nil {
		return fmt.Errorf("db.Migrate: adoption check: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE SCHEMA `+adoptionCheckSchema+`; SET LOCAL search_path TO `+adoptionCheckSchema); err != nil {
		return fmt.Errorf("db.Migrate: adoption check: %w", err)
	}
	if _, err := tx.Exec(ctx, baseline.sql); err != nil {
		return fmt.Errorf("db.Migrate: adoption check: build %s: %w", baseline.file(), err)
	}
	rows, err := tx.Query(ctx, `
SELECT table_name || '.' || column_name || ' ' || data_type || CASE is_nullable WHEN 'NO' THEN ' NOT NULL' ELSE '' END
FROM information_schema.columns WHERE table_schema = $1
EXCEPT
SELECT table_name || '.' || column_name || ' ' || data_type || CASE is_nullable WHEN 'NO' THEN ' NOT NULL' ELSE '' END
FROM information_schema.columns WHERE table_schema = $2
ORDER BY 1`, adoptionCheckSchema, current)
	if err != nil {
		return fmt.Errorf("db.Migrate: adoption check: %w", err)
	}
	missing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("db.Migrate: adoption check: %w", err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("db.Migrate: the database has a schema and no schema_migrations, and it is not the schema %s adopts: it lacks %s. Bring it to that schema with %s, then start again",
			baseline.file(), strings.Join(missing, ", "), legacySchemaRemedy)
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
	r, err := tx.Query(ctx, `SELECT id, COALESCE(agent_id, ''), author, platform_experiment_id, text,
       status::text, created_at, status_changed_at FROM hypotheses ORDER BY created_at`)
	if err != nil {
		return fmt.Errorf("db.MoveLegacyHypotheses: %w", err)
	}
	var rows []*domain.Hypothesis
	for r.Next() {
		h := &domain.Hypothesis{}
		if err := r.Scan(&h.ID, &h.AgentID, &h.Actor.Name, &h.PlatformExperimentID, &h.Text,
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

// humanWritesLockKey serialises MoveHumanWrites across control-service replicas.
const humanWritesLockKey = int64(0x68756d616e7772)

// MoveHumanWrites gives the records written before actors were kept the account human_writes
// holds for them, then drops the table. A comment it names becomes the user's here, and the rows
// for hypotheses and links go to creditGraph, which does the same in the graph. Every other old
// record stays unverified: no account is inferred from a name.
//
// It does nothing once the table is gone. creditGraph runs before the drop commits and sets an
// actor only where none is kept, so a start interrupted between the two repeats it harmlessly.
func MoveHumanWrites(ctx context.Context, pool *Pool, creditGraph func([]domain.HumanWrite) error) error {
	tx, err := pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db.MoveHumanWrites: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", humanWritesLockKey); err != nil {
		return fmt.Errorf("db.MoveHumanWrites: lock: %w", err)
	}
	var found bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('human_writes') IS NOT NULL`).Scan(&found); err != nil {
		return fmt.Errorf("db.MoveHumanWrites: %w", err)
	}
	if !found {
		return nil
	}
	if _, err := tx.Exec(ctx, `
UPDATE hypothesis_comments c SET actor_kind = 'user', actor_id = w.user_id
FROM human_writes w WHERE w.kind = 'comment' AND w.subject = c.id AND c.actor_kind = 'unverified'`); err != nil {
		return fmt.Errorf("db.MoveHumanWrites: comments: %w", err)
	}
	r, err := tx.Query(ctx, `SELECT user_id, kind, subject FROM human_writes WHERE kind IN ('hypothesis', 'link') ORDER BY id`)
	if err != nil {
		return fmt.Errorf("db.MoveHumanWrites: %w", err)
	}
	var writes []domain.HumanWrite
	for r.Next() {
		var w domain.HumanWrite
		if err := r.Scan(&w.UserID, &w.Kind, &w.Subject); err != nil {
			r.Close()
			return fmt.Errorf("db.MoveHumanWrites: scan: %w", err)
		}
		writes = append(writes, w)
	}
	r.Close()
	if err := r.Err(); err != nil {
		return fmt.Errorf("db.MoveHumanWrites: %w", err)
	}
	if err := creditGraph(writes); err != nil {
		return fmt.Errorf("db.MoveHumanWrites: graph: %w", err)
	}
	if _, err := tx.Exec(ctx, `DROP TABLE human_writes`); err != nil {
		return fmt.Errorf("db.MoveHumanWrites: drop: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db.MoveHumanWrites: commit: %w", err)
	}
	return nil
}
