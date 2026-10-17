package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// changesLockKey serialises the appends to changes: held from before an append takes its ids
// until it commits, so ids commit in order and a reader that has seen id N has seen every id
// below it that will ever commit.
const changesLockKey = 72903

const changesChannel = "changes"

const pruneBatch = 10000

var (
	// ErrChangeUnknown is an after beyond the newest change: no id this platform issued.
	ErrChangeUnknown = errors.New("not a change id this platform issued")
	// ErrChangesPruned is an after older than the changes still held.
	ErrChangesPruned = errors.New("changes are no longer held")
)

// Change announces c once the write commits, and with it. A change that does not validate fails
// the write.
func (w *Write) Change(c domain.Change) {
	if err := c.Validate(); err != nil {
		w.err = errors.Join(w.err, err)
		return
	}
	c.Detail = domain.EventDetail(c.Detail)
	w.changes = append(w.changes, c)
}

// ChangeUnlessAnnounced appends c unless the newest change of its kind, subject and detail already
// says its value: the retry of a graph write whose earlier Postgres transaction may have aborted
// after the graph committed. A concurrent retry may announce it twice, which is harmless.
func (w *Write) ChangeUnlessAnnounced(ctx context.Context, c domain.Change) error {
	var value string
	err := w.tx.QueryRow(ctx, `SELECT value FROM changes
WHERE platform_experiment_id = $1 AND kind = $2 AND subject = $3 AND detail = $4
ORDER BY id DESC LIMIT 1`, c.PlatformExperimentID, c.Kind, c.Subject, domain.EventDetail(c.Detail)).Scan(&value)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("changes.ChangeUnlessAnnounced: %w", err)
	}
	if err == nil && value == c.Value {
		return nil
	}
	w.Change(c)
	return nil
}

// flush appends the write's changes and wakes every listener, the last statements before commit.
func (w *Write) flush(ctx context.Context) error {
	if w.err != nil {
		return w.err
	}
	if len(w.changes) == 0 {
		return nil
	}
	if _, err := w.tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, changesLockKey); err != nil {
		return fmt.Errorf("changes.flush: lock: %w", err)
	}
	cols := make([][]string, 6)
	for _, c := range w.changes {
		for i, v := range []string{c.PlatformExperimentID, c.AgentID, c.Kind, c.Subject, c.Value, c.Detail} {
			cols[i] = append(cols[i], v)
		}
	}
	if _, err := w.tx.Exec(ctx, `INSERT INTO changes (platform_experiment_id, agent_id, kind, subject, value, detail)
SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[])`,
		cols[0], cols[1], cols[2], cols[3], cols[4], cols[5]); err != nil {
		return fmt.Errorf("changes.flush: insert: %w", err)
	}
	if _, err := w.tx.Exec(ctx, `SELECT pg_notify($1, '')`, changesChannel); err != nil {
		return fmt.Errorf("changes.flush: notify: %w", err)
	}
	return nil
}

// Around runs a write to another store inside a Postgres write that holds platformExperimentID
// open until it commits, so fn's changes commit with it or not at all.
func (s *Store) Around(ctx context.Context, platformExperimentID string, fn func(*Write) error) error {
	return s.write(ctx, func(w *Write) error {
		if err := holdOpenPlatformExperiment(ctx, w, platformExperimentID); err != nil {
			return err
		}
		return fn(w)
	})
}

// After reads, in one snapshot, up to limit changes of platformExperimentID with an id above
// after. next is where to read on from: the last id read when the page is full, else the newest
// id of any platform experiment, and caughtUp says nothing more is held. An after above the
// newest id is ErrChangeUnknown, one below the oldest held ErrChangesPruned.
func (s *Store) After(ctx context.Context, platformExperimentID string, after int64, limit int) (changes []domain.Event, next int64, caughtUp bool, err error) {
	tx, err := s.pool.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, false, fmt.Errorf("changes.After: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var oldest, head int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(min(id), 0), COALESCE(max(id), 0) FROM changes`).Scan(&oldest, &head); err != nil {
		return nil, 0, false, fmt.Errorf("changes.After: bounds: %w", err)
	}
	if after > head {
		return nil, 0, false, fmt.Errorf("%w: %d", ErrChangeUnknown, after)
	}
	if after > 0 && after < oldest-1 {
		return nil, 0, false, fmt.Errorf("%w: changes after %d", ErrChangesPruned, after)
	}
	rows, err := tx.Query(ctx, `SELECT id, kind, subject, value, detail, agent_id, occurred_at FROM changes
WHERE platform_experiment_id = $1 AND id > $2 ORDER BY id LIMIT $3`, platformExperimentID, after, limit)
	if err != nil {
		return nil, 0, false, fmt.Errorf("changes.After: %w", err)
	}
	changes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Event, error) {
		e := domain.Event{PlatformExperimentID: platformExperimentID}
		err := row.Scan(&e.ID, &e.Kind, &e.Subject, &e.Value, &e.Detail, &e.AgentID, &e.OccurredAt)
		e.OccurredAt = e.OccurredAt.UTC()
		return e, err
	})
	if err != nil {
		return nil, 0, false, fmt.Errorf("changes.After: %w", err)
	}
	if len(changes) == limit {
		return changes, changes[len(changes)-1].ID, false, nil
	}
	return changes, head, true, nil
}

// Head is the newest change id, 0 while there is none.
func (s *Store) Head(ctx context.Context) (int64, error) {
	var head int64
	if err := s.pool.pool.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM changes`).Scan(&head); err != nil {
		return 0, fmt.Errorf("changes.Head: %w", err)
	}
	return head, nil
}

// Prune deletes the changes older than retention in batches, keeping the newest so the head
// survives, and returns how many it deleted.
func (s *Store) Prune(ctx context.Context, retention time.Duration) (int64, error) {
	var deleted int64
	for {
		tag, err := s.pool.pool.Exec(ctx, `DELETE FROM changes WHERE id IN (
SELECT id FROM changes WHERE occurred_at < now() - $1::interval AND id < (SELECT max(id) FROM changes) ORDER BY id LIMIT $2)`,
			retention, pruneBatch)
		if err != nil {
			return deleted, fmt.Errorf("changes.Prune: %w", err)
		}
		deleted += tag.RowsAffected()
		if tag.RowsAffected() < pruneBatch {
			return deleted, nil
		}
	}
}

// Listen holds one dedicated connection on LISTEN and calls woke once LISTEN is in effect and on
// every append after it, until ctx ends or the connection breaks, returning why.
func (s *Store) Listen(ctx context.Context, woke func()) error {
	acquired, err := s.pool.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("changes.Listen: acquire: %w", err)
	}
	conn := acquired.Hijack()
	defer conn.Close(context.Background()) //nolint:errcheck
	if _, err := conn.Exec(ctx, "LISTEN "+changesChannel); err != nil {
		return fmt.Errorf("changes.Listen: %w", err)
	}
	woke()
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return fmt.Errorf("changes.Listen: wait: %w", err)
		}
		woke()
	}
}
