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

// signupInstanceJoin joins experiment_signups s to the machine of its agent as i, NULL for an
// agent that never had one asked for.
const signupInstanceJoin = `
LEFT JOIN hosted_instances i ON i.platform_experiment_id = s.platform_experiment_id AND i.agent_id = s.agent_id`

// coordinatorInstanceJoin joins platform_experiments pe to the machine of its coordinator as ci.
const coordinatorInstanceJoin = `
LEFT JOIN hosted_instances ci ON ci.platform_experiment_id = pe.id AND ci.agent_id = ''`

// instanceColumns read the machine i of signupInstanceJoin as domain.AgentInstance holds it, what
// it holds before one is asked for where none was.
const instanceColumns = `COALESCE(i.name, ''), COALESCE(i.restarts, 0), COALESCE(i.renewals, 0), COALESCE(i.stopped, false), i.exited_at, COALESCE(i.exit_log, ''),
       i.seen_at, i.asked_at, COALESCE(i.launch, ''),
       COALESCE(i.state, ''), i.progress_at, COALESCE(i.end_reason, ''), COALESCE(i.activity, ''), i.activity_at`

// stoppedSQL is whether the machine of signup s stopped with no restart left.
const stoppedSQL = `EXISTS (SELECT 1 FROM hosted_instances stopped
        WHERE stopped.platform_experiment_id = s.platform_experiment_id AND stopped.agent_id = s.agent_id AND stopped.stopped)`

// hostedSQL is whether the platform runs the agent $2 of platform experiment $1, or with $2 empty
// its coordinator.
const hostedSQL = `(EXISTS (SELECT 1 FROM experiment_signups s WHERE s.platform_experiment_id = $1 AND s.agent_id = $2 AND s.hosted)
   OR $2 = '' AND EXISTS (SELECT 1 FROM platform_experiments pe WHERE pe.id = $1 AND pe.coordinator_hosted))`

// Hosted reports whether the platform runs the agent agentID of a platform experiment, or with no
// agentID its coordinator.
func (s *PlatformExperimentsStore) Hosted(ctx context.Context, platformExpID, agentID string) (bool, error) {
	var hosted bool
	if err := s.pool.pool.QueryRow(ctx, `SELECT `+hostedSQL, platformExpID, agentID).Scan(&hosted); err != nil {
		return false, fmt.Errorf("platform_experiments_store.Hosted: %w", err)
	}
	return hosted, nil
}

