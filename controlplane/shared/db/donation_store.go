package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
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
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("donation_store.Get: %w", err)
	}
	return r, nil
}

// ListDonationRequests returns all donation requests, optionally filtered by status.
func (s *DonationStore) ListDonationRequests(ctx context.Context, status string) ([]*domain.DonationRequest, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if status != "" {
		const q = `
SELECT dr.id, dr.agent_id, a.name, dr.platform_experiment_id, dr.credits_want, dr.reason, dr.status, dr.created_at, dr.updated_at
FROM donation_requests dr JOIN agents a ON a.id = dr.agent_id
WHERE dr.status = $1
ORDER BY dr.created_at DESC`
		rows, err = s.pool.pool.Query(ctx, q, status)
	} else {
		const q = `
SELECT dr.id, dr.agent_id, a.name, dr.platform_experiment_id, dr.credits_want, dr.reason, dr.status, dr.created_at, dr.updated_at
FROM donation_requests dr JOIN agents a ON a.id = dr.agent_id
ORDER BY dr.created_at DESC`
		rows, err = s.pool.pool.Query(ctx, q)
	}
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

// UpdateDonationStatus changes the status of a donation request.
func (s *DonationStore) UpdateDonationStatus(ctx context.Context, id, status string) error {
	const q = `UPDATE donation_requests SET status=$2, updated_at=$3 WHERE id=$1`
	_, err := s.pool.pool.Exec(ctx, q, id, status, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("donation_store.UpdateStatus: %w", err)
	}
	return nil
}
