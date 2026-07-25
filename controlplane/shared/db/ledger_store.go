package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// LedgerStore provides persistence for domain.CreditLedgerEntry.
type LedgerStore struct {
	pool *Pool
}

// NewLedgerStore creates a LedgerStore backed by pool.
func NewLedgerStore(pool *Pool) *LedgerStore {
	return &LedgerStore{pool: pool}
}

// AppendLedgerEntry inserts a new credit ledger entry.
func (s *LedgerStore) AppendLedgerEntry(ctx context.Context, entry *domain.CreditLedgerEntry) error {
	const q = `
INSERT INTO credit_ledger (id, agent_id, amount, reason, experiment_id, period, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`

	_, err := s.pool.pool.Exec(ctx, q,
		entry.ID, entry.AgentID, entry.Amount, entry.Reason,
		entry.ExperimentID, entry.Period, entry.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("ledger_store.AppendLedgerEntry: %w", err)
	}
	return nil
}

// GetAgentLedger returns all ledger entries for an agent ordered by created_at.
func (s *LedgerStore) GetAgentLedger(ctx context.Context, agentID string) ([]*domain.CreditLedgerEntry, error) {
	const q = `
SELECT id, agent_id, amount, reason, experiment_id, period, created_at
FROM credit_ledger
WHERE agent_id = $1
ORDER BY created_at ASC`

	rows, err := s.pool.pool.Query(ctx, q, agentID)
	if err != nil {
		return nil, fmt.Errorf("ledger_store.GetAgentLedger: %w", err)
	}
	defer rows.Close()

	return scanLedgerEntries(rows)
}

// GetAgentBalance returns the sum of all ledger amounts for the agent in the
// given period.
func (s *LedgerStore) GetAgentBalance(ctx context.Context, agentID string, period int) (float64, error) {
	const q = `
SELECT COALESCE(SUM(amount), 0)
FROM credit_ledger
WHERE agent_id = $1 AND period = $2`

	var balance float64
	if err := s.pool.pool.QueryRow(ctx, q, agentID, period).Scan(&balance); err != nil {
		return 0, fmt.Errorf("ledger_store.GetAgentBalance: %w", err)
	}
	return balance, nil
}

// GetAgentCreditsConsumed returns the total credits spent (sum of debit entries,
// returned as a positive number) by the agent in the given period.
func (s *LedgerStore) GetAgentCreditsConsumed(ctx context.Context, agentID string, period int) (float64, error) {
	const q = `
SELECT COALESCE(-SUM(amount), 0)
FROM credit_ledger
WHERE agent_id = $1 AND period = $2 AND amount < 0`

	var consumed float64
	if err := s.pool.pool.QueryRow(ctx, q, agentID, period).Scan(&consumed); err != nil {
		return 0, fmt.Errorf("ledger_store.GetAgentCreditsConsumed: %w", err)
	}
	return consumed, nil
}

// GetCohortMedianEfficiency returns a neutral efficiency value.
// ML metrics no longer live in Postgres; efficiency scoring is deferred.
func (s *LedgerStore) GetCohortMedianEfficiency(_ context.Context, _ int) (float64, error) {
	return 1.0, nil
}

func scanLedgerEntries(rows pgx.Rows) ([]*domain.CreditLedgerEntry, error) {
	var out []*domain.CreditLedgerEntry
	for rows.Next() {
		e := &domain.CreditLedgerEntry{}
		if err := rows.Scan(
			&e.ID, &e.AgentID, &e.Amount, &e.Reason,
			&e.ExperimentID, &e.Period, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan ledger entry: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