// AskInstance records the name a hosted agent's machine, or with no agentID a hosted
// coordinator's, is asked for under, and when. launch identifies what the machine is started
// with: under the launch the restart budget was spent under it stands, and under another the
// budget is whole and a stopped one is asked for again. restarts counts every one, ever. It
// records nothing for an agent or coordinator the platform does not run.
func (s *PlatformExperimentsStore) AskInstance(ctx context.Context, platformExpID, agentID, name, launch string) error {
	_, err := s.pool.pool.Exec(ctx, `
INSERT INTO hosted_instances AS i (platform_experiment_id, agent_id, name, launch, asked_at)
SELECT $1, $2, $3, $4, now()
WHERE `+hostedSQL+`
ON CONFLICT (platform_experiment_id, agent_id) DO UPDATE
SET name = $3, asked_at = now(), seen_at = NULL, launch = $4, machine = '',
    state = '', progress = '', progress_at = NULL, activity = '', activity_at = NULL,
    restarted_at = CASE WHEN i.launch = $4 THEN i.restarted_at ELSE '{}' END,
    stopped = i.stopped AND i.launch = $4`, platformExpID, agentID, name, launch)
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
// the latest only. machine is the provider's id of the machine the name was reported by, which
// ListLauncherMachines answers. A report that changes nothing writes nothing. An agent's machine
// reported running starts the clock of a run that waited for it (startClock).
func (s *PlatformExperimentsStore) ObserveInstance(ctx context.Context, platformExpID, agentID, name, state string, progress *string, progressLimit time.Duration, activity, machine string) error {
	if machine == "" {
		return fmt.Errorf("platform_experiments_store.ObserveInstance: %s is reported with no machine id", name)
	}
	const stamp = `($5::text IS NOT NULL AND $5 <> progress AND (progress_at IS NULL OR progress_at <= now() - make_interval(secs => $6)))`
	const said = `($7 <> '' AND $7 <> activity)`
	_, err := s.pool.pool.Exec(ctx, `
UPDATE hosted_instances
SET seen_at = COALESCE(seen_at, now()), state = $4, machine = $8,
    progress = CASE WHEN `+stamp+` THEN $5 ELSE progress END,
    progress_at = CASE WHEN `+stamp+` THEN now() ELSE progress_at END,
    activity_at = CASE WHEN `+said+` THEN now() ELSE activity_at END,
    activity = CASE WHEN `+said+` THEN $7 ELSE activity END
WHERE platform_experiment_id = $1 AND agent_id = $2 AND name = $3 AND name <> ''
  AND (seen_at IS NULL OR state <> $4 OR machine <> $8 OR `+stamp+` OR `+said+`)`,
		platformExpID, agentID, name, state, progress, progressLimit.Seconds()/progressStampsPerLimit, activity, machine)
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
	// Every form names $6, so that one statement serves them all.
	var when string
	switch how {
	case InstanceExited, InstanceExpired:
		when = "$6::float8 IS NOT NULL"
	case InstanceGone:
		when = "$6::float8 IS NOT NULL AND seen_at IS NOT NULL"
	case InstanceNeverSeen:
		when = "seen_at IS NULL AND asked_at <= now() - make_interval(secs => $6)"
	case InstanceStillStarting:
		when = "seen_at <= now() - make_interval(secs => $6)"
	case InstanceSilent, InstanceProgressUnreadable:
		when = "state = 'running' AND COALESCE(progress_at, seen_at) <= now() - make_interval(secs => $6)"
	default:
		return after, false, fmt.Errorf("platform_experiments_store.EndInstance: unknown end %q", how)
	}
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return after, false, fmt.Errorf("platform_experiments_store.EndInstance: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	const recent = `ARRAY(SELECT t FROM unnest(restarted_at) t WHERE t > now() - make_interval(secs => $9))`
	const restart = `$8::boolean AND cardinality(` + recent + `) < $5`
	err = tx.QueryRow(ctx, `
UPDATE hosted_instances
SET name = '', machine = '', exited_at = now(), exit_log = $4, end_reason = $7, state = '',
    activity = '', activity_at = NULL,
    stopped = $8::boolean AND cardinality(`+recent+`) >= $5,
    restarts = restarts + CASE WHEN `+restart+` THEN 1 ELSE 0 END,
    restarted_at = CASE WHEN `+restart+` THEN `+recent+` || now() ELSE `+recent+` END,
    renewals = renewals + CASE WHEN $8::boolean THEN 0 ELSE 1 END
WHERE platform_experiment_id = $1 AND agent_id = $2 AND NOT stopped AND name = $3 AND name <> '' AND `+when+`
RETURNING restarts, renewals, stopped, exited_at, exit_log, launch, end_reason`,
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
// An agent's machine is kept windDown after its run ended, a coordinator's coordinatorWindDown.
func (s *PlatformExperimentsStore) ReleaseInstances(ctx context.Context, launcher string, windDown, coordinatorWindDown time.Duration) error {
	sentences := map[string]string{}
	for _, end := range []domain.RunEnd{"", domain.RunEndClosed, domain.RunEndCut, domain.RunEndFinished, domain.RunEndPlanned} {
		sentences[string(end)] = end.ReleasedBecause()
	}
	because, err := json.Marshal(sentences)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.ReleaseInstances: %w", err)
	}
	const released = `name = '', machine = '', state = '', exited_at = now(), end_reason = '` + string(InstanceReleased) + `',
    activity = '', activity_at = NULL`
	if _, err := s.pool.pool.Exec(ctx, `
UPDATE hosted_instances i
SET `+released+`, exit_log = $3::jsonb ->> (`+runEndSQL+`)
FROM experiment_signups s
WHERE s.platform_experiment_id = i.platform_experiment_id AND s.agent_id = i.agent_id
  AND s.hosted AND s.launcher = $1 AND i.name <> ''
  AND (s.platform_experiment_id, s.agent_id) NOT IN (SELECT s.platform_experiment_id, s.agent_id`+launcherAgentsFrom+`)`,
		launcher, windDown.Seconds(), because); err != nil {
		return fmt.Errorf("platform_experiments_store.ReleaseInstances: agents: %w", err)
	}
	if _, err := s.pool.pool.Exec(ctx, `
UPDATE hosted_instances i
SET `+released+`, exit_log = $3
FROM platform_experiments pe
WHERE pe.id = i.platform_experiment_id AND i.agent_id = ''
  AND pe.coordinator_hosted AND pe.coordinator_launcher = $1 AND i.name <> ''
  AND pe.id NOT IN (SELECT pe.id`+launcherCoordinatorsWhere+`)`,
		launcher, coordinatorWindDown.Seconds(), domain.RunEndClosed.ReleasedBecause()); err != nil {
		return fmt.Errorf("platform_experiments_store.ReleaseInstances: coordinators: %w", err)
	}
	return nil
}

// LauncherMachine is a machine the platform holds for a launcher: the name it is asked for under
// and the provider's id the launcher last reported that name by.
type LauncherMachine struct {
	Name    string
	Machine string
}

// ListLauncherMachines returns every machine of launcher, an agent's or a coordinator's, whose
// name is still held and was reported with a machine id, by name. It writes nothing.
func (s *PlatformExperimentsStore) ListLauncherMachines(ctx context.Context, launcher string) ([]LauncherMachine, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT i.name, i.machine FROM hosted_instances i
LEFT JOIN experiment_signups s ON s.platform_experiment_id = i.platform_experiment_id AND s.agent_id = i.agent_id
LEFT JOIN platform_experiments pe ON pe.id = i.platform_experiment_id AND i.agent_id = ''
WHERE i.name <> '' AND i.machine <> ''
  AND (s.hosted AND s.launcher = $1 OR pe.coordinator_hosted AND pe.coordinator_launcher = $1)
ORDER BY 1`, launcher)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListLauncherMachines: %w", err)
	}
	machines, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (LauncherMachine, error) {
		var m LauncherMachine
		err := row.Scan(&m.Name, &m.Machine)
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListLauncherMachines: %w", err)
	}
	return machines, nil
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

// SetLauncherHolds records that a launcher was answered, now, the version it said it runs, and
// whether it holds machines after that question: holds is true when it listed any, or its answer
// asks for any.
func (s *PlatformExperimentsStore) SetLauncherHolds(ctx context.Context, name string, holds bool, agentVersion string) error {
	if _, err := s.pool.pool.Exec(ctx, `UPDATE launchers SET holds_machines = $2, answered_at = now(), agent_version = $3 WHERE name = $1`, name, holds, agentVersion); err != nil {
		return fmt.Errorf("platform_experiments_store.SetLauncherHolds: %w", err)
	}
	return nil
}

// LauncherContact is a launcher's last answered question: when, nil for one never answered, and
// the version its agent said it runs then, empty from one that said none.
type LauncherContact struct {
	At           *time.Time
	AgentVersion string
}

// LauncherContacts is each launcher named's last contact, absent for a name no launcher is
// registered under.
func (s *PlatformExperimentsStore) LauncherContacts(ctx context.Context, names []string) (map[string]LauncherContact, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT name, answered_at, agent_version FROM launchers WHERE name = ANY($1)`, names)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.LauncherContacts: %w", err)
	}
	defer rows.Close()
	out := map[string]LauncherContact{}
	for rows.Next() {
		var name string
		var contact LauncherContact
		if err := rows.Scan(&name, &contact.At, &contact.AgentVersion); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.LauncherContacts: %w", err)
		}
		out[name] = contact
	}
	return out, rows.Err()
}

// LauncherInUseError is a launcher that cannot be removed: its token is what takes its machines
// down, so it is kept while there is a machine to take down or an agent that may be given one.
type LauncherInUseError struct {
	Name string
	// PlatformExperiments are those that have a hosted agent or coordinator of this launcher and
	// did not close more than its wind-down ago. With none, the launcher's last question still
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
WHERE pe.coordinator_hosted AND pe.coordinator_launcher = $1 AND (pe.status <> 'closed' OR pe.closed_at > now() - make_interval(secs => $3))
   OR EXISTS (SELECT 1 FROM experiment_signups s WHERE s.platform_experiment_id = pe.id AND s.hosted AND s.launcher = $1)
      AND (pe.status <> 'closed' OR pe.closed_at > now() - make_interval(secs => $2))
ORDER BY pe.id`, name, domain.AgentWindDown.Seconds(), domain.CoordinatorWindDown.Seconds())
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
