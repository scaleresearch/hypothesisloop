package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// ClosePlatformExperiment closes a platform experiment and records why: by is whose credential
// asked, nil when the platform closes it at its end time, and message what that caller said of
// why. One already closed keeps what it has.
func (s *PlatformExperimentsStore) ClosePlatformExperiment(ctx context.Context, id string, by *domain.Actor, message string) error {
	reason, actor := "", domain.Actor{}
	if by != nil {
		reason, actor = string(domain.CloseByRequest), *by
	}
	// What the platform closes is a run at its end, a draft nobody proposed for inside the
	// planning limit or whose answer no person took up, an open one that was not running by
	// its baseline deadline, or a running one no researcher's machine came up for.
	const q = `
UPDATE platform_experiments
SET status = 'closed', updated_at = NOW(), closed_at = NOW(),
    close_reason = CASE WHEN $2 <> '' THEN $2
                        WHEN status = 'draft' AND proposed_at IS NULL THEN '` + string(domain.ClosePlanningLimitReached) + `'
                        WHEN status = 'draft' THEN '` + string(domain.CloseProposalExpired) + `'
                        WHEN status = 'open' THEN '` + string(domain.CloseBaselineLimitReached) + `'
                        WHEN started_at IS NULL THEN '` + string(domain.CloseAgentsNotStarted) + `'
                        ELSE '` + string(domain.CloseEndTimeReached) + `' END,
    closed_by_kind = $3, closed_by_id = $4, close_message = $5
WHERE id = $1 AND status <> 'closed'`
	if _, err := s.pool.pool.Exec(ctx, q, id, reason, string(actor.Kind), actor.ID, message); err != nil {
		return fmt.Errorf("platform_experiments_store.Close: %w", err)
	}
	return nil
}

// RemovePlatformExperiment removes platform experiment id for by: from then on it is answered to
// nobody and read by the platform's own loops alone, and everything it holds is kept. One not
// closed yet is closed in the same write, with close_reason removed. The trigger of the row
// announces the close and the removal. ErrPlatformExperimentNotFound for an id none has, and for
// one removed before.
func (s *PlatformExperimentsStore) RemovePlatformExperiment(ctx context.Context, id string, by domain.Actor) error {
	tag, err := s.pool.pool.Exec(ctx, `
UPDATE platform_experiments
SET removed_at = now(), removed_by_kind = $2, removed_by_id = $3, updated_at = now(),
    status = 'closed', closed_at = COALESCE(closed_at, now()),
    close_reason = CASE WHEN status = 'closed' THEN close_reason ELSE $4 END,
    closed_by_kind = CASE WHEN status = 'closed' THEN closed_by_kind ELSE $2 END,
    closed_by_id = CASE WHEN status = 'closed' THEN closed_by_id ELSE $3 END
WHERE id = $1 AND removed_at IS NULL`, id, string(by.Kind), by.ID, string(domain.CloseRemoved))
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Remove: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrPlatformExperimentNotFound
	}
	return nil
}

// RecordClose writes record as the platform's record of the close of the closed platform
// experiment id, once: one written before stays. The coordinator's summary is kept beside it.
func (s *PlatformExperimentsStore) RecordClose(ctx context.Context, id, record string) error {
	const q = `UPDATE platform_experiments SET close_record = $2, updated_at = NOW() WHERE id = $1 AND status = 'closed' AND close_record = ''`
	if _, err := s.pool.pool.Exec(ctx, q, id, record); err != nil {
		return fmt.Errorf("platform_experiments_store.RecordClose: %w", err)
	}
	return nil
}

// ErrNotReopenable is a platform experiment that is not a closed run that started and no other
// run continues.
var ErrNotReopenable = errors.New("platform experiment is not a closed run that started")

