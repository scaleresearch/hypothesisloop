package db

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Two replicas starting against a database that still has the legacy table: one imports and drops
// it, the other waits and then finds nothing to move.
func TestMoveLegacyHypothesesRunsOnceAcrossConcurrentStarts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, Config{DSN: testDSN(), MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Skipf("no database at %s: %v", testDSN(), err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(func() {
		pool.pool.Exec(context.Background(), `DROP TABLE IF EXISTS hypotheses CASCADE`) //nolint:errcheck
	})
	if _, err := pool.pool.Exec(ctx, `CREATE TABLE hypotheses (id TEXT PRIMARY KEY, agent_id TEXT, source TEXT NOT NULL,
  author TEXT NOT NULL, platform_experiment_id TEXT NOT NULL, text TEXT NOT NULL, status TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL, status_changed_at TIMESTAMPTZ)`); err != nil {
		t.Fatalf("create legacy table: got = %v, want nil", err)
	}
	if _, err := pool.pool.Exec(ctx, `INSERT INTO hypotheses VALUES
  ('legacy-1', 'agent-1', 'agent', '', 'pe-legacy', 'one', 'open', now(), NULL),
  ('legacy-2', NULL, 'human', 'Ada', 'pe-legacy', 'two', 'refuted', now(), now())`); err != nil {
		t.Fatalf("insert legacy rows: got = %v, want nil", err)
	}

	var imports atomic.Int32
	var imported []*domain.Hypothesis
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = MoveLegacyHypotheses(ctx, pool, func(rows []*domain.Hypothesis) error {
				imports.Add(1)
				imported = rows
				time.Sleep(200 * time.Millisecond)
				return nil
			})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("MoveLegacyHypotheses %d: got = %v, want nil", i, err)
		}
	}
	if n := imports.Load(); n != 1 || len(imported) != 2 {
		t.Fatalf("imports: got = (%d calls, %d rows), want (1, 2)", n, len(imported))
	}
	var found bool
	if err := pool.pool.QueryRow(ctx, `SELECT to_regclass('hypotheses') IS NOT NULL`).Scan(&found); err != nil || found {
		t.Errorf("legacy table after the move: got = (%v, %v), want gone", found, err)
	}
}

func TestNewPoolRequiresAnExplicitMaxConns(t *testing.T) {
	if _, err := NewPool(context.Background(), Config{DSN: testDSN()}); err == nil {
		t.Fatal("NewPool without MaxConns: got = nil, want an error")
	}
}

// A database from before records kept their actor: human_writes names the user behind some of
// them. The move makes those the user's, leaves every other old record unverified, hands the
// graph its share once, and drops the table, however many replicas start at once.
func TestMoveHumanWritesCreditsTheRecordsItNamesAndDropsTheTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := eventsTestDB(t)
	suffix := uuid.New().String()
	peID, agentID, userID := "pe-writes-"+suffix, "agent-writes-"+suffix, "user-writes-"+suffix
	createTestPE(t, pool, peID, 10)
	createTestAgent(t, pool, agentID)
	t.Cleanup(func() {
		pool.pool.Exec(context.Background(), `DROP TABLE IF EXISTS human_writes`) //nolint:errcheck
	})
	for _, q := range []string{
		`DROP TABLE IF EXISTS human_writes`,
		`CREATE TABLE human_writes (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, user_id TEXT NOT NULL, author TEXT NOT NULL, kind TEXT NOT NULL,
    subject TEXT NOT NULL, platform_experiment_id TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
	} {
		if _, err := pool.pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	comment := func(id, agent, source, author, actorKind, actorID string) {
		t.Helper()
		if _, err := pool.pool.Exec(ctx, `
INSERT INTO hypothesis_comments (id, hypothesis_id, platform_experiment_id, agent_id, source, author, actor_kind, actor_id, text)
VALUES ($1, 'hyp-1', $2, NULLIF($3, ''), $4, $5, $6, $7, 'note')`, id, peID, agent, source, author, actorKind, actorID); err != nil {
			t.Fatalf("insert comment %s: %v", id, err)
		}
	}
	comment("c-person-"+suffix, "", "human", "Ada Lovelace", "unverified", "")
	comment("c-typed-"+suffix, "", "human", "Mallory", "unverified", "")
	comment("c-agent-"+suffix, agentID, "agent", "", "unverified", "")
	comment("c-new-"+suffix, "", "human", "", "key", "key-1")
	link := "hyp-a related hyp-b"
	if _, err := pool.pool.Exec(ctx, `INSERT INTO human_writes (user_id, author, kind, subject, platform_experiment_id) VALUES
  ($1, 'Ada Lovelace', 'comment', $2, $5), ($1, 'Ada Lovelace', 'comment', $3, $5),
  ($1, 'Ada Lovelace', 'hypothesis', 'hyp-a', $5), ($1, 'Ada Lovelace', 'link', $4, $5)`,
		userID, "c-person-"+suffix, "c-new-"+suffix, link, peID); err != nil {
		t.Fatalf("insert human writes: %v", err)
	}

	var calls atomic.Int32
	var handed []domain.HumanWrite
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = MoveHumanWrites(ctx, pool, func(writes []domain.HumanWrite) error {
				calls.Add(1)
				handed = writes
				time.Sleep(200 * time.Millisecond)
				return nil
			})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("MoveHumanWrites %d: got = %v, want nil", i, err)
		}
	}
	want := []domain.HumanWrite{{UserID: userID, Kind: "hypothesis", Subject: "hyp-a"}, {UserID: userID, Kind: "link", Subject: link}}
	if calls.Load() != 1 || !slices.Equal(handed, want) {
		t.Fatalf("handed to the graph: got = %+v in %d calls, want %+v once", handed, calls.Load(), want)
	}
	var found bool
	if err := pool.pool.QueryRow(ctx, `SELECT to_regclass('human_writes') IS NOT NULL`).Scan(&found); err != nil || found {
		t.Errorf("human_writes after the move: got = (%v, %v), want gone", found, err)
	}
	for id, wantActor := range map[string]domain.Actor{
		"c-person-" + suffix: {Kind: domain.ActorUser, ID: userID},
		"c-typed-" + suffix:  {Kind: domain.ActorUnverified},
		"c-agent-" + suffix:  {Kind: domain.ActorUnverified},
		"c-new-" + suffix:    {Kind: domain.ActorKey, ID: "key-1"},
	} {
		var got domain.Actor
		if err := pool.pool.QueryRow(ctx, `SELECT actor_kind, actor_id FROM hypothesis_comments WHERE id = $1`, id).Scan(&got.Kind, &got.ID); err != nil || got != wantActor {
			t.Errorf("comment %s: got = %+v, %v, want %+v", id, got, err, wantActor)
		}
	}
	if err := MoveHumanWrites(ctx, pool, func([]domain.HumanWrite) error {
		t.Error("the graph was asked again after the table was gone")
		return nil
	}); err != nil {
		t.Errorf("a later start: got = %v, want nil", err)
	}
}
