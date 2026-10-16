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

// defaultHypothesisListLimit and maxHypothesisListLimit bound every finding/comment page. The
// default is deliberately far below the maximum: every caller is an autonomous agent whose whole
// response lands in a bounded context window, so a caller that does not think about paging gets a
// page it can read, plus the exact total telling it what it has not seen.
const (
	defaultHypothesisListLimit = 20
	maxHypothesisListLimit     = 200
)

// HypothesisActivity is how much evidence hangs off one hypothesis.
type HypothesisActivity struct {
	Findings int
	Comments int
}

// HypothesesStore persists the evidence filed against hypotheses: findings and comments. The
// hypotheses themselves live in the graph store (shared/graphdb).
type HypothesesStore struct {
	pool *Pool
}

// NewHypothesesStore creates a HypothesesStore backed by pool.
func NewHypothesesStore(pool *Pool) *HypothesesStore {
	return &HypothesesStore{pool: pool}
}

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

// ErrFindingExists reports that the job already has its finding.
var ErrFindingExists = errors.New("the job already has a summary")

// CreateHypothesisFinding records the agent's post-run write-up for a job, attached to the
// hypothesis it tested. A job has one finding (UNIQUE on experiment_id in shared/db/migrations)
// and it is never replaced: a second one is refused with ErrFindingExists. agentID is the job's
// agent, whose finding it is, and actor whose credential files it.
func (s *HypothesesStore) CreateHypothesisFinding(ctx context.Context, hypothesisID, platformExperimentID, experimentID, agentID string, actor domain.Actor, summary string) (*domain.HypothesisFinding, error) {
	id, err := newHypothesisID()
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.CreateHypothesisFinding: %w", err)
	}
	f := &domain.HypothesisFinding{
		ID:                   id,
		HypothesisID:         hypothesisID,
		PlatformExperimentID: platformExperimentID,
		ExperimentID:         experimentID,
		AgentID:              agentID,
		Actor:                actor,
		Summary:              summary,
		CreatedAt:            time.Now().UTC(),
	}
	const q = `
INSERT INTO hypothesis_findings (id, hypothesis_id, platform_experiment_id, experiment_id, agent_id, actor_kind, actor_id, summary, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	if _, err := s.pool.pool.Exec(ctx, q, f.ID, f.HypothesisID, f.PlatformExperimentID, f.ExperimentID, f.AgentID, string(actor.Kind), actor.ID, f.Summary, f.CreatedAt); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == "hypothesis_findings_experiment_id_key" {
			return nil, fmt.Errorf("hypotheses_store.CreateHypothesisFinding: %w: %s", ErrFindingExists, experimentID)
		}
		return nil, fmt.Errorf("hypotheses_store.CreateHypothesisFinding: %w", err)
	}
	return f, nil
}

// FindingSummary returns the summary filed for a job, empty when none was.
func (s *HypothesesStore) FindingSummary(ctx context.Context, experimentID string) (string, error) {
	var summary string
	err := s.pool.pool.QueryRow(ctx, `SELECT summary FROM hypothesis_findings WHERE experiment_id = $1`, experimentID).Scan(&summary)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("hypotheses_store.FindingSummary: %w", err)
	}
	return summary, nil
}

// FindingSummaries returns the summary filed for each job in experimentIDs that has one.
func (s *HypothesesStore) FindingSummaries(ctx context.Context, experimentIDs []string) (map[string]string, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT experiment_id, summary FROM hypothesis_findings WHERE experiment_id = ANY($1)`, experimentIDs)
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.FindingSummaries: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, summary string
		if err := rows.Scan(&id, &summary); err != nil {
			return nil, fmt.Errorf("hypotheses_store.FindingSummaries: scan: %w", err)
		}
		out[id] = summary
	}
	return out, rows.Err()
}

