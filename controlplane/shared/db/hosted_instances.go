package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// InstanceEnd is why the machine asked for under a name is given up on. Its value is what the
// API shows as the instance's end_reason.
type InstanceEnd string

const (
	// InstanceExited: the launcher reported it as exited.
	InstanceExited InstanceEnd = "exited"
	// InstanceGone: the launcher reported it once and no longer does.
	InstanceGone InstanceEnd = "gone"
	// InstanceNeverSeen: the launcher has not reported it for the limit since it was asked for.
	// Inside the limit nothing is recorded.
	InstanceNeverSeen InstanceEnd = "never_seen"
	// InstanceStillStarting: the launcher has reported it as starting for the limit since it
	// first reported it. Inside the limit nothing is recorded.
	InstanceStillStarting InstanceEnd = "still_starting"
	// InstanceSilent: the launcher reports it as running, and what it reports of the machine's
	// progress has not changed for the limit. Inside the limit nothing is recorded.
	InstanceSilent InstanceEnd = "silent"
	// InstanceProgressUnreadable: the launcher reports it as running and could not read its
	// progress, and none was recorded for the limit, by the clock InstanceSilent is held to.
	// Whether its command works is not known. Inside the limit nothing is recorded.
	InstanceProgressUnreadable InstanceEnd = "progress_unreadable"
	// InstanceExpired: the launcher reported it as exited because its provider ended it at the
	// provider's own limit on how long it rents one. Its command did nothing wrong.
	InstanceExpired InstanceEnd = "expired"
	// InstanceReleased: the platform stopped asking for it because its run is over, and the
	// launcher took it down. Recorded by ReleaseInstances alone, and it spends no restart.
	InstanceReleased InstanceEnd = "released"
)

// SpendsRestart says whether a machine given up on this way counts against its agent's restarts.
// One that does not is a renewal: asked for again however many came before, and never what stops
// the agent. What bounds the machines rented that way is the run they are for: each is rented to
// end by itself at its platform experiment's latest end, and none is asked for after it.
func (e InstanceEnd) SpendsRestart() bool {
	return e != InstanceExpired && e != InstanceProgressUnreadable
}

// progressStampsPerLimit is how often inside one progress limit a machine's progress is written
// down. A machine that keeps reporting progress costs that many writes per limit and no more,
// and one that stops is ended at most a limit and one such part of it later.
const progressStampsPerLimit = 4

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
    `+c.prefix+`state = '', `+c.prefix+`progress = '', `+c.prefix+`progress_at = NULL,
    `+c.prefix+`activity = '', `+c.prefix+`activity_at = NULL,
    `+c.prefix+`restarts = CASE WHEN `+c.prefix+`launch = $4 THEN `+c.prefix+`restarts ELSE 0 END,
    `+c.prefix+`restarted_at = CASE WHEN `+c.prefix+`launch = $4 THEN `+c.prefix+`restarted_at ELSE '{}' END,
    `+c.prefix+`stopped = `+c.prefix+`stopped AND `+c.prefix+`launch = $4
WHERE `+c.key, platformExpID, agentID, name, launch)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.AskInstance: %w", err)
	}
	return nil
}

// ObserveInstance records what the launcher reports of the machine asked for under name while
// it lives: when it was first reported, the state it is in, its progress and what its agent says
// it is doing. progress is the marker the machine's command last printed, nil when the launcher
// could not read it. The time of progress is the database's own, taken when the marker differs
// from the one stored, and at most progressStampsPerLimit times per progressLimit. activity is
// the sentence the command last printed of what its agent does, empty when it printed none or
// could not be read, which leaves the one stored: it is kept with the time it was first read,
// the latest only. A report that changes nothing writes nothing. An agent's machine reported
// running starts the clock of a run that waited for it (startClock).
func (s *PlatformExperimentsStore) ObserveInstance(ctx context.Context, platformExpID, agentID, name, state string, progress *string, progressLimit time.Duration, activity string) error {
	c := instanceOf(agentID)
	stamp := `($5::text IS NOT NULL AND $5 <> ` + c.prefix + `progress AND (` + c.prefix + `progress_at IS NULL OR ` + c.prefix + `progress_at <= now() - make_interval(secs => $6)))`
	said := `($7 <> '' AND $7 <> ` + c.prefix + `activity)`
	_, err := s.pool.pool.Exec(ctx, `