// ReopenPlatformExperiment sets the closed platform experiment id, whose run started and which no
// other run continues, running again until endsAt, durationHours from its start, and with a
// coordinatorModel that is not empty its hosted coordinator on that model. What the close wrote
// goes with it: closed_at, why and by whom, the agent lines and record of the close and the
// placements, so every reader takes it for the running run it was. The status change is announced
// by the row's trigger. ErrNotReopenable, and ErrPlatformExperimentNotFound for an id none has
// and a removed one.
func (s *PlatformExperimentsStore) ReopenPlatformExperiment(ctx context.Context, id string, endsAt time.Time, durationHours float64, coordinatorModel string) error {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Reopen: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `
UPDATE platform_experiments pe
SET status = 'running', updated_at = now(), ends_at = $2, duration_hours = $3,
    coordinator_model = CASE WHEN $4 <> '' THEN $4 ELSE coordinator_model END,
    closed_at = NULL, close_reason = '', closed_by_kind = '', closed_by_id = '', close_message = '',
    close_agents = '', close_record = ''
WHERE pe.id = $1 AND pe.status = 'closed' AND pe.removed_at IS NULL AND pe.started_at IS NOT NULL AND ($4 = '' OR pe.coordinator_hosted)
  AND NOT EXISTS (SELECT 1 FROM platform_experiments later WHERE later.continues = pe.id)`, id, endsAt, durationHours, coordinatorModel)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Reopen: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM platform_experiments WHERE id = $1 AND removed_at IS NULL)`, id).Scan(&exists); err != nil {
			return fmt.Errorf("platform_experiments_store.Reopen: %w", err)
		}
		if !exists {
			return ErrPlatformExperimentNotFound
		}
		return ErrNotReopenable
	}
	if _, err := tx.Exec(ctx, `DELETE FROM experiment_top3 WHERE platform_experiment_id = $1`, id); err != nil {
		return fmt.Errorf("platform_experiments_store.Reopen: placements: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.Reopen: commit: %w", err)
	}
	return nil
}

// unsettledJobsSQL says a job of pe still runs, waits to, or ended and its cost is not yet
// written to the metrics store (quota_settled_at): what pe spent is not yet whole.
const unsettledJobsSQL = `EXISTS (SELECT 1 FROM experiments e WHERE e.platform_experiment_id = pe.id
    AND (e.status IN ('QUEUED', 'SUBMITTED', 'RUNNING') OR e.quota_settled_at IS NULL))`

// JobsSettled says every job of the platform experiment id ended and its cost was written, so
// what it spent is whole.
func (s *PlatformExperimentsStore) JobsSettled(ctx context.Context, id string) (bool, error) {
	var settled bool
	if err := s.pool.pool.QueryRow(ctx, `SELECT NOT `+unsettledJobsSQL+` FROM platform_experiments pe WHERE pe.id = $1`, id).Scan(&settled); err != nil {
		return false, fmt.Errorf("platform_experiments_store.JobsSettled: %w", err)
	}
	return settled, nil
}

// ClosesToRecord is the platform experiments closed in the last within, after they were open,
// that have no record of the close yet and whose every job ended and was settled (JobsSettled): those whose
// record of the close waited for what they spent to be whole.
func (s *PlatformExperimentsStore) ClosesToRecord(ctx context.Context, within time.Duration) ([]string, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT pe.id FROM platform_experiments pe
WHERE pe.status = 'closed' AND pe.close_record = '' AND pe.closed_at > now() - make_interval(secs => $1)
  AND pe.close_reason IN ($2, $3, $4, $5) AND (pe.goal = '' OR pe.starts_at IS NOT NULL)
  AND NOT `+unsettledJobsSQL+`
ORDER BY pe.closed_at`, within.Seconds(), string(domain.CloseEndTimeReached), string(domain.CloseByRequest), string(domain.CloseBaselineLimitReached), string(domain.CloseAgentsNotStarted))
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ClosesToRecord: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ClosesToRecord: %w", err)
	}
	return ids, nil
}
