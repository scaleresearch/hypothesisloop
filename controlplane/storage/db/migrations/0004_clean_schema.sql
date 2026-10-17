-- One clean schema (snapshots/0004.sql), converted from 0003 in place with every row of a real
-- run kept. It breaks the previous release: deploy it with the release that reads it.

DO $$
BEGIN
    IF to_regclass('hypotheses') IS NOT NULL OR to_regclass('human_writes') IS NOT NULL THEN
        RAISE EXCEPTION 'the legacy tables hypotheses or human_writes are still here: start the previous release once to move them, then this one';
    END IF;
END $$;

-- Statuses and tiers: enums become TEXT with a CHECK. What refers to the enum values is dropped
-- and made again over the text.
ALTER TABLE experiments
    DROP CONSTRAINT experiments_queue_reason_consistent,
    DROP CONSTRAINT experiments_reason_of_attempt,
    DROP CONSTRAINT experiments_active_has_data_key;
DROP INDEX idx_experiments_desired;
DROP INDEX idx_experiments_unsettled;
ALTER TABLE experiments
    ALTER COLUMN status TYPE TEXT USING status::text,
    ALTER COLUMN capacity_tier DROP DEFAULT,
    ALTER COLUMN capacity_tier TYPE TEXT USING capacity_tier::text,
    ALTER COLUMN capacity_tier SET DEFAULT 'guaranteed',
    ADD CONSTRAINT experiments_status
        CHECK (status IN ('SUBMITTED', 'QUEUED', 'RUNNING', 'COMPLETED', 'FAILED', 'EVICTED', 'REJECTED')),
    ADD CONSTRAINT experiments_capacity_tier CHECK (capacity_tier IN ('guaranteed', 'burst')),
    ADD CONSTRAINT experiments_queue_reason_consistent
        CHECK ((status = 'QUEUED') = (not_admitted_reason IS NOT NULL)),
    ADD CONSTRAINT experiments_reason_of_attempt
        CHECK (eviction_reason IS NULL OR status NOT IN ('SUBMITTED', 'RUNNING', 'COMPLETED')),
    ADD CONSTRAINT experiments_active_has_data_key
        CHECK (data_access_key_id <> '' OR status IN ('COMPLETED', 'FAILED', 'EVICTED', 'REJECTED'));
CREATE INDEX idx_experiments_desired ON experiments(cluster_name, status)
    WHERE status IN ('SUBMITTED', 'RUNNING');
CREATE INDEX idx_experiments_unsettled  ON experiments(updated_at)
    WHERE quota_settled_at IS NULL AND status IN ('COMPLETED', 'FAILED', 'EVICTED', 'REJECTED');

ALTER TABLE platform_experiments
    ALTER COLUMN status DROP DEFAULT,
    ALTER COLUMN status TYPE TEXT USING status::text,
    ALTER COLUMN status SET DEFAULT 'draft',
    ADD CONSTRAINT platform_experiments_status CHECK (status IN ('draft', 'open', 'running', 'closed'));

DROP TYPE experiment_status;
DROP TYPE platform_experiment_status;
DROP TYPE capacity_tier;

-- Columns nothing reads: a score every agent held at 0.5, and the source of a comment, which its
-- actor and agent_id say.
ALTER TABLE agents DROP COLUMN performance_score;
ALTER TABLE hypothesis_comments DROP COLUMN source;

-- Actors. A required actor of a record from before actors were kept is unverified, as it read.
UPDATE platform_experiments SET created_by_kind = 'unverified' WHERE created_by_kind = '';
UPDATE experiment_signups SET added_by_kind = 'unverified' WHERE added_by_kind = '';
UPDATE agent_types SET created_by_kind = 'unverified' WHERE created_by_kind = '';
-- A version is immutable to everything but this backfill of who wrote it.
ALTER TABLE agent_type_versions DISABLE TRIGGER agent_type_versions_immutable;
UPDATE agent_type_versions SET created_by_kind = 'unverified' WHERE created_by_kind = '';
ALTER TABLE agent_type_versions ENABLE TRIGGER agent_type_versions_immutable;
UPDATE org_github_installations SET connected_by_kind = 'unverified' WHERE connected_by_kind = '';

