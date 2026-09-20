package db

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// LauncherCoordinator is a hosted coordinator as its launcher's desired state names it. Ending
// is true once its platform experiment closed: its machine is no longer wanted and is left to
// finish its last turn.
type LauncherCoordinator struct {
	PlatformExperimentID string
	domain.HostedCoordinator
	Ending bool
}

// launcherCoordinatorsWhere selects the platform experiments whose coordinator a launcher still
// has something to do for: open or running ones, and those closed less than $2 seconds ago, on
// the database's clock. A coordinator whose machine exited for the last time is none of them.
const launcherCoordinatorsWhere = `
FROM platform_experiments pe
WHERE pe.coordinator_hosted AND pe.coordinator_launcher = $1 AND NOT pe.coordinator_instance_stopped
  AND (pe.status IN ('open', 'running') OR (pe.status = 'closed' AND pe.closed_at > now() - make_interval(secs => $2)))`

// ListLauncherCoordinators returns the hosted coordinators whose machine the launcher keeps or
// leaves alone right now.
func (s *PlatformExperimentsStore) ListLauncherCoordinators(ctx context.Context, launcher string, windDown time.Duration) ([]LauncherCoordinator, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT pe.id, `+coordinatorColumns+`, pe.status = 'closed'`+launcherCoordinatorsWhere+`
ORDER BY pe.id`, launcher, windDown.Seconds())
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListLauncherCoordinators: %w", err)
	}
	defer rows.Close()
	coordinators := []LauncherCoordinator{}
	for rows.Next() {
		var c LauncherCoordinator
		var hosted bool
		if err := rows.Scan(&c.PlatformExperimentID, &hosted, &c.Launcher, &c.Model, &c.Instance.Name, &c.Instance.Restarts,
			&c.Instance.Stopped, &c.Instance.ExitedAt, &c.Instance.ExitLog, &c.Ending); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.ListLauncherCoordinators: %w", err)
		}
		coordinators = append(coordinators, c)
	}
	return coordinators, rows.Err()
}

// LauncherHasCoordinators reports whether ListLauncherCoordinators would return anything.
func (s *PlatformExperimentsStore) LauncherHasCoordinators(ctx context.Context, launcher string, windDown time.Duration) (bool, error) {
	var has bool
	if err := s.pool.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1`+launcherCoordinatorsWhere+`)`, launcher, windDown.Seconds()).Scan(&has); err != nil {
		return false, fmt.Errorf("platform_experiments_store.LauncherHasCoordinators: %w", err)
	}
	return has, nil
}

// SetCoordinatorInstanceName records the name a hosted coordinator's machine is asked for under,
// and when.
func (s *PlatformExperimentsStore) SetCoordinatorInstanceName(ctx context.Context, platformExpID, name string) error {
	if _, err := s.pool.pool.Exec(ctx, `
UPDATE platform_experiments SET coordinator_instance_name = $2, coordinator_instance_asked_at = now()
WHERE id = $1 AND coordinator_hosted`, platformExpID, name); err != nil {
		return fmt.Errorf("platform_experiments_store.SetCoordinatorInstanceName: %w", err)
	}
	return nil
}

// RecordCoordinatorInstanceExit is RecordAgentInstanceExit for a hosted coordinator's machine.
func (s *PlatformExperimentsStore) RecordCoordinatorInstanceExit(ctx context.Context, platformExpID, name, exitLog string, maxRestarts int) (bool, error) {
	return s.endCoordinatorInstance(ctx, platformExpID, name, exitLog, maxRestarts, 0)
}

// RecordCoordinatorInstanceStuck is RecordAgentInstanceStuck for a hosted coordinator's machine.
func (s *PlatformExperimentsStore) RecordCoordinatorInstanceStuck(ctx context.Context, platformExpID, name, why string, bootLimit time.Duration, maxRestarts int) (bool, error) {
	return s.endCoordinatorInstance(ctx, platformExpID, name, why, maxRestarts, bootLimit)
}

func (s *PlatformExperimentsStore) endCoordinatorInstance(ctx context.Context, platformExpID, name, exitLog string, maxRestarts int, askedBefore time.Duration) (bool, error) {
	tag, err := s.pool.pool.Exec(ctx, `
UPDATE platform_experiments
SET coordinator_instance_name = '', coordinator_instance_exited_at = now(), coordinator_instance_exit_log = $3,
    coordinator_instance_stopped = coordinator_instance_restarts >= $4,
    coordinator_instance_restarts = CASE WHEN coordinator_instance_restarts >= $4 THEN coordinator_instance_restarts ELSE coordinator_instance_restarts + 1 END
WHERE id = $1 AND coordinator_hosted AND NOT coordinator_instance_stopped
  AND coordinator_instance_name = $2 AND coordinator_instance_name <> ''
  AND coordinator_instance_asked_at <= now() - make_interval(secs => $5)`, platformExpID, name, exitLog, maxRestarts, askedBefore.Seconds())
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store: end coordinator instance of %s: %w", platformExpID, err)
	}
	return tag.RowsAffected() == 1, nil
}