UPDATE `+c.table+`
SET `+c.prefix+`seen_at = COALESCE(`+c.prefix+`seen_at, now()), `+c.prefix+`state = $4,
    `+c.prefix+`progress = CASE WHEN `+stamp+` THEN $5 ELSE `+c.prefix+`progress END,
    `+c.prefix+`progress_at = CASE WHEN `+stamp+` THEN now() ELSE `+c.prefix+`progress_at END,
    `+c.prefix+`activity_at = CASE WHEN `+said+` THEN now() ELSE `+c.prefix+`activity_at END,
    `+c.prefix+`activity = CASE WHEN `+said+` THEN $7 ELSE `+c.prefix+`activity END
WHERE `+c.key+` AND `+c.prefix+`name = $3 AND `+c.prefix+`name <> ''
  AND (`+c.prefix+`seen_at IS NULL OR `+c.prefix+`state <> $4 OR `+stamp+` OR `+said+`)`,
		platformExpID, agentID, name, state, progress, progressLimit.Seconds()/progressStampsPerLimit, activity)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.ObserveInstance: %w", err)
	}
	if agentID != "" && state == domain.InstanceRunning {
		return s.startClock(ctx, platformExpID, agentID, name)
	}
	return nil
}

// EndInstance records that the machine asked for under name is given up on, with exitLog as the
// end of what it wrote or the reason there is nothing. A restart is counted while fewer than
// maxRestarts were within the last domain.AgentInstanceRestartWindow, and the agent or
// coordinator is stopped otherwise. An end that spends no
// restart (InstanceEnd.SpendsRestart) counts a renewal and stops nothing. The name is cleared, so
// the same end reported again records nothing: recorded is false, as it is for a name that is
// not the one asked for and for an end whose limit has not passed on the database's clock. limit
// is the boot limit for a machine never seen or still starting, the progress limit for a silent
// one or one whose progress could not be read, and unused otherwise. after is the instance as the write left it.
func (s *PlatformExperimentsStore) EndInstance(ctx context.Context, platformExpID, agentID, name string, how InstanceEnd, exitLog string, limit time.Duration, maxRestarts int) (after domain.AgentInstance, recorded bool, err error) {
	c := instanceOf(agentID)
	// Every form names $6, so that one statement serves them all.
	var when string
	switch how {
	case InstanceExited, InstanceExpired:
		when = "$6::float8 IS NOT NULL"
	case InstanceGone:
		when = "$6::float8 IS NOT NULL AND " + c.prefix + "seen_at IS NOT NULL"
	case InstanceNeverSeen:
		when = c.prefix + "seen_at IS NULL AND " + c.prefix + "asked_at <= now() - make_interval(secs => $6)"
	case InstanceStillStarting:
		when = c.prefix + "seen_at <= now() - make_interval(secs => $6)"
	case InstanceSilent, InstanceProgressUnreadable:
		when = c.prefix + "state = 'running' AND COALESCE(" + c.prefix + "progress_at, " + c.prefix + "seen_at) <= now() - make_interval(secs => $6)"
	default:
		return after, false, fmt.Errorf("platform_experiments_store.EndInstance: unknown end %q", how)
	}
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return after, false, fmt.Errorf("platform_experiments_store.EndInstance: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	recent := `ARRAY(SELECT t FROM unnest(` + c.prefix + `restarted_at) t WHERE t > now() - make_interval(secs => $9))`
	restart := `$8::boolean AND cardinality(` + recent + `) < $5`
	err = tx.QueryRow(ctx, `
UPDATE `+c.table+`
SET `+c.prefix+`name = '', `+c.prefix+`exited_at = now(), `+c.prefix+`exit_log = $4, `+c.prefix+`end_reason = $7, `+c.prefix+`state = '',
    `+c.prefix+`activity = '', `+c.prefix+`activity_at = NULL,
    `+c.prefix+`stopped = $8::boolean AND cardinality(`+recent+`) >= $5,
    `+c.prefix+`restarts = `+c.prefix+`restarts + CASE WHEN `+restart+` THEN 1 ELSE 0 END,
    `+c.prefix+`restarted_at = CASE WHEN `+restart+` THEN `+recent+` || now() ELSE `+recent+` END,
    `+c.prefix+`renewals = `+c.prefix+`renewals + CASE WHEN $8::boolean THEN 0 ELSE 1 END
