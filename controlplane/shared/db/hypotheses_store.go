package db

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ErrUnknownAgent is returned when a write references an agent_id that has never registered —
// a foreign-key violation the caller sent bad data to cause, not a server fault, so callers map
// it to 4xx rather than 500.
var ErrUnknownAgent = errors.New("unknown agent_id")

// pgForeignKeyViolation is the PostgreSQL SQLSTATE for a foreign-key constraint violation.
const pgForeignKeyViolation = "23503"

// defaultHypothesisListLimit and maxHypothesisListLimit bound every ListHypotheses read so a
// weeks-long platform experiment's accumulated pool can never flood an agent's catch-up read —
// see plan.md. 200 is ample even for a single agent's full own-history read: registration is
// gated by the platform's per-agent submission rate limit, so one agent cannot accumulate
// thousands of hypotheses over a run.
// The default is deliberately far below the maximum. Every caller of this API is an autonomous
// agent whose whole response lands in a bounded context window, so a list that answers "here is
// everything" hands it a truncated or poisoned context and no way to recover. A caller that
// genuinely wants a large page asks for one; a caller that does not think about it gets a page it
// can read, plus the exact total in X-Total-Count telling it what it has not seen.
const (
	defaultHypothesisListLimit = 20
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

// agent_id is NULL on a human-submitted row and read back as the empty string, which is what
// "no owner" means everywhere above the store — every ownership check compares against it.
const hypothesisColumns = `id, COALESCE(agent_id, ''), source, author, platform_experiment_id, text, status, created_at`

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
// (platform_experiment_id, normalized_text) (shared/db/schema.sql) rather than any in-process novelty
// heuristic. The same text registered under a different platform experiment is a distinct
// hypothesis. Returns (hypothesis, alreadyExisted, error).
//
// source/agentID/author come already validated from registry.RegisterHypothesis (see
// domain.ClassifyHypothesisOrigin) — the dedup below is deliberately blind to all three, so a
// human idea and an agent's restatement of the same claim collide exactly as two agent rows do.
func (s *HypothesesStore) FindOrCreateHypothesis(ctx context.Context, source domain.HypothesisSource, agentID, author, platformExperimentID, text string) (*domain.Hypothesis, bool, error) {
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
		Source:               source,
		Author:               author,
		PlatformExperimentID: platformExperimentID,
		Text:                 text,
		Status:               domain.HypothesisOpen,
		CreatedAt:            time.Now().UTC(),
	}

	const q = `
INSERT INTO hypotheses (id, agent_id, source, author, platform_experiment_id, text, normalized_text, status, created_at)
VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (platform_experiment_id, normalized_text) DO NOTHING`
	tag, err := s.pool.pool.Exec(ctx, q, h.ID, h.AgentID, h.Source, h.Author, h.PlatformExperimentID, h.Text, normalized, h.Status, h.CreatedAt)
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

// ListHypotheses returns hypotheses, most recent first, each carrying its finding/comment
// counts. Bounded at the store so the endpoint can never return unbounded rows even if a
// caller forgets a limit: limit <= 0 uses defaultHypothesisListLimit, and any limit above
// maxHypothesisListLimit is clamped down to it. platformExperimentID, if non-empty, restricts
// the result to that one pool; empty spans every platform experiment (the operator-facing
// global view — a single ORDER BY created_at DESC LIMIT/OFFSET across all pools, not a
// per-pool fetch merged client-side). agentID, if non-empty, restricts the result to that
// agent's own hypotheses. status, if non-empty, restricts the result to that one status
// (open/confirmed/inconclusive) — lets a caller pull just the still-actionable rows
// (open/inconclusive) or just the settled ones (confirmed) instead of paying to fetch and
// filter the whole pool client-side, which matters once the hypothesis count grows past a
// single catch-up's easy skim.
func (s *HypothesesStore) ListHypotheses(ctx context.Context, platformExperimentID, agentID string, status domain.HypothesisStatus, limit, offset int) ([]*HypothesisListItem, error) {
	if limit <= 0 {
		limit = defaultHypothesisListLimit
	} else if limit > maxHypothesisListLimit {
		limit = maxHypothesisListLimit
	}

	clauses, args := hypothesisFilterClauses(platformExperimentID, agentID, status)
	args = append(args, limit)

	q := `
SELECT h.id, COALESCE(h.agent_id, ''), h.source, h.author, h.platform_experiment_id, h.text, h.status, h.created_at,
       COUNT(DISTINCT f.id) AS finding_count, COUNT(DISTINCT c.id) AS comment_count
FROM hypotheses h
LEFT JOIN hypothesis_findings f ON f.hypothesis_id = h.id
LEFT JOIN hypothesis_comments c ON c.hypothesis_id = h.id
WHERE ` + strings.Join(clauses, " AND ") + `
GROUP BY h.id
ORDER BY h.created_at DESC, h.id DESC
LIMIT $` + fmt.Sprint(len(args))
	if offset > 0 {
		args = append(args, offset)
		q += " OFFSET $" + fmt.Sprint(len(args))
	}

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
		if err := rows.Scan(&h.ID, &h.AgentID, &h.Source, &h.Author, &h.PlatformExperimentID, &h.Text, &h.Status, &h.CreatedAt, &item.FindingCount, &item.CommentCount); err != nil {
			return nil, fmt.Errorf("hypotheses_store.ListHypotheses: scan: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// CreateHypothesisFinding records the agent's post-run write-up for a job, attached to the
// hypothesis it tested. One finding per job (UNIQUE on experiment_id in shared/db/schema.sql) — a
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

// ListFindingsByHypothesis returns one page of the findings filed against a hypothesis, oldest
// first — the accumulated evidence trail for that claim across every job that tested it. The
// trail only ever grows, so limit is defaulted and clamped exactly like every other list read
// here; CountFindingsByHypothesis reports how much of it a page left behind.
func (s *HypothesesStore) ListFindingsByHypothesis(ctx context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisFinding, error) {
	limit, offset = clampHypothesisPage(limit, offset)
	const q = `SELECT id, hypothesis_id, experiment_id, agent_id, summary, created_at
FROM hypothesis_findings WHERE hypothesis_id = $1 ORDER BY created_at ASC LIMIT $2 OFFSET $3`
	rows, err := s.pool.pool.Query(ctx, q, hypothesisID, limit, offset)
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
func (s *HypothesesStore) CreateHypothesisComment(ctx context.Context, hypothesisID string, source domain.HypothesisSource, agentID, author, text string) (*domain.HypothesisComment, error) {
	id, err := newHypothesisID()
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.CreateHypothesisComment: %w", err)
	}
	c := &domain.HypothesisComment{
		ID:           id,
		HypothesisID: hypothesisID,
		AgentID:      agentID,
		Source:       source,
		Author:       author,
		Text:         text,
		CreatedAt:    time.Now().UTC(),
	}
	const q = `
INSERT INTO hypothesis_comments (id, hypothesis_id, agent_id, source, author, text, created_at)
VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7)`
	if _, err := s.pool.pool.Exec(ctx, q, c.ID, c.HypothesisID, c.AgentID, c.Source, c.Author, c.Text, c.CreatedAt); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
			return nil, fmt.Errorf("hypotheses_store.CreateHypothesisComment: %w: %s", ErrUnknownAgent, agentID)
		}
		return nil, fmt.Errorf("hypotheses_store.CreateHypothesisComment: %w", err)
	}
	return c, nil
}

// ListCommentsByHypothesis returns one page of the comments filed against a hypothesis, oldest
// first — bounded like ListFindingsByHypothesis, for the same reason.
func (s *HypothesesStore) ListCommentsByHypothesis(ctx context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisComment, error) {
	limit, offset = clampHypothesisPage(limit, offset)
	const q = `SELECT id, hypothesis_id, COALESCE(agent_id, ''), source, author, text, created_at
FROM hypothesis_comments WHERE hypothesis_id = $1 ORDER BY created_at ASC LIMIT $2 OFFSET $3`
	rows, err := s.pool.pool.Query(ctx, q, hypothesisID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.ListCommentsByHypothesis: %w", err)
	}
	defer rows.Close()

	out := []*domain.HypothesisComment{} // non-nil: see ListHypotheses for why
	for rows.Next() {
		c := &domain.HypothesisComment{}
		if err := rows.Scan(&c.ID, &c.HypothesisID, &c.AgentID, &c.Source, &c.Author, &c.Text, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("hypotheses_store.ListCommentsByHypothesis: scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanHypothesis(row rowScanner) (*domain.Hypothesis, error) {
	h := &domain.Hypothesis{}
	if err := row.Scan(&h.ID, &h.AgentID, &h.Source, &h.Author, &h.PlatformExperimentID, &h.Text, &h.Status, &h.CreatedAt); err != nil {
		return nil, err
	}
	return h, nil
}

// ErrNotOwner is returned by UpdateHypothesisStatus when callerAgentID doesn't match the
// hypothesis's own agent_id — a hypothesis's status is the owning agent's own claim to a verdict
// on its own idea, not a call any other agent (or the operator) gets to make.
var ErrNotOwner = fmt.Errorf("hypotheses_store: caller does not own this hypothesis")

// UpdateHypothesisStatus sets a hypothesis's status, but only if callerAgentID matches the
// hypothesis's own agent_id — enforced in the same statement as the write (not a separate
// read-then-check) so there's no TOCTOU window. Returns ErrNotOwner if the hypothesis exists but
// belongs to a different agent, or a "not found" nil/nil if no such hypothesis exists at all —
// callers distinguish the two with a preceding existence check if they need a different message.
// A human-submitted row has a NULL agent_id — nobody owns it, so any agent may settle it. That is
// the whole rule: `agent_id = $2 OR agent_id IS NULL`, one predicate, still a single statement.
// The alternative considered was to let an agent settle a human row only once it had filed a
// finding against it. It is rejected as more machinery for a weaker guarantee: it makes the write
// depend on a second table, turns one refusal into two indistinguishable ones (not owner / no
// finding yet), and still stops nobody determined — an agent can file a finding and then stamp
// whatever it likes. This is a shared lab notebook with no auth (important.md #14), so the honest
// model is a wiki page: whoever read the evidence records the verdict, and the findings and
// comments hanging off the row are the audit trail if the verdict is wrong. Leaving it unsettable
// is not an option — agents can now run jobs against a human row, and one that stays `open` after
// being settled makes the pool lie about what is still unresolved.
func (s *HypothesesStore) UpdateHypothesisStatus(ctx context.Context, id, callerAgentID string, status domain.HypothesisStatus) (*domain.Hypothesis, error) {
	const q = `UPDATE hypotheses SET status = $3 WHERE id = $1 AND (agent_id = $2 OR agent_id IS NULL) RETURNING ` + hypothesisColumns
	row := s.pool.pool.QueryRow(ctx, q, id, callerAgentID, status)
	h, err := scanHypothesis(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			// Either the hypothesis doesn't exist, or it does but belongs to someone else —
			// distinguish so the caller can return 404 vs 403 instead of always one or the other.
			existing, getErr := s.GetHypothesis(ctx, id)
			if getErr != nil {
				return nil, fmt.Errorf("hypotheses_store.UpdateHypothesisStatus: %w", getErr)
			}
			if existing == nil {
				return nil, nil
			}
			return nil, ErrNotOwner
		}
		return nil, fmt.Errorf("hypotheses_store.UpdateHypothesisStatus: %w", err)
	}
	return h, nil
}

// hypothesisFilterClauses builds the WHERE fragment shared by ListHypotheses and
// CountHypotheses, so a page and its total can never be computed over different predicates.
// platformExperimentID is optional: empty means every platform experiment's pool (the global,
// cross-experiment view /hypotheses without a filter needs) rather than one pool's rows.
func hypothesisFilterClauses(platformExperimentID, agentID string, status domain.HypothesisStatus) ([]string, []any) {
	clauses := []string{"1=1"}
	args := []any{}
	if platformExperimentID != "" {
		args = append(args, platformExperimentID)
		clauses = append(clauses, fmt.Sprintf("h.platform_experiment_id = $%d", len(args)))
	}
	if agentID != "" {
		args = append(args, agentID)
		clauses = append(clauses, fmt.Sprintf("h.agent_id = $%d", len(args)))
	}
	if status != "" {
		args = append(args, status)
		clauses = append(clauses, fmt.Sprintf("h.status = $%d", len(args)))
	}
	return clauses, args
}

// CountHypotheses returns how many hypotheses match, ignoring limit/offset — the total a
// paginating caller shows, matching the X-Total-Count convention of every other list endpoint.
func (s *HypothesesStore) CountHypotheses(ctx context.Context, platformExperimentID, agentID string, status domain.HypothesisStatus) (int, error) {
	clauses, args := hypothesisFilterClauses(platformExperimentID, agentID, status)
	q := `SELECT COUNT(*) FROM hypotheses h WHERE ` + strings.Join(clauses, " AND ")
	var total int
	if err := s.pool.pool.QueryRow(ctx, q, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("hypotheses_store.CountHypotheses: %w", err)
	}
	return total, nil
}

// clampHypothesisPage applies the shared list bounds to a hypothesis sub-list page.
func clampHypothesisPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = defaultHypothesisListLimit
	} else if limit > maxHypothesisListLimit {
		limit = maxHypothesisListLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// CountFindingsByHypothesis returns the full number of findings filed against a hypothesis,
// ignoring limit/offset.
func (s *HypothesesStore) CountFindingsByHypothesis(ctx context.Context, hypothesisID string) (int, error) {
	var n int
	if err := s.pool.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM hypothesis_findings WHERE hypothesis_id = $1`, hypothesisID).Scan(&n); err != nil {
		return 0, fmt.Errorf("hypotheses_store.CountFindingsByHypothesis: %w", err)
	}
	return n, nil
}

// CountCommentsByHypothesis returns the full number of comments filed against a hypothesis,
// ignoring limit/offset.
func (s *HypothesesStore) CountCommentsByHypothesis(ctx context.Context, hypothesisID string) (int, error) {
	var n int
	if err := s.pool.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM hypothesis_comments WHERE hypothesis_id = $1`, hypothesisID).Scan(&n); err != nil {
		return 0, fmt.Errorf("hypotheses_store.CountCommentsByHypothesis: %w", err)
	}
	return n, nil
}
