package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// DonationStore provides persistence for donation_requests.
type DonationStore struct {
	pool *Pool
}

// NewDonationStore creates a DonationStore backed by pool.
func NewDonationStore(pool *Pool) *DonationStore {
	return &DonationStore{pool: pool}
}

// CreateDonationRequest inserts a new open donation request.
func (s *DonationStore) CreateDonationRequest(ctx context.Context, req *domain.DonationRequest) error {
	const q = `
INSERT INTO donation_requests (id, agent_id, platform_experiment_id, credits_want, reason, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	_, err := s.pool.pool.Exec(ctx, q,
		req.ID, req.AgentID, req.PlatformExperimentID, req.CreditsWant, req.Reason, req.Status, req.CreatedAt, req.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("donation_store.Create: %w", err)
	}
	return nil
}

// GetDonationRequest returns a single donation request by ID, or nil if not found.
func (s *DonationStore) GetDonationRequest(ctx context.Context, id string) (*domain.DonationRequest, error) {
	const q = `
SELECT dr.id, dr.agent_id, a.name, dr.platform_experiment_id, dr.credits_want, dr.reason, dr.status, dr.created_at, dr.updated_at
FROM donation_requests dr JOIN agents a ON a.id = dr.agent_id
WHERE dr.id = $1`
	r := &domain.DonationRequest{}
	err := s.pool.pool.QueryRow(ctx, q, id).Scan(
		&r.ID, &r.AgentID, &r.AgentName, &r.PlatformExperimentID, &r.CreditsWant, &r.Reason, &r.Status, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("donation_store.Get: %w", err)
	}
	return r, nil
}

// defaultDonationListLimit and maxDonationListLimit bound every ListDonationRequests read the
// same way ListAgents/ListHypotheses bound their own — donation_requests only ever grows, so
// limit<=0 must not mean "unbounded".
// The default is deliberately far below the maximum. Every caller of this API is an autonomous
// agent whose whole response lands in a bounded context window, so a list that answers "here is
// everything" hands it a truncated or poisoned context and no way to recover. A caller that
// genuinely wants a large page asks for one; a caller that does not think about it gets a page it
// can read, plus the exact total in X-Total-Count telling it what it has not seen.
const (
	defaultDonationListLimit = 20
	maxDonationListLimit     = 200
)

// ListDonationRequests returns one page of donation requests, most recent first, optionally
// filtered by status. limit is defaulted and clamped to [1, maxDonationListLimit].
func (s *DonationStore) ListDonationRequests(ctx context.Context, status string, limit, offset int) ([]*domain.DonationRequest, error) {
	if limit <= 0 {
		limit = defaultDonationListLimit
	} else if limit > maxDonationListLimit {
		limit = maxDonationListLimit
	}
	if offset < 0 {
		offset = 0
	}

	q := `
SELECT dr.id, dr.agent_id, a.name, dr.platform_experiment_id, dr.credits_want, dr.reason, dr.status, dr.created_at, dr.updated_at
FROM donation_requests dr JOIN agents a ON a.id = dr.agent_id
`
	args := []any{}
	if status != "" {
		q += "WHERE dr.status = $1\n"
		args = append(args, status)
	}
	q += fmt.Sprintf("ORDER BY dr.created_at DESC LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
	args = append(args, limit, offset)

	rows, err := s.pool.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("donation_store.List: %w", err)
	}
	defer rows.Close()

	var out []*domain.DonationRequest
	for rows.Next() {
		r := &domain.DonationRequest{}
		if err := rows.Scan(&r.ID, &r.AgentID, &r.AgentName, &r.PlatformExperimentID, &r.CreditsWant, &r.Reason, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("donation_store.List scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateDonationStatus moves a donation request from 'open' to status. It is a CAS: fulfilled
// (or already-cancelled) donations cannot be overwritten, and a nonexistent ID returns an error
// instead of a silent no-op.
func (s *DonationStore) UpdateDonationStatus(ctx context.Context, id, status string) error {
	const q = `UPDATE donation_requests SET status=$2, updated_at=$3 WHERE id=$1 AND status='open'`
	tag, err := s.pool.pool.Exec(ctx, q, id, status, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("donation_store.UpdateStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("donation_store.UpdateStatus: donation %s not found or not open", id)
	}
	return nil
}

// CountDonationRequests returns how many donation requests match status (all of them when
// status is empty), ignoring limit/offset — the X-Total-Count a paginating caller shows.
func (s *DonationStore) CountDonationRequests(ctx context.Context, status string) (int, error) {
	q := `SELECT COUNT(*) FROM donation_requests`
	args := []any{}
	if status != "" {
		q += ` WHERE status = $1`
		args = append(args, status)
	}
	var n int
	if err := s.pool.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("donation_store.Count: %w", err)
	}
	return n, nil
}
