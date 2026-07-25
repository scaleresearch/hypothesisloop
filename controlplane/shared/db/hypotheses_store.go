package db

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// defaultHypothesisListLimit and maxHypothesisListLimit bound every ListHypotheses read so a
// weeks-long platform experiment's accumulated pool can never flood an agent's catch-up read —
// see plan.md. 200 is ample even for a single agent's full own-history read: registration is
// gated by the platform's per-agent submission rate limit, so one agent cannot accumulate
// thousands of hypotheses over a run.
const (
	defaultHypothesisListLimit = 200
	maxHypothesisListLimit     = 200
)

// HypothesisListItem bundles a hypothesis with cheap activity counts — not the finding/comment
// bodies — so a bounded list read tells the agent which rows are worth a full detail fetch,
// without opening every hypothesis in the pool.
type HypothesisListItem struct {
	*domain.Hypothesis
	FindingCount int `json:"finding_count"`
	CommentCount int `json:"comment_count"`
}

// HypothesesStore provides persistence for domain.Hypothesis and domain.HypothesisFinding.
type HypothesesStore struct {
	pool *Pool
}

// NewHypothesesStore creates a HypothesesStore backed by pool.
func NewHypothesesStore(pool *Pool) *HypothesesStore {
	return &HypothesesStore{pool: pool}
}

const hypothesisColumns = `id, agent_id, platform_experiment_id, text, created_at`

// newHypothesisID generates a UUIDv7-formatted string, matching the ID scheme used
// elsewhere for agent-visible entities.
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

