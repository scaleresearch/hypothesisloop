package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Write is one transaction of the store's writes: every statement of a write that takes more
// than one, or that announces a change, runs through it, so the write and its changes commit
// whole or not at all.
type Write struct {
	tx      pgx.Tx
	changes []domain.Change
	err     error
}

func (w *Write) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return w.tx.Exec(ctx, sql, args...)
}

func (w *Write) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return w.tx.Query(ctx, sql, args...)
}

func (w *Write) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return w.tx.QueryRow(ctx, sql, args...)
}

// write runs fn in one transaction, committed when fn returns nil and rolled back otherwise.
func (s *Store) write(ctx context.Context, fn func(*Write) error) error {
	return s.pool.write(ctx, fn)
}

func (p *Pool) write(ctx context.Context, fn func(*Write) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	w := &Write{tx: tx}
	if err := fn(w); err != nil {
		return err
	}
	if err := w.flush(ctx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}
