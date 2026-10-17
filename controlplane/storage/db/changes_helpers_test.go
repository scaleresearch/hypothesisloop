package db

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// eventsTestPrefix namespaces the rows these tests insert.
const eventsTestPrefix = "watch-test-"

// eventsTestDB connects to the run's scratch database and skips the test when no database is
// reachable.
func eventsTestDB(t *testing.T) *Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, Config{DSN: testDSN(), MaxConns: 4, MinConns: 1, Timeouts: testTimeouts})
	if err != nil {
		t.Skipf("no database at %s: %v", testDSN(), err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// listenForEvents delivers every change appended after it returns, of any platform experiment,
// until stopped or the test ends.
func listenForEvents(t *testing.T, pool *Pool) (<-chan domain.Event, context.CancelFunc) {
	t.Helper()
	after := cursorNow(t, pool)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events := make(chan domain.Event, 256)
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			rows, err := pool.pool.Query(ctx, `SELECT id, kind, subject, value, detail, platform_experiment_id, agent_id, occurred_at
FROM changes WHERE id > $1 ORDER BY id`, after)
			if err != nil {
				return
			}
			var read []domain.Event
			for rows.Next() {
				var e domain.Event
				if err := rows.Scan(&e.ID, &e.Kind, &e.Subject, &e.Value, &e.Detail, &e.PlatformExperimentID, &e.AgentID, &e.OccurredAt); err != nil {
					rows.Close()
					return
				}
				e.OccurredAt = e.OccurredAt.UTC()
				read = append(read, e)
			}
			rows.Close()
			for _, e := range read {
				select {
				case events <- e:
					after = e.ID
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events, cancel
}

// cursorNow is the newest change id: changesAfter it returns only what is appended later.
func cursorNow(t *testing.T, pool *Pool) int64 {
	t.Helper()
	head, err := NewStore(pool, 3).Head(context.Background())
	if err != nil {
		t.Fatalf("head: got = %v, want = nil", err)
	}
	return head
}

// changesAfter is every change of filter's platform experiment after the id after that filter
// matches, in order.
func changesAfter(t *testing.T, store *Store, filter domain.EventFilter, after int64) []domain.Event {
	t.Helper()
	var matched []domain.Event
	for {
		page, next, caughtUp, err := store.After(context.Background(), filter.PlatformExperimentID, after, 500)
		if err != nil {
			t.Fatalf("changes after %d: got = %v, want = nil", after, err)
		}
		for _, e := range page {
			if filter.Matches(e) {
				matched = append(matched, e)
			}
		}
		if caughtUp {
			return matched
		}
		after = next
	}
}

// awaitEvent returns the first event of the given kind and subject, ignoring everything else on
// the channel: the tests share one database, so a stream carrying another test's rows is the
// ordinary case and not a failure.
func awaitEvent(t *testing.T, events <-chan domain.Event, kind, subject string) domain.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			if e.Kind == kind && e.Subject == subject {
				return e
			}
		case <-deadline:
			t.Fatalf("%s for %s: got = %v, want = one event", kind, subject, "no event")
		}
	}
}

// seedRun inserts an agent, a platform experiment and one comment on a hypothesis, inside one
// transaction, committing or rolling back as asked. The hypothesis itself lives in the graph
// store, so here it is only an id.
func seedRun(t *testing.T, pool *Pool, suffix string, commit bool) (agentID, peID, hypothesisID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := pool.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: got = %v, want = nil", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	agentID = "watch-test-agent-" + suffix
	peID = "watch-test-pe-" + suffix
	hypothesisID = "watch-test-hypothesis-" + suffix
	if _, err := tx.Exec(ctx, `INSERT INTO agents (id, name, org_id) VALUES ($1, $1, 'default')`, agentID); err != nil {
		t.Fatalf("insert agent: got = %v, want = nil", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO platform_experiments (id, project_id, name, budget_accelerator_hours, starts_at, ends_at, default_quota_tier, created_by_kind)
VALUES ($1, 'default', $1, 1, now(), now() + interval '1 hour', 'guaranteed', 'staff')`, peID); err != nil {
		t.Fatalf("insert platform experiment: got = %v, want = nil", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO hypothesis_comments (hypothesis_id, platform_experiment_id, agent_id, actor_kind, actor_id, text)
VALUES ($1, $2, $3, 'agent', $3, 'watch test comment')`, hypothesisID, peID, agentID); err != nil {
		t.Fatalf("insert comment: got = %v, want = nil", err)
	}
	if !commit {
		return agentID, peID, hypothesisID
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: got = %v, want = nil", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		for _, q := range []string{
			`DELETE FROM hypothesis_findings WHERE platform_experiment_id = $1`,
			`DELETE FROM hypothesis_comments WHERE platform_experiment_id = $1`,
			`DELETE FROM baseline_raises WHERE platform_experiment_id = $1`,
			`DELETE FROM experiments WHERE platform_experiment_id = $1`,
			`DELETE FROM platform_experiment_cuts WHERE platform_experiment_id = $1`,
			`DELETE FROM experiment_signups WHERE platform_experiment_id = $1`,
			`DELETE FROM changes WHERE platform_experiment_id = $1`,
			`DELETE FROM platform_experiments WHERE id = $1`,
		} {
			if _, err := pool.pool.Exec(cleanupCtx, q, peID); err != nil {
				t.Errorf("cleanup: got = %v, want = nil", err)
			}
		}
		if _, err := pool.pool.Exec(cleanupCtx, `DELETE FROM agents WHERE id = $1`, agentID); err != nil {
			t.Errorf("cleanup agent: got = %v, want = nil", err)
		}
	})
	return agentID, peID, hypothesisID
}