// FindOrCreateHypothesis registers a hypothesis within a platform experiment, or returns the
// existing row if one with equivalent normalized text already exists *in that same platform
// experiment* — this is the real uniqueness check: it relies on the DB's UNIQUE index on
// (platform_experiment_id, normalized_text) (schema.sql) rather than any in-process novelty
// heuristic. The same text registered under a different platform experiment is a distinct
// hypothesis. Returns (hypothesis, alreadyExisted, error).
func (s *HypothesesStore) FindOrCreateHypothesis(ctx context.Context, agentID, platformExperimentID, text string) (*domain.Hypothesis, bool, error) {
	normalized := domain.NormalizeHypothesisText(text)
	if normalized == "" {
		return nil, false, fmt.Errorf("hypotheses_store.FindOrCreateHypothesis: text is empty")
	}

	if existing, err := s.FindByNormalizedText(ctx, platformExperimentID, normalized); err != nil {
		return nil, false, err
	} else if existing != nil {
		return existing, true, nil
	}

	id, err := newHypothesisID()
	if err != nil {
		return nil, false, fmt.Errorf("hypotheses_store.FindOrCreateHypothesis: %w", err)
	}
	h := &domain.Hypothesis{
		ID:                   id,
		AgentID:              agentID,
		PlatformExperimentID: platformExperimentID,
		Text:                 text,
		CreatedAt:            time.Now().UTC(),
	}

	const q = `
INSERT INTO hypotheses (id, agent_id, platform_experiment_id, text, normalized_text, created_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (platform_experiment_id, normalized_text) DO NOTHING`
	tag, err := s.pool.pool.Exec(ctx, q, h.ID, h.AgentID, h.PlatformExperimentID, h.Text, normalized, h.CreatedAt)
	if err != nil {
		return nil, false, fmt.Errorf("hypotheses_store.FindOrCreateHypothesis: insert: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Lost a race with a concurrent registration of the same normalized text within
		// this platform experiment.
		existing, err := s.FindByNormalizedText(ctx, platformExperimentID, normalized)
		if err != nil {
			return nil, false, err
		}
		if existing != nil {
			return existing, true, nil
		}
	}
	return h, false, nil
}

// FindByNormalizedText looks up a hypothesis by its normalized text within a platform
// experiment, or returns nil if none exists.
func (s *HypothesesStore) FindByNormalizedText(ctx context.Context, platformExperimentID, normalized string) (*domain.Hypothesis, error) {
	q := `SELECT ` + hypothesisColumns + ` FROM hypotheses WHERE platform_experiment_id = $1 AND normalized_text = $2`
	row := s.pool.pool.QueryRow(ctx, q, platformExperimentID, normalized)
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

// ListHypotheses returns hypotheses registered within a platform experiment, most recent
// first, each carrying its finding/comment counts. Bounded at the store so the endpoint can
// never return unbounded rows even if a caller forgets a limit: limit <= 0 uses
// defaultHypothesisListLimit, and any limit above maxHypothesisListLimit is clamped down to
// it. agentID, if non-empty, restricts the result to that agent's own hypotheses.
func (s *HypothesesStore) ListHypotheses(ctx context.Context, platformExperimentID, agentID string, limit int) ([]*HypothesisListItem, error) {
	if limit <= 0 {
		limit = defaultHypothesisListLimit
	} else if limit > maxHypothesisListLimit {
		limit = maxHypothesisListLimit
	}

	clauses := []string{"h.platform_experiment_id = $1"}
	args := []any{platformExperimentID}
	if agentID != "" {
		args = append(args, agentID)
		clauses = append(clauses, fmt.Sprintf("h.agent_id = $%d", len(args)))
	}
	args = append(args, limit)

	q := `
SELECT h.id, h.agent_id, h.platform_experiment_id, h.text, h.created_at,
       COUNT(DISTINCT f.id) AS finding_count, COUNT(DISTINCT c.id) AS comment_count
FROM hypotheses h
LEFT JOIN hypothesis_findings f ON f.hypothesis_id = h.id
LEFT JOIN hypothesis_comments c ON c.hypothesis_id = h.id
WHERE ` + strings.Join(clauses, " AND ") + `
GROUP BY h.id
ORDER BY h.created_at DESC
LIMIT $` + fmt.Sprint(len(args))

	rows, err := s.pool.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.ListHypotheses: %w", err)
	}
	defer rows.Close()

	// Non-nil even when empty: a nil slice serializes to JSON `null`, and a zero-hypothesis
	// platform experiment is the common case (a fresh platform experiment always starts
	// with none) — callers merging results across many platform experiments must be able to
	// treat every response as an array without a null-check per item.
	out := []*HypothesisListItem{}
	for rows.Next() {
		h := &domain.Hypothesis{}
		item := &HypothesisListItem{Hypothesis: h}
		if err := rows.Scan(&h.ID, &h.AgentID, &h.PlatformExperimentID, &h.Text, &h.CreatedAt, &item.FindingCount, &item.CommentCount); err != nil {
			return nil, fmt.Errorf("hypotheses_store.ListHypotheses: scan: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// CreateHypothesisFinding records the agent's post-run write-up for a job, attached to the
// hypothesis it tested. One finding per job (UNIQUE on experiment_id in schema.sql) — a
// second call for the same experiment_id is a caller bug, not a legitimate "amend my
// write-up" path, and will fail the unique constraint.
func (s *HypothesesStore) CreateHypothesisFinding(ctx context.Context, hypothesisID, experimentID, agentID, summary string) (*domain.HypothesisFinding, error) {
	id, err := newHypothesisID()
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.CreateHypothesisFinding: %w", err)
	}
	f := &domain.HypothesisFinding{
		ID:           id,
		HypothesisID: hypothesisID,
		ExperimentID: experimentID,
		AgentID:      agentID,
		Summary:      summary,
		CreatedAt:    time.Now().UTC(),
	}
	const q = `
INSERT INTO hypothesis_findings (id, hypothesis_id, experiment_id, agent_id, summary, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := s.pool.pool.Exec(ctx, q, f.ID, f.HypothesisID, f.ExperimentID, f.AgentID, f.Summary, f.CreatedAt); err != nil {
		return nil, fmt.Errorf("hypotheses_store.CreateHypothesisFinding: %w", err)
	}
	return f, nil
}

// ListFindingsByHypothesis returns every finding filed against a hypothesis, oldest first —
// the accumulated evidence trail for that claim across every job that tested it.
func (s *HypothesesStore) ListFindingsByHypothesis(ctx context.Context, hypothesisID string) ([]*domain.HypothesisFinding, error) {
	const q = `SELECT id, hypothesis_id, experiment_id, agent_id, summary, created_at
FROM hypothesis_findings WHERE hypothesis_id = $1 ORDER BY created_at ASC`
	rows, err := s.pool.pool.Query(ctx, q, hypothesisID)
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.ListFindingsByHypothesis: %w", err)
	}
	defer rows.Close()

	out := []*domain.HypothesisFinding{} // non-nil: see ListHypotheses for why
	for rows.Next() {
		f := &domain.HypothesisFinding{}
		if err := rows.Scan(&f.ID, &f.HypothesisID, &f.ExperimentID, &f.AgentID, &f.Summary, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("hypotheses_store.ListFindingsByHypothesis: scan: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CreateHypothesisComment records a freeform, job-independent note against a hypothesis — see
// domain.HypothesisComment for how this differs from a finding.
func (s *HypothesesStore) CreateHypothesisComment(ctx context.Context, hypothesisID, agentID, text string) (*domain.HypothesisComment, error) {
	id, err := newHypothesisID()
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.CreateHypothesisComment: %w", err)
	}
	c := &domain.HypothesisComment{
		ID:           id,
		HypothesisID: hypothesisID,
		AgentID:      agentID,
		Text:         text,
		CreatedAt:    time.Now().UTC(),
	}
	const q = `
INSERT INTO hypothesis_comments (id, hypothesis_id, agent_id, text, created_at)
VALUES ($1, $2, $3, $4, $5)`
	if _, err := s.pool.pool.Exec(ctx, q, c.ID, c.HypothesisID, c.AgentID, c.Text, c.CreatedAt); err != nil {
		return nil, fmt.Errorf("hypotheses_store.CreateHypothesisComment: %w", err)
	}
	return c, nil
}

// ListCommentsByHypothesis returns every comment filed against a hypothesis, oldest first.
func (s *HypothesesStore) ListCommentsByHypothesis(ctx context.Context, hypothesisID string) ([]*domain.HypothesisComment, error) {
	const q = `SELECT id, hypothesis_id, agent_id, text, created_at
FROM hypothesis_comments WHERE hypothesis_id = $1 ORDER BY created_at ASC`
	rows, err := s.pool.pool.Query(ctx, q, hypothesisID)
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.ListCommentsByHypothesis: %w", err)
	}
	defer rows.Close()

	out := []*domain.HypothesisComment{} // non-nil: see ListHypotheses for why
	for rows.Next() {
		c := &domain.HypothesisComment{}
		if err := rows.Scan(&c.ID, &c.HypothesisID, &c.AgentID, &c.Text, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("hypotheses_store.ListCommentsByHypothesis: scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanHypothesis(row rowScanner) (*domain.Hypothesis, error) {
	h := &domain.Hypothesis{}
	if err := row.Scan(&h.ID, &h.AgentID, &h.PlatformExperimentID, &h.Text, &h.CreatedAt); err != nil {
		return nil, err
	}
	return h, nil
}
