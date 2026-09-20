package db

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