// jobOf is one QUEUED experiment of the agent waiting for reason, queued now.
func jobOf(agentID, peID, hypothesisID, jobID, reason string) *domain.Experiment {
	now := time.Now()
	return &domain.Experiment{
		ID: jobID, AgentID: agentID, Actor: domain.Actor{Kind: domain.ActorAgent, ID: agentID}, PlatformExperimentID: peID,
		CodeRef: "r@0", ConfigHash: "h", DataRef: "d", HypothesisID: hypothesisID, Hypothesis: "claim",
		AcceleratorType: "test-accelerator", EstimatedDurationHours: 1, EstimatedCostAccH: 1,
		CapacityTier: domain.CapacityGuaranteed, Status: domain.StatusQueued, NotAdmittedReason: reason,
		Job:       domain.JobSpec{AcceleratorType: "test-accelerator"},
		DataKey:   domain.DataKey{AccessKeyID: "AK", SecretAccessKey: "SK"},
		CreatedAt: now, UpdatedAt: now,
	}
}

// queueJob writes exp through the store, as a submit does.
func queueJob(t *testing.T, pool *Pool, exp *domain.Experiment) {
	t.Helper()
	if err := insertExperiment(context.Background(), pool, exp); err != nil {
		t.Fatalf("insert experiment %s: got = %v, want = nil", exp.ID, err)
	}
}

func exec(t *testing.T, pool *Pool, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.pool.Exec(ctx, query, args...); err != nil {
		t.Fatalf("exec %q: got = %v, want = nil", query, err)
	}
}

// launcherAgentsOf is ListLauncherAgents narrowed to the platform experiments peIDs: the launcher
// serves every platform experiment, and the tests share one database.
func launcherAgentsOf(t *testing.T, s *Store, windDown time.Duration, peIDs ...string) []LauncherAgent {
	t.Helper()
	all, err := s.ListLauncherAgents(context.Background(), windDown)
	if err != nil {
		t.Fatalf("list launcher agents: %v", err)
	}
	var out []LauncherAgent
	for _, a := range all {
		if slices.Contains(peIDs, a.PlatformExperimentID) {
			out = append(out, a)
		}
	}
	return out
}

// launcherCoordinatorsOf is ListLauncherCoordinators narrowed to the platform experiments peIDs.
func launcherCoordinatorsOf(t *testing.T, s *Store, windDown time.Duration, peIDs ...string) []LauncherCoordinator {
	t.Helper()
	all, err := s.ListLauncherCoordinators(context.Background(), windDown)
	if err != nil {
		t.Fatalf("list launcher coordinators: %v", err)
	}
	var out []LauncherCoordinator
	for _, c := range all {
		if slices.Contains(peIDs, c.PlatformExperimentID) {
			out = append(out, c)
		}
	}
	return out
}
