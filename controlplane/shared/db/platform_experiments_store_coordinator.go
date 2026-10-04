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
	// AgentImage is the image the platform experiment names for its machines, empty for the
	// platform's own, and EnvironmentImage that of its agent environment, empty without one.
	AgentImage       string
	EnvironmentImage string
	Ending           bool
	// EndsBy is the latest moment a machine rented for the coordinator now may still run
	// (launcherEndsBySQL).
	EndsBy time.Time
}

// coordinatorAskedSQL says the machine of pe's hosted coordinator was asked for at least once.
const coordinatorAskedSQL = `pe.coordinator_instance_asked_at IS NOT NULL`

// coordinatorWantedSQL says pe, open or running, has work for its hosted coordinator: its run
// started, or it waits for the coordinator (coordinator_reason, TellCoordinator). While its
// baseline is measured nobody needs it, and its machine would idle. Once asked for, the machine
// is kept until the close, so a reason that clears never cuts a turn short.
const coordinatorWantedSQL = `(pe.status = 'running' OR pe.coordinator_reason IS NOT NULL OR ` + coordinatorAskedSQL + `)`

// launcherCoordinatorsWhere selects the platform experiments whose coordinator a launcher still
// has something to do for: open or running ones that want it (coordinatorWantedSQL), and those
// closed less than $2 seconds ago whose coordinator's machine was asked for, on the database's
// clock. A stopped coordinator is among them, as a stopped agent is. None is while the platform
// still creates the repository (awaitsRepository).
const launcherCoordinatorsWhere = `
FROM platform_experiments pe
WHERE pe.coordinator_hosted AND pe.coordinator_launcher = $1 AND NOT (` + awaitsRepository + `)
  AND (pe.status IN ('open', 'running') AND ` + coordinatorWantedSQL + `
       OR pe.status = 'closed' AND pe.closed_at > now() - make_interval(secs => $2) AND ` + coordinatorAskedSQL + `)`

// ListLauncherCoordinators returns the hosted coordinators the launcher's desired state is
// decided from.
func (s *PlatformExperimentsStore) ListLauncherCoordinators(ctx context.Context, launcher string, windDown time.Duration) ([]LauncherCoordinator, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT pe.id, `+coordinatorColumns+`, pe.agent_image, `+agentEnvironmentImageSQL+`, pe.status = 'closed', `+launcherEndsBySQL+launcherCoordinatorsWhere+`
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
			&c.Instance.Renewals, &c.Instance.Stopped, &c.Instance.ExitedAt, &c.Instance.ExitLog, &c.Instance.SeenAt, &c.Instance.AskedAt, &c.Instance.Launch, &c.Instance.Reported, &c.Instance.ProgressAt, &c.Instance.EndReason, &c.Instance.Released, &c.AgentImage, &c.EnvironmentImage, &c.Ending, &c.EndsBy); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.ListLauncherCoordinators: %w", err)
		}
		coordinators = append(coordinators, c)
	}
	return coordinators, rows.Err()
}
