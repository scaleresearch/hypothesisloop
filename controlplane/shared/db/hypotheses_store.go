package db

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// HypothesesStore provides persistence for domain.Hypothesis.
type HypothesesStore struct {
	pool *Pool
}

// NewHypothesesStore creates a HypothesesStore backed by pool.
func NewHypothesesStore(pool *Pool) *HypothesesStore {
	return &HypothesesStore{pool: pool}
}

const hypothesisColumns = `id, agent_id, text, created_at`

// newHypothesisID generates a UUIDv7-formatted string, matching the ID scheme used
// elsewhere for agent-visible entities (see registry.newUUIDv7).
func newHypothesisID() (string, error) {
	var rnd [10]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	ms := uint64(time.Now().UnixMilli())
	var b [16]byte
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	rand12 := binary.BigEndian.Uint16(rnd[0:2]) & 0x0fff
	binary.BigEndian.PutUint16(b[6:8], 0x7000|rand12)
	copy(b[8:], rnd[2:])
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// FindOrCreateHypothesis registers a hypothesis, or returns the existing row if one with
// equivalent normalized text already exists — this is the real uniqueness check: it relies
// on the DB's UNIQUE index on normalized_text (schema.sql) rather than any in-process
// novelty heuristic. Returns (hypothesis, alreadyExisted, error).
func (s *HypothesesStore) FindOrCreateHypothesis(ctx context.Context, agentID, text string) (*domain.Hypothesis, bool, error) {
	normalized := domain.NormalizeHypothesisText(text)
	if normalized == "" {
		return nil, false, fmt.Errorf("hypotheses_store.FindOrCreateHypothesis: text is empty")
	}

	if existing, err := s.FindByNormalizedText(ctx, normalized); err != nil {
		return nil, false, err
	} else if existing != nil {
		return existing, true, nil
	}

	id, err := newHypothesisID()
	if err != nil {
		return nil, false, fmt.Errorf("hypotheses_store.FindOrCreateHypothesis: %w", err)
	}
	h := &domain.Hypothesis{
		ID:        id,
		AgentID:   agentID,
		Text:      text,
		CreatedAt: time.Now().UTC(),
	}

	const q = `
INSERT INTO hypotheses (id, agent_id, text, normalized_text, created_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (normalized_text) DO NOTHING`
	tag, err := s.pool.pool.Exec(ctx, q, h.ID, h.AgentID, h.Text, normalized, h.CreatedAt)
	if err != nil {
		return nil, false, fmt.Errorf("hypotheses_store.FindOrCreateHypothesis: insert: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Lost a race with a concurrent registration of the same normalized text.
		existing, err := s.FindByNormalizedText(ctx, normalized)
		if err != nil {
			return nil, false, err
		}
		if existing != nil {
			return existing, true, nil
		}
	}
	return h, false, nil
}

// FindByNormalizedText looks up a hypothesis by its normalized text, or returns nil if none exists.
func (s *HypothesesStore) FindByNormalizedText(ctx context.Context, normalized string) (*domain.Hypothesis, error) {
	q := `SELECT ` + hypothesisColumns + ` FROM hypotheses WHERE normalized_text = $1`
	row := s.pool.pool.QueryRow(ctx, q, normalized)
	h, err := scanHypothesis(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("hypotheses_store.FindByNormalizedText: %w", err)
	}
	return h, nil
}

// GetHypothesis fetches a single hypothesis by ID.
func (s *HypothesesStore) GetHypothesis(ctx context.Context, id string) (*domain.Hypothesis, error) {
	q := `SELECT ` + hypothesisColumns + ` FROM hypotheses WHERE id = $1`
	row := s.pool.pool.QueryRow(ctx, q, id)
	h, err := scanHypothesis(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("hypotheses_store.GetHypothesis: %w", err)
	}
	return h, nil
}

// ListHypotheses returns all registered hypotheses, most recent first.
func (s *HypothesesStore) ListHypotheses(ctx context.Context) ([]*domain.Hypothesis, error) {
	q := `SELECT ` + hypothesisColumns + ` FROM hypotheses ORDER BY created_at DESC`
	rows, err := s.pool.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.ListHypotheses: %w", err)
	}
	defer rows.Close()

	var out []*domain.Hypothesis
	for rows.Next() {
		h, err := scanHypothesis(rows)
		if err != nil {
			return nil, fmt.Errorf("hypotheses_store.ListHypotheses: scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func scanHypothesis(row rowScanner) (*domain.Hypothesis, error) {
	h := &domain.Hypothesis{}
	if err := row.Scan(&h.ID, &h.AgentID, &h.Text, &h.CreatedAt); err != nil {
		return nil, err
	}
	return h, nil
}