// NewestFindings returns the newest finding filed against each hypothesis in ids, in one query. A
// hypothesis with no finding is absent.
func (s *HypothesesStore) NewestFindings(ctx context.Context, ids []string) (map[string]*domain.HypothesisFinding, error) {
	const q = `SELECT DISTINCT ON (hypothesis_id) id, hypothesis_id, platform_experiment_id, experiment_id, agent_id, actor_kind, actor_id, summary, created_at
FROM hypothesis_findings WHERE hypothesis_id = ANY($1) ORDER BY hypothesis_id, created_at DESC`
	rows, err := s.pool.pool.Query(ctx, q, ids)
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.NewestFindings: %w", err)
	}
	defer rows.Close()
	out := map[string]*domain.HypothesisFinding{}
	for rows.Next() {
		f := &domain.HypothesisFinding{}
		var actorKind domain.ActorKind
		var actorID string
		if err := rows.Scan(&f.ID, &f.HypothesisID, &f.PlatformExperimentID, &f.ExperimentID, &f.AgentID, &actorKind, &actorID, &f.Summary, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("hypotheses_store.NewestFindings: scan: %w", err)
		}
		f.Actor = domain.StoredActor(actorKind, actorID, "", f.AgentID)
		out[f.HypothesisID] = f
	}
	return out, rows.Err()
}

// ListFindingsByHypothesis returns one page of the findings filed against a hypothesis, oldest
// first — the accumulated evidence trail for that claim across every job that tested it. The
// trail only ever grows, so limit is defaulted and clamped exactly like every other list read
// here; CountFindingsByHypothesis reports how much of it a page left behind.
func (s *HypothesesStore) ListFindingsByHypothesis(ctx context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisFinding, error) {
	limit, offset = clampHypothesisPage(limit, offset)
	const q = `SELECT id, hypothesis_id, platform_experiment_id, experiment_id, agent_id, actor_kind, actor_id, summary, created_at
FROM hypothesis_findings WHERE hypothesis_id = $1 ORDER BY created_at ASC LIMIT $2 OFFSET $3`
	rows, err := s.pool.pool.Query(ctx, q, hypothesisID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.ListFindingsByHypothesis: %w", err)
	}
	defer rows.Close()

	out := []*domain.HypothesisFinding{}
	for rows.Next() {
		f := &domain.HypothesisFinding{}
		var actorKind domain.ActorKind
		var actorID string
		if err := rows.Scan(&f.ID, &f.HypothesisID, &f.PlatformExperimentID, &f.ExperimentID, &f.AgentID, &actorKind, &actorID, &f.Summary, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("hypotheses_store.ListFindingsByHypothesis: scan: %w", err)
		}
		f.Actor = domain.StoredActor(actorKind, actorID, "", f.AgentID)
		out = append(out, f)
	}
	return out, rows.Err()
}

// CreateHypothesisComment records a freeform, job-independent note against a hypothesis — see
// domain.HypothesisComment for how this differs from a finding.
// agentID is whose note it is, empty when no agent owns it, and actor whose credential wrote it.
func (s *HypothesesStore) CreateHypothesisComment(ctx context.Context, hypothesisID, platformExperimentID, agentID string, actor domain.Actor, text string) (*domain.HypothesisComment, error) {
	id, err := newHypothesisID()
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.CreateHypothesisComment: %w", err)
	}
	c := &domain.HypothesisComment{
		ID:                   id,
		HypothesisID:         hypothesisID,
		PlatformExperimentID: platformExperimentID,
		AgentID:              agentID,
		Actor:                actor,
		Text:                 text,
		CreatedAt:            time.Now().UTC(),
	}
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.CreateHypothesisComment: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := holdOpenPlatformExperiment(ctx, tx, platformExperimentID); err != nil {
		return nil, fmt.Errorf("hypotheses_store.CreateHypothesisComment: %w", err)
	}
	const q = `
INSERT INTO hypothesis_comments (id, hypothesis_id, platform_experiment_id, agent_id, actor_kind, actor_id, text, created_at)
VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8)`
	if _, err := tx.Exec(ctx, q, c.ID, c.HypothesisID, c.PlatformExperimentID, c.AgentID, string(actor.Kind), actor.ID, c.Text, c.CreatedAt); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
			return nil, fmt.Errorf("hypotheses_store.CreateHypothesisComment: %w: %s", ErrUnknownAgent, agentID)
		}
		return nil, fmt.Errorf("hypotheses_store.CreateHypothesisComment: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("hypotheses_store.CreateHypothesisComment: commit: %w", err)
	}
	return c, nil
}