WHERE `+c.key+` AND NOT `+c.prefix+`stopped AND `+c.prefix+`name = $3 AND `+c.prefix+`name <> '' AND `+when+`
RETURNING `+c.prefix+`restarts, `+c.prefix+`renewals, `+c.prefix+`stopped, `+c.prefix+`exited_at, `+c.prefix+`exit_log, `+c.prefix+`launch, `+c.prefix+`end_reason`,
		platformExpID, agentID, name, exitLog, maxRestarts, limit.Seconds(), string(how), how.SpendsRestart(), domain.AgentInstanceRestartWindow.Seconds()).
		Scan(&after.Restarts, &after.Renewals, &after.Stopped, &after.ExitedAt, &after.ExitLog, &after.Launch, &after.EndReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return after, false, nil
	}
	if err != nil {
		return after, false, fmt.Errorf("platform_experiments_store.EndInstance: %w", err)
	}
	// A draft whose planner is given up on before it answered has nobody left to answer it: it is
	// closed with the stop, saying so, where it would have read planning until its limit.
	if after.Stopped && agentID != "" {
		if _, err := tx.Exec(ctx, `
UPDATE platform_experiments pe
SET status = 'closed', updated_at = now(), closed_at = now(), close_reason = '`+string(domain.ClosePlannerStopped)+`'
WHERE pe.id = $1 AND pe.status = 'draft' AND pe.proposed_at IS NULL
  AND EXISTS (SELECT 1 FROM experiment_signups s
              WHERE s.platform_experiment_id = pe.id AND s.agent_id = $2 AND s.role = '`+string(domain.SignupRolePlanner)+`')`,
			platformExpID, agentID); err != nil {
			return after, false, fmt.Errorf("platform_experiments_store.EndInstance: close the draft of a stopped planner: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return after, false, fmt.Errorf("platform_experiments_store.EndInstance: commit: %w", err)
	}
	return after, true, nil
}

// ReleaseInstances records, for every machine of launcher whose agent or coordinator the launcher
// is no longer asked to keep, that the platform released it and why: the name is cleared, the end
// is InstanceReleased and the exit log says which end of the run it was. The launcher takes such
// a machine down on its next answer, so without the record a later question about the same agent,
// a planner whose draft is revised, finds a machine that was seen and is gone and counts a
// machine the provider lost. No restart is spent. A pass with nothing to release writes nothing.
func (s *PlatformExperimentsStore) ReleaseInstances(ctx context.Context, launcher string, windDown time.Duration) error {
	sentences := map[string]string{}
	for _, end := range []domain.RunEnd{"", domain.RunEndClosed, domain.RunEndCut, domain.RunEndFinished, domain.RunEndPlanned} {
		sentences[string(end)] = end.ReleasedBecause()
	}
	because, err := json.Marshal(sentences)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.ReleaseInstances: %w", err)
	}
	if _, err := s.pool.pool.Exec(ctx, `
UPDATE experiment_signups s
SET instance_name = '', instance_state = '', instance_exited_at = now(), instance_end_reason = '`+string(InstanceReleased)+`',
    instance_activity = '', instance_activity_at = NULL,
    instance_exit_log = $3::jsonb ->> (`+runEndSQL+`)
WHERE s.hosted AND s.launcher = $1 AND s.instance_name <> ''
  AND (s.platform_experiment_id, s.agent_id) NOT IN (SELECT s.platform_experiment_id, s.agent_id`+launcherAgentsFrom+`)`,
		launcher, windDown.Seconds(), because); err != nil {
		return fmt.Errorf("platform_experiments_store.ReleaseInstances: agents: %w", err)
	}
	if _, err := s.pool.pool.Exec(ctx, `
UPDATE platform_experiments pe
SET coordinator_instance_name = '', coordinator_instance_state = '', coordinator_instance_exited_at = now(),
    coordinator_instance_activity = '', coordinator_instance_activity_at = NULL,
    coordinator_instance_end_reason = '`+string(InstanceReleased)+`', coordinator_instance_exit_log = $3
