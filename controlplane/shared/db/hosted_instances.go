package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// InstanceEnd is why the machine asked for under a name is given up on.
type InstanceEnd int

const (
	// InstanceExited: the launcher reported it as exited.
	InstanceExited InstanceEnd = iota
	// InstanceGone: the launcher reported it once and no longer does.
	InstanceGone
	// InstanceNeverSeen: the launcher has not reported it for the boot limit since it was asked
	// for. Inside the limit nothing is recorded.
	InstanceNeverSeen
	// InstanceStillStarting: the launcher has reported it as starting for the boot limit since
	// it first reported it. Inside the limit nothing is recorded.
	InstanceStillStarting
)

// instanceColumns is where a hosted agent or a hosted coordinator keeps what became of its
// machine. Both are evaluated by the same statements. key takes the platform experiment as $1
// and the agent as $2, which is empty for a coordinator.
type instanceColumns struct {
	table, key, prefix string
}

var (
	agentInstance       = instanceColumns{"experiment_signups", "platform_experiment_id = $1 AND agent_id = $2 AND hosted", "instance_"}
	coordinatorInstance = instanceColumns{"platform_experiments", "id = $1 AND $2 = '' AND coordinator_hosted", "coordinator_instance_"}
)

func instanceOf(agentID string) instanceColumns {
	if agentID == "" {
		return coordinatorInstance
	}
	return agentInstance
}

// AskInstance records the name a hosted agent's machine, or with no agentID a hosted
// coordinator's, is asked for under, and when. launch identifies what the machine is started
// with: under the launch the restarts were spent under they stand, and under another none is
// spent and a stopped one is asked for again.
func (s *PlatformExperimentsStore) AskInstance(ctx context.Context, platformExpID, agentID, name, launch string) error {
	c := instanceOf(agentID)
	_, err := s.pool.pool.Exec(ctx, `
UPDATE `+c.table+`
SET `+c.prefix+`name = $3, `+c.prefix+`asked_at = now(), `+c.prefix+`seen_at = NULL, `+c.prefix+`launch = $4,
    `+c.prefix+`restarts = CASE WHEN `+c.prefix+`launch = $4 THEN `+c.prefix+`restarts ELSE 0 END,
    `+c.prefix+`stopped = `+c.prefix+`stopped AND `+c.prefix+`launch = $4
WHERE `+c.key, platformExpID, agentID, name, launch)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.AskInstance: %w", err)
	}
	return nil
}

// SeeInstance records the first time the launcher reported the machine asked for under name.
func (s *PlatformExperimentsStore) SeeInstance(ctx context.Context, platformExpID, agentID, name string) error {
	c := instanceOf(agentID)
	_, err := s.pool.pool.Exec(ctx, `
UPDATE `+c.table+` SET `+c.prefix+`seen_at = now()
WHERE `+c.key+` AND `+c.prefix+`name = $3 AND `+c.prefix+`name <> '' AND `+c.prefix+`seen_at IS NULL`, platformExpID, agentID, name)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.SeeInstance: %w", err)
	}
	return nil
}

// EndInstance records that the machine asked for under name is given up on, with exitLog as the
// end of what it wrote or the reason there is nothing. A restart is counted while fewer than
// maxRestarts were, and the agent or coordinator is stopped otherwise. The name is cleared, so
// the same end reported again records nothing: recorded is false, as it is for a name that is
// not the one asked for and for an end whose boot limit has not passed on the database's clock.
// after is the instance as the write left it.
func (s *PlatformExperimentsStore) EndInstance(ctx context.Context, platformExpID, agentID, name string, how InstanceEnd, exitLog string, bootLimit time.Duration, maxRestarts int) (after domain.AgentInstance, recorded bool, err error) {
	c := instanceOf(agentID)
	// Every form names $6, so that one statement serves them all.
	var when string
	switch how {
	case InstanceExited:
		when = "$6::float8 IS NOT NULL"
	case InstanceGone:
		when = "$6::float8 IS NOT NULL AND " + c.prefix + "seen_at IS NOT NULL"
	case InstanceNeverSeen:
		when = c.prefix + "seen_at IS NULL AND " + c.prefix + "asked_at <= now() - make_interval(secs => $6)"
	case InstanceStillStarting:
		when = c.prefix + "seen_at <= now() - make_interval(secs => $6)"
	default:
		return after, false, fmt.Errorf("platform_experiments_store.EndInstance: unknown end %d", how)
	}
	err = s.pool.pool.QueryRow(ctx, `
UPDATE `+c.table+`
SET `+c.prefix+`name = '', `+c.prefix+`exited_at = now(), `+c.prefix+`exit_log = $4,
    `+c.prefix+`stopped = `+c.prefix+`restarts >= $5,
    `+c.prefix+`restarts = CASE WHEN `+c.prefix+`restarts >= $5 THEN `+c.prefix+`restarts ELSE `+c.prefix+`restarts + 1 END
WHERE `+c.key+` AND NOT `+c.prefix+`stopped AND `+c.prefix+`name = $3 AND `+c.prefix+`name <> '' AND `+when+`
RETURNING `+c.prefix+`restarts, `+c.prefix+`stopped, `+c.prefix+`exited_at, `+c.prefix+`exit_log, `+c.prefix+`launch`,
		platformExpID, agentID, name, exitLog, maxRestarts, bootLimit.Seconds()).
		Scan(&after.Restarts, &after.Stopped, &after.ExitedAt, &after.ExitLog, &after.Launch)
	if errors.Is(err, pgx.ErrNoRows) {
		return after, false, nil
	}
	if err != nil {
		return after, false, fmt.Errorf("platform_experiments_store.EndInstance: %w", err)
	}
	return after, true, nil
}

// RegisterLauncher makes an agent-launcher's token work. Registering one again changes nothing.
func (s *PlatformExperimentsStore) RegisterLauncher(ctx context.Context, name string) error {
	if err := domain.ValidateLauncherName(name); err != nil {
		return err
	}
	if _, err := s.pool.pool.Exec(ctx, `INSERT INTO launchers (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`, name); err != nil {
		return fmt.Errorf("platform_experiments_store.RegisterLauncher: %w", err)
	}
	return nil
}

// RemoveLauncher ends a launcher's token. It reports false for a name that was not registered.
// The machines it still holds are no longer the platform's to take down: empty it first by
// naming another launcher in config and waiting for its list to be empty.
func (s *PlatformExperimentsStore) RemoveLauncher(ctx context.Context, name string) (bool, error) {
	tag, err := s.pool.pool.Exec(ctx, `DELETE FROM launchers WHERE name = $1`, name)
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.RemoveLauncher: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// LauncherKnown reports whether a launcher of this name is registered.
func (s *PlatformExperimentsStore) LauncherKnown(ctx context.Context, name string) (bool, error) {
	var known bool
	if err := s.pool.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM launchers WHERE name = $1)`, name).Scan(&known); err != nil {
		return false, fmt.Errorf("platform_experiments_store.LauncherKnown: %w", err)
	}
	return known, nil
}