// holdOpenPlatformExperiment refuses a hypothesis write to a closed platform experiment with
// ErrPlatformExperimentClosed, whoever makes it. It holds the row until tx ends, so a close waits
// for a write that passed and a write that waited for a close sees it.
func holdOpenPlatformExperiment(ctx context.Context, tx pgx.Tx, platformExperimentID string) error {
	var status domain.PlatformExperimentStatus
	err := tx.QueryRow(ctx, `SELECT status FROM platform_experiments WHERE id = $1 FOR SHARE`, platformExperimentID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrPlatformExperimentNotFound, platformExperimentID)
	}
	if err != nil {
		return err
	}
	if status == domain.PlatformExpClosed {
		return fmt.Errorf("%w: %s", ErrPlatformExperimentClosed, platformExperimentID)
	}
	return nil
}

// ListCommentsByHypothesis returns one page of the comments filed against a hypothesis, oldest
// first — bounded like ListFindingsByHypothesis, for the same reason.
func (s *HypothesesStore) ListCommentsByHypothesis(ctx context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisComment, error) {
	limit, offset = clampHypothesisPage(limit, offset)
	const q = `SELECT id, hypothesis_id, platform_experiment_id, COALESCE(agent_id, ''), actor_kind, actor_id, author, text, created_at
FROM hypothesis_comments WHERE hypothesis_id = $1 ORDER BY created_at ASC LIMIT $2 OFFSET $3`
	rows, err := s.pool.pool.Query(ctx, q, hypothesisID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.ListCommentsByHypothesis: %w", err)
	}
	defer rows.Close()

	out := []*domain.HypothesisComment{}
	for rows.Next() {
		c := &domain.HypothesisComment{}
		var actorKind domain.ActorKind
		var actorID, author string
		if err := rows.Scan(&c.ID, &c.HypothesisID, &c.PlatformExperimentID, &c.AgentID, &actorKind, &actorID, &author, &c.Text, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("hypotheses_store.ListCommentsByHypothesis: scan: %w", err)
		}
		c.Actor = domain.StoredActor(actorKind, actorID, author, c.AgentID)
		out = append(out, c)
	}
	return out, rows.Err()
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

// DeleteHypothesisComments removes the comments filed against a hypothesis that was removed, and
// its checkouts.
func (s *HypothesesStore) DeleteHypothesisComments(ctx context.Context, hypothesisID string) error {
	if _, err := s.pool.pool.Exec(ctx, `DELETE FROM hypothesis_comments WHERE hypothesis_id = $1`, hypothesisID); err != nil {
		return fmt.Errorf("hypotheses_store.DeleteHypothesisComments: %w", err)
	}
	if _, err := s.pool.pool.Exec(ctx, `DELETE FROM hypothesis_checkouts WHERE hypothesis_id = $1`, hypothesisID); err != nil {
		return fmt.Errorf("hypotheses_store.DeleteHypothesisComments: checkouts: %w", err)
	}
	return nil
}

// CountHypothesisActivity returns the finding and comment counts of every hypothesis in ids, in
// one query. A hypothesis with neither is present with zero counts.
func (s *HypothesesStore) CountHypothesisActivity(ctx context.Context, ids []string) (map[string]HypothesisActivity, error) {
	out := make(map[string]HypothesisActivity, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	const q = `
SELECT ids.id,
       (SELECT COUNT(*) FROM hypothesis_findings f WHERE f.hypothesis_id = ids.id),
       (SELECT COUNT(*) FROM hypothesis_comments c WHERE c.hypothesis_id = ids.id)
FROM unnest($1::text[]) AS ids(id)`
	rows, err := s.pool.pool.Query(ctx, q, ids)
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.CountHypothesisActivity: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var a HypothesisActivity
		if err := rows.Scan(&id, &a.Findings, &a.Comments); err != nil {
			return nil, fmt.Errorf("hypotheses_store.CountHypothesisActivity: scan: %w", err)
		}
		out[id] = a
	}
	return out, rows.Err()
}

// evidenceStatuses are the job statuses an Evidence entry counts, each under its lowercase name.
var evidenceStatuses = []domain.ExperimentStatus{
	domain.StatusSubmitted, domain.StatusQueued, domain.StatusRunning,
	domain.StatusCompleted, domain.StatusFailed, domain.StatusEvicted, domain.StatusRejected,
}

// Evidence returns what Postgres holds about each hypothesis of a platform experiment, keyed by
// hypothesis id, in the form a graph query binds as $evidence: jobs (every job but rejected
// ones), one count per lowercase job status, findings, comments, and last_finding_at when there
// is a finding. Every count is present, zero included. A hypothesis with no job, finding or
// comment has no entry.
func (s *HypothesesStore) Evidence(ctx context.Context, platformExperimentID string) (map[string]any, error) {
	const q = `
SELECT hypothesis_id, lower(status::text), COUNT(*), NULL::timestamptz FROM experiments
WHERE platform_experiment_id = $1 GROUP BY 1, 2
UNION ALL
SELECT hypothesis_id, 'findings', COUNT(*), MAX(created_at) FROM hypothesis_findings
WHERE platform_experiment_id = $1 GROUP BY 1
UNION ALL
SELECT hypothesis_id, 'comments', COUNT(*), NULL FROM hypothesis_comments
WHERE platform_experiment_id = $1 GROUP BY 1`
	rows, err := s.pool.pool.Query(ctx, q, platformExperimentID)
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.Evidence: %w", err)
	}
	defer rows.Close()
	rejected := strings.ToLower(string(domain.StatusRejected))
	out := map[string]any{}
	for rows.Next() {
		var id, key string
		var n int64
		var newest *time.Time
		if err := rows.Scan(&id, &key, &n, &newest); err != nil {
			return nil, fmt.Errorf("hypotheses_store.Evidence: scan: %w", err)
		}
		e, ok := out[id].(map[string]any)
		if !ok {
			e = map[string]any{"jobs": int64(0), "findings": int64(0), "comments": int64(0)}
			for _, status := range evidenceStatuses {
				e[strings.ToLower(string(status))] = int64(0)
			}
			out[id] = e
		}
		e[key] = n
		if key != "findings" && key != "comments" && key != rejected {
			e["jobs"] = e["jobs"].(int64) + n
		}
		if newest != nil {
			e["last_finding_at"] = newest.UTC()
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hypotheses_store.Evidence: %w", err)
	}
	return out, nil
}

// agentAtWork is the condition that the agent of agentRef in the platform experiment of peRef has
// not ended: it neither finished nor was cut, and the platform experiment is not closed.
func agentAtWork(peRef, agentRef string) string {
	return `NOT EXISTS (SELECT 1 FROM experiment_signups fin WHERE fin.platform_experiment_id = ` + peRef + ` AND fin.agent_id = ` + agentRef + ` AND fin.finished_at IS NOT NULL)
	AND NOT EXISTS (SELECT 1 FROM platform_experiment_cuts cut WHERE cut.platform_experiment_id = ` + peRef + ` AND cut.agent_id = ` + agentRef + `)
	AND NOT EXISTS (SELECT 1 FROM platform_experiments shut WHERE shut.id = ` + peRef + ` AND shut.status = 'closed')`
}

// checkoutClaims is the condition on a hypothesis_checkouts row c that it still claims its
// hypothesis: checked out within $2 seconds by an agent that has not ended.
var checkoutClaims = `c.checked_out_at > now() - make_interval(secs => $2)
	AND ` + agentAtWork("c.platform_experiment_id", "c.agent_id")

// jobOnIt is the condition on an experiments row e that a job of an agent at work is queued or running.
var jobOnIt = `e.status IN ('QUEUED', 'SUBMITTED', 'RUNNING') AND ` + agentAtWork("e.platform_experiment_id", "e.agent_id")

// CheckOutHypothesis records that agentID checked hypothesisID of platformExpID out on branch
// now, and answers the other agents working on it.
func (s *HypothesesStore) CheckOutHypothesis(ctx context.Context, platformExpID, hypothesisID, agentID, branch string) ([]domain.HypothesisWorker, error) {
	if _, err := s.pool.pool.Exec(ctx, `
INSERT INTO hypothesis_checkouts (hypothesis_id, agent_id, platform_experiment_id, branch) VALUES ($1, $2, $3, $4)
ON CONFLICT (hypothesis_id, agent_id) DO UPDATE SET branch = EXCLUDED.branch, checked_out_at = now()`,
		hypothesisID, agentID, platformExpID, branch); err != nil {
		return nil, fmt.Errorf("hypotheses_store.CheckOutHypothesis: %w", err)
	}
	working, err := s.Working(ctx, []string{hypothesisID})
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.CheckOutHypothesis: %w", err)
	}
	others := []domain.HypothesisWorker{}
	for _, w := range working[hypothesisID] {
		if w.AgentID != agentID {
			others = append(others, w)
		}
	}
	return others, nil
}