WHERE pe.coordinator_hosted AND pe.coordinator_launcher = $1 AND pe.coordinator_instance_name <> ''
  AND pe.id NOT IN (SELECT pe.id`+launcherCoordinatorsWhere+`)`,
		launcher, windDown.Seconds(), domain.RunEndClosed.ReleasedBecause()); err != nil {
		return fmt.Errorf("platform_experiments_store.ReleaseInstances: coordinators: %w", err)
	}
	return nil
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

// SetLauncherHolds records that a launcher was answered, now, and whether it holds machines after
// that question: holds is true when it listed any, or its answer asks for any.
func (s *PlatformExperimentsStore) SetLauncherHolds(ctx context.Context, name string, holds bool) error {
	if _, err := s.pool.pool.Exec(ctx, `UPDATE launchers SET holds_machines = $2, answered_at = now() WHERE name = $1`, name, holds); err != nil {
		return fmt.Errorf("platform_experiments_store.SetLauncherHolds: %w", err)
	}
	return nil
}

// LauncherAnsweredAt is when each launcher named was last answered: nil for one never answered,
// and absent for a name no launcher is registered under.
func (s *PlatformExperimentsStore) LauncherAnsweredAt(ctx context.Context, names []string) (map[string]*time.Time, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT name, answered_at FROM launchers WHERE name = ANY($1)`, names)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.LauncherAnsweredAt: %w", err)
	}
	defer rows.Close()
	out := map[string]*time.Time{}
	for rows.Next() {
		var name string
		var at *time.Time
		if err := rows.Scan(&name, &at); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.LauncherAnsweredAt: %w", err)
		}
		out[name] = at
	}
	return out, rows.Err()
}

// LauncherInUseError is a launcher that cannot be removed: its token is what takes its machines
// down, so it is kept while there is a machine to take down or an agent that may be given one.
type LauncherInUseError struct {
	Name string
	// PlatformExperiments are those not yet closed for domain.AgentWindDown that have a hosted
	// agent or coordinator of this launcher. With none, the launcher's last question still
	// listed machines or was answered with some.
	PlatformExperiments []string
}

func (e *LauncherInUseError) Error() string {
	if len(e.PlatformExperiments) > 0 {
		return fmt.Sprintf("launcher %s runs the hosted agents of platform experiments %s: close them or let them end, then let the launcher take their machines down",
			e.Name, strings.Join(e.PlatformExperiments, ", "))
	}
	return fmt.Sprintf("launcher %s holds machines, by its last question: keep it running until one of its passes lists none", e.Name)
}

// RemoveLauncher ends a launcher's token. It reports false for a name that was not registered,
// and refuses with a *LauncherInUseError while the launcher has something to take down: removed
// then, its machines would be nobody's.
func (s *PlatformExperimentsStore) RemoveLauncher(ctx context.Context, name string) (bool, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.RemoveLauncher: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var holds bool
	err = tx.QueryRow(ctx, `SELECT holds_machines FROM launchers WHERE name = $1 FOR UPDATE`, name).Scan(&holds)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.RemoveLauncher: lock launcher: %w", err)
	}
	rows, err := tx.Query(ctx, `
SELECT pe.id FROM platform_experiments pe
WHERE (pe.status <> 'closed' OR pe.closed_at > now() - make_interval(secs => $2))
  AND (pe.coordinator_hosted AND pe.coordinator_launcher = $1
       OR EXISTS (SELECT 1 FROM experiment_signups s WHERE s.platform_experiment_id = pe.id AND s.hosted AND s.launcher = $1))
ORDER BY pe.id`, name, domain.AgentWindDown.Seconds())
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.RemoveLauncher: list platform experiments: %w", err)
	}
	using, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.RemoveLauncher: list platform experiments: %w", err)
	}
	if len(using) > 0 || holds {
		return false, &LauncherInUseError{Name: name, PlatformExperiments: using}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM launchers WHERE name = $1`, name); err != nil {
		return false, fmt.Errorf("platform_experiments_store.RemoveLauncher: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("platform_experiments_store.RemoveLauncher: commit: %w", err)
	}
	return true, nil
}

// LauncherKnown reports whether a launcher of this name is registered.
func (s *PlatformExperimentsStore) LauncherKnown(ctx context.Context, name string) (bool, error) {
	var known bool
	if err := s.pool.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM launchers WHERE name = $1)`, name).Scan(&known); err != nil {
		return false, fmt.Errorf("platform_experiments_store.LauncherKnown: %w", err)
	}
	return known, nil
}
