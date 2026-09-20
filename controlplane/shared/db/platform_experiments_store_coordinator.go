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
// the database's clock. A stopped coordinator is among them, as a stopped agent is.
const launcherCoordinatorsWhere = `
FROM platform_experiments pe
WHERE pe.coordinator_hosted AND pe.coordinator_launcher = $1
  AND (pe.status IN ('open', 'running') OR (pe.status = 'closed' AND pe.closed_at > now() - make_interval(secs => $2)))`

// ListLauncherCoordinators returns the hosted coordinators the launcher's desired state is
// decided from.
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
			&c.Instance.Stopped, &c.Instance.ExitedAt, &c.Instance.ExitLog, &c.Instance.SeenAt, &c.Instance.Launch, &c.Ending); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.ListLauncherCoordinators: %w", err)
		}
		coordinators = append(coordinators, c)
	}
	return coordinators, rows.Err()
}