// TakenHypotheses are the ids of the hypotheses some agent works on now, as Working counts it,
// within platformExpID or, empty, in every platform experiment.
func (s *HypothesesStore) TakenHypotheses(ctx context.Context, platformExpID string) ([]string, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT e.hypothesis_id FROM experiments e
WHERE e.hypothesis_id <> '' AND `+jobOnIt+` AND ($1 = '' OR e.platform_experiment_id = $1)
UNION
SELECT c.hypothesis_id FROM hypothesis_checkouts c
WHERE ($1 = '' OR c.platform_experiment_id = $1) AND `+checkoutClaims, platformExpID, domain.CheckoutClaim.Seconds())
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.TakenHypotheses: %w", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("hypotheses_store.TakenHypotheses: scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Working is, for each of ids with any, the agents working on it now (domain.HypothesisWorker),
// by agent id.
func (s *HypothesesStore) Working(ctx context.Context, ids []string) (map[string][]domain.HypothesisWorker, error) {
	out := map[string][]domain.HypothesisWorker{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.pool.Query(ctx, `
SELECT on_it.hypothesis_id, on_it.agent_id, COALESCE(NULLIF(a.name, ''), on_it.agent_id)
FROM (
	SELECT e.hypothesis_id, e.agent_id FROM experiments e
	WHERE e.hypothesis_id = ANY($1) AND `+jobOnIt+`
	UNION
	SELECT c.hypothesis_id, c.agent_id FROM hypothesis_checkouts c
	WHERE c.hypothesis_id = ANY($1) AND `+checkoutClaims+`
) on_it LEFT JOIN agents a ON a.id = on_it.agent_id
ORDER BY on_it.hypothesis_id, on_it.agent_id`, ids, domain.CheckoutClaim.Seconds())
	if err != nil {
		return nil, fmt.Errorf("hypotheses_store.Working: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var w domain.HypothesisWorker
		if err := rows.Scan(&id, &w.AgentID, &w.Name); err != nil {
			return nil, fmt.Errorf("hypotheses_store.Working: scan: %w", err)
		}
		out[id] = append(out[id], w)
	}
	return out, rows.Err()
}