ALTER TABLE platform_experiments
    ALTER COLUMN created_by_kind DROP DEFAULT,
    ADD CONSTRAINT platform_experiments_created_by_kind
        CHECK (created_by_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified')),
    ADD CONSTRAINT platform_experiments_accepted_by_kind
        CHECK (accepted_by_kind IN ('', 'user', 'key', 'agent', 'coordinator', 'staff', 'unverified')),
    ADD CONSTRAINT platform_experiments_closed_by_kind
        CHECK (closed_by_kind IN ('', 'user', 'key', 'agent', 'coordinator', 'staff', 'unverified'));
ALTER TABLE experiment_signups
    ALTER COLUMN added_by_kind DROP DEFAULT,
    ADD CONSTRAINT experiment_signups_added_by_kind
        CHECK (added_by_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'));
ALTER TABLE agent_types
    ALTER COLUMN created_by_kind DROP DEFAULT,
    ADD CONSTRAINT agent_types_created_by_kind
        CHECK (created_by_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'));
ALTER TABLE agent_type_versions
    ALTER COLUMN created_by_kind DROP DEFAULT,
    ADD CONSTRAINT agent_type_versions_created_by_kind
        CHECK (created_by_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'));
ALTER TABLE github_install_states
    ADD CONSTRAINT github_install_states_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'));
ALTER TABLE planning_requests
    ADD CONSTRAINT planning_requests_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'));
ALTER TABLE agent_messages
    ADD CONSTRAINT agent_messages_sender CHECK (sender IN ('coordinator', 'owner')),
    ADD CONSTRAINT agent_messages_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'));
ALTER TABLE experiments
    ADD CONSTRAINT experiments_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'));
ALTER TABLE hypothesis_findings
    ADD CONSTRAINT hypothesis_findings_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'));
ALTER TABLE hypothesis_comments
    ADD CONSTRAINT hypothesis_comments_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'));

-- disabled_by held a user's or key's id, or the kind of a caller with none.
ALTER TABLE org_disabled_clusters
    ADD COLUMN disabled_by_kind TEXT,
    ADD COLUMN disabled_by_id   TEXT NOT NULL DEFAULT '';
UPDATE org_disabled_clusters d SET
    disabled_by_kind = CASE
        WHEN d.disabled_by IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified') THEN d.disabled_by
        WHEN EXISTS (SELECT 1 FROM users u WHERE u.id = d.disabled_by) THEN 'user'
        WHEN EXISTS (SELECT 1 FROM api_keys k WHERE k.id = d.disabled_by) THEN 'key'
        ELSE 'unverified' END,
    disabled_by_id = CASE
        WHEN d.disabled_by IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified') THEN ''
        ELSE d.disabled_by END;
ALTER TABLE org_disabled_clusters
    ALTER COLUMN disabled_by_kind SET NOT NULL,
    DROP COLUMN disabled_by,
    ADD CONSTRAINT org_disabled_clusters_disabled_by_kind
        CHECK (disabled_by_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'));

-- The type of an account is the account's: an installation connected before it was kept takes it
-- from another org's connection of the same installation.
UPDATE org_github_installations i SET account_type = known.account_type
FROM (SELECT DISTINCT ON (installation_id) installation_id, account_type FROM org_github_installations
      WHERE account_type <> '' ORDER BY installation_id, connected_at DESC) known
WHERE known.installation_id = i.installation_id AND i.account_type = '';
ALTER TABLE org_github_installations RENAME COLUMN connected_by TO connected_by_id;
ALTER TABLE org_github_installations
    ALTER COLUMN connected_by_kind DROP DEFAULT,
    ALTER COLUMN account_type DROP DEFAULT,
    ADD CONSTRAINT org_github_installations_account_type CHECK (account_type IN ('', 'User', 'Organization')),
    ADD CONSTRAINT org_github_installations_connected_by_kind
        CHECK (connected_by_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'));

-- Quota tiers are stored resolved: '' read as the platform experiment's default, and an
-- experiment's '' as guaranteed.
UPDATE platform_experiments SET default_quota_tier = 'guaranteed' WHERE default_quota_tier = '';
UPDATE experiment_signups s SET quota_tier = pe.default_quota_tier
FROM platform_experiments pe WHERE pe.id = s.platform_experiment_id AND s.quota_tier = '';
ALTER TABLE platform_experiments
    ALTER COLUMN default_quota_tier DROP DEFAULT,
    DROP CONSTRAINT platform_experiments_default_quota_tier,
    ADD CONSTRAINT platform_experiments_default_quota_tier CHECK (default_quota_tier IN ('guaranteed', 'burst_only'));
ALTER TABLE experiment_signups
    ALTER COLUMN quota_tier DROP DEFAULT,
    DROP CONSTRAINT experiment_signups_quota_tier,
    ADD CONSTRAINT experiment_signups_quota_tier CHECK (quota_tier IN ('guaranteed', 'burst_only'));

-- A run with no clock held the zero time.
ALTER TABLE platform_experiments ALTER COLUMN starts_at DROP NOT NULL;
UPDATE platform_experiments SET starts_at = NULL WHERE starts_at < '0002-01-01';

-- A revision requested before its transaction was kept replays at position 0.
UPDATE planning_requests SET created_xid = 0 WHERE created_xid IS NULL;
ALTER TABLE planning_requests ALTER COLUMN created_xid SET NOT NULL;

-- An environment built before its repository was kept takes it from the first job that ran in it,
-- or else an agent that waited for it; one neither names is dropped.
UPDATE environments e SET project_id = used.project_id, git_repository = used.repository
FROM (SELECT DISTINCT ON (x.environment_key) x.environment_key, pe.project_id, pe.repository
      FROM experiments x JOIN platform_experiments pe ON pe.id = x.platform_experiment_id
      WHERE x.environment_key IS NOT NULL ORDER BY x.environment_key, x.created_at, x.id) used
WHERE used.environment_key = e.key AND e.project_id = '';
UPDATE environments e SET project_id = waited.project_id, git_repository = waited.repository
FROM (SELECT DISTINCT ON (w.environment_key) w.environment_key, pe.project_id, pe.repository
      FROM environment_waiters w JOIN platform_experiments pe ON pe.id = w.platform_experiment_id
      ORDER BY w.environment_key, pe.created_at, pe.id) waited
WHERE waited.environment_key = e.key AND e.project_id = '';
DELETE FROM environments WHERE project_id = '';
ALTER TABLE environments
    ALTER COLUMN project_id DROP DEFAULT,
    ALTER COLUMN git_repository DROP DEFAULT,
    ADD CONSTRAINT environments_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects (id);

-- Foreign keys on what named rows with none. Rows naming no row are dropped first.
DELETE FROM environment_waiters w WHERE NOT EXISTS (SELECT 1 FROM agents a WHERE a.id = w.agent_id);
ALTER TABLE environment_waiters
    ADD CONSTRAINT environment_waiters_agent_id_fkey FOREIGN KEY (agent_id) REFERENCES agents (id);
DELETE FROM baseline_raises r
WHERE NOT EXISTS (SELECT 1 FROM experiments x WHERE x.id = r.from_job_id)
   OR NOT EXISTS (SELECT 1 FROM experiments x WHERE x.id = r.to_job_id);
ALTER TABLE baseline_raises
    ADD CONSTRAINT baseline_raises_from_job_id_fkey FOREIGN KEY (from_job_id) REFERENCES experiments(id),
    ADD CONSTRAINT baseline_raises_to_job_id_fkey FOREIGN KEY (to_job_id) REFERENCES experiments(id),
    ADD CONSTRAINT baseline_raises_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'));

-- A signup's agent type, which an agent from before agent types ran as the default.
UPDATE experiment_signups SET flavor = 'generalist' WHERE flavor = '';
ALTER TABLE experiment_signups RENAME COLUMN flavor TO agent_type;
ALTER TABLE experiment_signups
    ALTER COLUMN agent_type DROP DEFAULT,
    ADD CONSTRAINT experiment_signups_role CHECK (role IN ('competitor', 'baseline', 'reviewer', 'planner'));

CREATE OR REPLACE FUNCTION hypothesisloop_agent_instructions_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('agent.instructions', NEW.agent_id,
        NEW.agent_type_version::text, NEW.agent_type, NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- The machines of hosted agents and coordinators move to one table of their own.
CREATE TABLE hosted_instances (
    platform_experiment_id TEXT          NOT NULL REFERENCES platform_experiments(id),
    agent_id               TEXT          NOT NULL,
    name                   TEXT          NOT NULL DEFAULT '',
    launch                 TEXT          NOT NULL DEFAULT '',
    asked_at               TIMESTAMPTZ,
    seen_at                TIMESTAMPTZ,
    machine                TEXT          NOT NULL DEFAULT '',
    state                  TEXT          NOT NULL DEFAULT '',
    progress               TEXT          NOT NULL DEFAULT '',
    progress_at            TIMESTAMPTZ,
    activity               TEXT          NOT NULL DEFAULT '',
    activity_at            TIMESTAMPTZ,
    restarts               INTEGER       NOT NULL DEFAULT 0,
    restarted_at           TIMESTAMPTZ[] NOT NULL DEFAULT '{}',
    renewals               INTEGER       NOT NULL DEFAULT 0,
    stopped                BOOLEAN       NOT NULL DEFAULT false,
    exited_at              TIMESTAMPTZ,
    exit_log               TEXT          NOT NULL DEFAULT '',
    end_reason             TEXT          NOT NULL DEFAULT '',
    PRIMARY KEY (platform_experiment_id, agent_id),
    CONSTRAINT hosted_instances_state CHECK (state IN ('', 'starting', 'running')),
    CONSTRAINT hosted_instances_end_reason CHECK (end_reason IN (
        '', 'exited', 'gone', 'never_seen', 'still_starting', 'silent', 'progress_unreadable', 'expired', 'released'
    ))
);
INSERT INTO hosted_instances (platform_experiment_id, agent_id, name, launch, asked_at, seen_at, machine, state, progress, progress_at,
                              activity, activity_at, restarts, restarted_at, renewals, stopped, exited_at, exit_log, end_reason)
SELECT platform_experiment_id, agent_id, instance_name, instance_launch, instance_asked_at, instance_seen_at, instance_machine,
       instance_state, instance_progress, instance_progress_at, instance_activity, instance_activity_at, instance_restarts,
       instance_restarted_at, instance_renewals, instance_stopped, instance_exited_at, instance_exit_log, instance_end_reason
FROM experiment_signups
WHERE hosted OR instance_name <> '' OR instance_launch <> '' OR instance_asked_at IS NOT NULL OR instance_seen_at IS NOT NULL
   OR instance_machine <> '' OR instance_state <> '' OR instance_progress <> '' OR instance_progress_at IS NOT NULL
   OR instance_activity <> '' OR instance_activity_at IS NOT NULL OR instance_restarts <> 0 OR instance_restarted_at <> '{}'
   OR instance_renewals <> 0 OR instance_stopped OR instance_exited_at IS NOT NULL OR instance_exit_log <> '' OR instance_end_reason <> '';
INSERT INTO hosted_instances (platform_experiment_id, agent_id, name, launch, asked_at, seen_at, machine, state, progress, progress_at,
                              activity, activity_at, restarts, restarted_at, renewals, stopped, exited_at, exit_log, end_reason)
SELECT id, '', coordinator_instance_name, coordinator_instance_launch, coordinator_instance_asked_at, coordinator_instance_seen_at,
       coordinator_instance_machine, coordinator_instance_state, coordinator_instance_progress, coordinator_instance_progress_at,
       coordinator_instance_activity, coordinator_instance_activity_at, coordinator_instance_restarts, coordinator_instance_restarted_at,
       coordinator_instance_renewals, coordinator_instance_stopped, coordinator_instance_exited_at, coordinator_instance_exit_log,
       coordinator_instance_end_reason
FROM platform_experiments
WHERE coordinator_hosted OR coordinator_instance_name <> '' OR coordinator_instance_launch <> ''
   OR coordinator_instance_asked_at IS NOT NULL OR coordinator_instance_seen_at IS NOT NULL OR coordinator_instance_machine <> ''
   OR coordinator_instance_state <> '' OR coordinator_instance_progress <> '' OR coordinator_instance_progress_at IS NOT NULL
   OR coordinator_instance_activity <> '' OR coordinator_instance_activity_at IS NOT NULL OR coordinator_instance_restarts <> 0
   OR coordinator_instance_restarted_at <> '{}' OR coordinator_instance_renewals <> 0 OR coordinator_instance_stopped
   OR coordinator_instance_exited_at IS NOT NULL OR coordinator_instance_exit_log <> '' OR coordinator_instance_end_reason <> '';
ALTER TABLE experiment_signups
    DROP COLUMN instance_name, DROP COLUMN instance_launch, DROP COLUMN instance_asked_at, DROP COLUMN instance_seen_at,
    DROP COLUMN instance_machine, DROP COLUMN instance_state, DROP COLUMN instance_progress, DROP COLUMN instance_progress_at,
    DROP COLUMN instance_activity, DROP COLUMN instance_activity_at, DROP COLUMN instance_restarts, DROP COLUMN instance_restarted_at,
    DROP COLUMN instance_renewals, DROP COLUMN instance_stopped, DROP COLUMN instance_exited_at, DROP COLUMN instance_exit_log,
    DROP COLUMN instance_end_reason;
ALTER TABLE platform_experiments
    DROP COLUMN coordinator_instance_name, DROP COLUMN coordinator_instance_launch, DROP COLUMN coordinator_instance_asked_at,
    DROP COLUMN coordinator_instance_seen_at, DROP COLUMN coordinator_instance_machine, DROP COLUMN coordinator_instance_state,
    DROP COLUMN coordinator_instance_progress, DROP COLUMN coordinator_instance_progress_at, DROP COLUMN coordinator_instance_activity,
    DROP COLUMN coordinator_instance_activity_at, DROP COLUMN coordinator_instance_restarts, DROP COLUMN coordinator_instance_restarted_at,
    DROP COLUMN coordinator_instance_renewals, DROP COLUMN coordinator_instance_stopped, DROP COLUMN coordinator_instance_exited_at,
    DROP COLUMN coordinator_instance_exit_log, DROP COLUMN coordinator_instance_end_reason;

-- Indexes another index or a primary key begins with.
DROP INDEX idx_experiments_agent_id;
DROP INDEX idx_experiment_signups_platform;
DROP INDEX idx_pe_cuts_platform;
DROP INDEX idx_agent_quotas_agent;
