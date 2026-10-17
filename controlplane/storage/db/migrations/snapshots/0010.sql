-- The whole schema at this version, built in one step on a fresh database (see db.Migrate).
--
-- Metrics never live here: they are GreptimeDB's. This database holds the relational, desired
-- state.
--
-- Conventions:
--   - Statuses and other closed sets are TEXT with a CHECK.
--   - Who did something is an actor (domain.Actor): <verb>_by_kind and <verb>_by_id, or actor_kind
--     and actor_id for whose credential wrote the row. An optional actor is '' while there is none.
--     A column named <verb>_by alone references users(id).
--   - No triggers and no functions: every column a write sets is set by the store, and every
--     change GET /watch delivers is a row of changes the store appends with the write.

-- ---------------------------------------------------------------------------
-- Tenancy
-- ---------------------------------------------------------------------------

CREATE TABLE users (
    id         TEXT        PRIMARY KEY,
    name       TEXT        NOT NULL DEFAULT '',
    staff      BOOLEAN     NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE orgs (
    id         TEXT        PRIMARY KEY,
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE org_members (
    org_id     TEXT        NOT NULL REFERENCES orgs(id),
    user_id    TEXT        NOT NULL REFERENCES users(id),
    role       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id),
    CONSTRAINT org_members_role CHECK (role IN ('owner', 'member'))
);
CREATE INDEX idx_org_members_user ON org_members(user_id);

-- token_hash is the sha256 of the whole token, which is shown once and stored nowhere.
CREATE TABLE org_invites (
    id          TEXT        PRIMARY KEY,
    org_id      TEXT        NOT NULL REFERENCES orgs(id),
    role        TEXT        NOT NULL,
    token_hash  TEXT        NOT NULL UNIQUE,
    created_by  TEXT        NOT NULL REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    accepted_by TEXT        REFERENCES users(id),
    accepted_at TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ,
    CONSTRAINT org_invites_role CHECK (role IN ('owner', 'member')),
    CONSTRAINT org_invites_accepted CHECK ((accepted_by IS NULL) = (accepted_at IS NULL))
);
CREATE INDEX idx_org_invites_org ON org_invites(org_id);

CREATE TABLE projects (
    id         TEXT        PRIMARY KEY,
    org_id     TEXT        NOT NULL REFERENCES orgs(id),
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);

INSERT INTO orgs (id, name) VALUES ('default', 'default');
INSERT INTO projects (id, org_id, name) VALUES ('default', 'default', 'default');

-- key_hash is the sha256 of the whole token. project_id NULL reaches every project of the org.
-- minted_by is the key an API key was created with, NULL when a person created it; a key is
-- revoked while any key it descends from is.
CREATE TABLE api_keys (
    id         TEXT        PRIMARY KEY,
    key_hash   TEXT        NOT NULL UNIQUE,
    org_id     TEXT        NOT NULL REFERENCES orgs(id),
    project_id TEXT        REFERENCES projects(id),
    name       TEXT        NOT NULL,
    created_by TEXT        NOT NULL REFERENCES users(id),
    minted_by  TEXT        REFERENCES api_keys(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);
CREATE INDEX idx_api_keys_org ON api_keys(org_id);

-- ---------------------------------------------------------------------------
-- Clusters
-- ---------------------------------------------------------------------------

-- A shared cluster is the platform's and holds priority 0, so every org's own compute is tried
-- first. The *_seconds and max_speculative_accelerators columns override the scheduler's defaults
-- (domain.ClusterSettings), NULL for the default.
CREATE TABLE clusters (
    name                           TEXT        PRIMARY KEY,
    org_id                         TEXT        NOT NULL REFERENCES orgs(id),
    shared                         BOOLEAN     NOT NULL DEFAULT false,
    priority                       INTEGER     NOT NULL DEFAULT 100,
    reported_id                    TEXT        UNIQUE,
    job_image                      TEXT        NOT NULL DEFAULT '',
    builds                         BOOLEAN     NOT NULL DEFAULT false,
    scale_up_timeout_seconds       INTEGER     CHECK (scale_up_timeout_seconds > 0 AND scale_up_timeout_seconds < 1800),
    max_speculative_accelerators   INTEGER     CHECK (max_speculative_accelerators > 0),
    stuck_pending_timeout_seconds  INTEGER     CHECK (stuck_pending_timeout_seconds > 0),
    status_silence_ceiling_seconds INTEGER     CHECK (status_silence_ceiling_seconds > 0),
    last_error                     TEXT        NOT NULL DEFAULT '',
    last_error_at                  TIMESTAMPTZ,
    created_at                     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT clusters_priority CHECK (
        (shared AND priority = 0) OR (NOT shared AND priority BETWEEN 1 AND 1000)
    )
);
CREATE INDEX idx_clusters_org ON clusters(org_id);

-- A row is the org's decision that none of its jobs is placed on the cluster; no row is enabled.
CREATE TABLE org_disabled_clusters (
    org_id           TEXT        NOT NULL REFERENCES orgs(id),
    cluster_name     TEXT        NOT NULL REFERENCES clusters(name) ON DELETE CASCADE,
    disabled_by_kind TEXT        NOT NULL,
    disabled_by_id   TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (cluster_name, org_id),
    CONSTRAINT org_disabled_clusters_disabled_by_kind
        CHECK (disabled_by_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'))
);

-- ---------------------------------------------------------------------------
-- GitHub
-- ---------------------------------------------------------------------------

-- account_type is '' on an installation connected before it was kept, until it is connected again.
CREATE TABLE org_github_installations (
    org_id            TEXT        NOT NULL REFERENCES orgs(id),
    installation_id   BIGINT      NOT NULL,
    account_login     TEXT        NOT NULL,
    account_type      TEXT        NOT NULL,
    is_default        BOOLEAN     NOT NULL DEFAULT false,
    connected_by_kind TEXT        NOT NULL,
    connected_by_id   TEXT        NOT NULL,
    connected_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, installation_id),
    CONSTRAINT org_github_installations_account_type CHECK (account_type IN ('', 'User', 'Organization')),
    CONSTRAINT org_github_installations_connected_by_kind
        CHECK (connected_by_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'))
);
CREATE UNIQUE INDEX idx_org_github_installations_default ON org_github_installations(org_id) WHERE is_default;

-- Only the hash of an install's state is kept, so the table does not hold what would finish it.
CREATE TABLE github_install_states (
    state_hash BYTEA       PRIMARY KEY,
    org_id     TEXT        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    actor_kind TEXT        NOT NULL,
    actor_id   TEXT        NOT NULL,
    return_to  TEXT        NOT NULL,
    issued_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT github_install_states_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'))
);
CREATE INDEX idx_github_install_states_expires ON github_install_states(expires_at);

-- ---------------------------------------------------------------------------
-- Agent types and agents
-- ---------------------------------------------------------------------------

-- A built-in type has no org and is seeded at every start (db.SeedBuiltinAgentTypes).
CREATE TABLE agent_types (
    id              TEXT        PRIMARY KEY,
    org_id          TEXT        REFERENCES orgs(id),
    name            TEXT        NOT NULL,
    description     TEXT        NOT NULL DEFAULT '',
    role            TEXT        NOT NULL,
    current_version INTEGER     NOT NULL,
    archived_at     TIMESTAMPTZ,
    created_by_kind TEXT        NOT NULL,
    created_by_id   TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT agent_types_role CHECK (role IN ('researcher', 'baseline', 'coordinator', 'planner')),
    CONSTRAINT agent_types_created_by_kind
        CHECK (created_by_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'))
);
CREATE INDEX idx_agent_types_org ON agent_types(org_id);

-- Immutable: every edit inserts the next version. release is that
-- of the binary that seeded a built-in version (agenttypes.Release), NULL for an org's.
CREATE TABLE agent_type_versions (
    type_id         TEXT        NOT NULL REFERENCES agent_types(id),
    version         INTEGER     NOT NULL,
    prompt          TEXT        NOT NULL,
    settings        JSONB       NOT NULL,
    digest          TEXT        NOT NULL,
    release         INTEGER,
    created_by_kind TEXT        NOT NULL,
    created_by_id   TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (type_id, version),
    CONSTRAINT agent_type_versions_positive CHECK (version > 0),
    CONSTRAINT agent_type_versions_created_by_kind
        CHECK (created_by_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'))
);

ALTER TABLE agent_types ADD CONSTRAINT agent_types_current_version
    FOREIGN KEY (id, current_version) REFERENCES agent_type_versions(type_id, version) DEFERRABLE INITIALLY DEFERRED;

-- Who changed an agent type, when and how, and the version it was at after. Append-only.
CREATE TABLE agent_type_events (
    id         BIGSERIAL   PRIMARY KEY,
    type_id    TEXT        NOT NULL REFERENCES agent_types(id),
    action     TEXT        NOT NULL,
    version    INTEGER     NOT NULL,
    detail     TEXT        NOT NULL DEFAULT '',
    actor_kind TEXT        NOT NULL,
    actor_id   TEXT        NOT NULL DEFAULT '',
    at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT agent_type_events_action CHECK (action IN ('create', 'edit', 'rename', 'describe', 'archive', 'restore')),
    CONSTRAINT agent_type_events_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'))
);
CREATE INDEX agent_type_events_by_type ON agent_type_events (type_id, id);

-- The rules every agent's prompt holds ({{rules}}), seeded at every start like the built-in agent
-- types (db.SeedBuiltins). The newest version is the one served. Append-only.
CREATE TABLE platform_rules (
    version    INTEGER     PRIMARY KEY,
    rules      TEXT        NOT NULL,
    digest     TEXT        NOT NULL,
    release    INTEGER     NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT platform_rules_positive CHECK (version > 0)
);

CREATE TABLE agents (
    id         TEXT        PRIMARY KEY,
    org_id     TEXT        NOT NULL REFERENCES orgs(id),
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_agents_org ON agents(org_id);

-- ---------------------------------------------------------------------------
-- Platform experiments
-- ---------------------------------------------------------------------------

-- starts_at is NULL until the run has a clock. ends_at is what closes it in every status: a
-- draft's planning or proposal limit, an open one's baseline deadline, a running one's deadline
-- for a researcher to be at work, then starts_at plus duration_hours once started_at is written.
-- proposed_at with no proposal is a declined draft. A write that moves the status clears
-- coordinator_reason and researchers_held with it.
-- A removed one (removed_at) is closed and answered to nobody, and every row it holds stays.
CREATE TABLE platform_experiments (
    id                             TEXT             PRIMARY KEY,
    project_id                     TEXT             NOT NULL REFERENCES projects(id),
    name                           TEXT             NOT NULL,
    description                    TEXT             NOT NULL DEFAULT '',
    goal                           TEXT             NOT NULL DEFAULT '',
    goal_version                   INTEGER          NOT NULL DEFAULT 1,
    status                         TEXT             NOT NULL DEFAULT 'draft',
    shared                         BOOLEAN          NOT NULL DEFAULT false,
    continues                      TEXT             REFERENCES platform_experiments(id),
    created_by_kind                TEXT             NOT NULL,
    created_by_id                  TEXT             NOT NULL DEFAULT '',
    created_at                     TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at                     TIMESTAMPTZ      NOT NULL DEFAULT now(),

    budget_accelerator_hours       DOUBLE PRECISION NOT NULL,
    max_agents                     INTEGER          NOT NULL DEFAULT 100,
    max_hosted                     INTEGER          NOT NULL DEFAULT 0,
    max_concurrent_accelerators    INTEGER,
    default_quota_tier             TEXT             NOT NULL,
    researchers                    JSONB            NOT NULL DEFAULT '[]',
    metrics                        JSONB            NOT NULL DEFAULT '[]',
    report_interval_seconds        INTEGER          NOT NULL DEFAULT 30,
    stages                         JSONB            NOT NULL DEFAULT '[{"length_pct":40,"evict_pct":75},{"length_pct":60,"evict_pct":0}]',
    current_stage                  INTEGER          NOT NULL DEFAULT 1,

    starts_at                      TIMESTAMPTZ,
    ends_at                        TIMESTAMPTZ      NOT NULL,
    duration_hours                 DOUBLE PRECISION NOT NULL DEFAULT 0,
    started_at                     TIMESTAMPTZ,

    repository                     TEXT             NOT NULL DEFAULT '',
    repository_url                 TEXT             NOT NULL DEFAULT '',
    repository_account             TEXT             NOT NULL DEFAULT '',
    repository_error               TEXT             NOT NULL DEFAULT '',
    agent_image                    TEXT             NOT NULL DEFAULT '',

    proposal                       JSONB,
    proposed_at                    TIMESTAMPTZ,
    declined_reason                TEXT             NOT NULL DEFAULT '',
    planner_ends_at                TIMESTAMPTZ,
    accepts_at                     TIMESTAMPTZ,
    accepted_at                    TIMESTAMPTZ,
    accepted_by_kind               TEXT             NOT NULL DEFAULT '',
    accepted_by_id                 TEXT             NOT NULL DEFAULT '',
    accepted_answers               JSONB,

    baseline_job_id                TEXT             NOT NULL DEFAULT '',
    job_estimated_duration_hours   DOUBLE PRECISION NOT NULL DEFAULT 0,
    baseline_held_seconds          DOUBLE PRECISION NOT NULL DEFAULT 0,
    baseline_held_at               TIMESTAMPTZ,

    coordinator_hosted             BOOLEAN          NOT NULL DEFAULT false,
    coordinator_model              TEXT,
    coordinator_agent_type         TEXT             NOT NULL DEFAULT 'coordinator',
    coordinator_reason             TEXT,
    researchers_held               TEXT,

    capacity_free_since            TIMESTAMPTZ,
    capacity_noticed_at            TIMESTAMPTZ,
    capacity_detail                TEXT             NOT NULL DEFAULT '',
    capacity_repeat                INT              NOT NULL DEFAULT 0,

    summary                        TEXT             NOT NULL DEFAULT '',
    summary_at                     TIMESTAMPTZ,
    closed_at                      TIMESTAMPTZ,
    close_reason                   TEXT             NOT NULL DEFAULT '',
    closed_by_kind                 TEXT             NOT NULL DEFAULT '',
    closed_by_id                   TEXT             NOT NULL DEFAULT '',
    close_message                  TEXT             NOT NULL DEFAULT '',
    close_record                   TEXT             NOT NULL DEFAULT '',
    close_agents                   TEXT             NOT NULL DEFAULT '',
    removed_at                     TIMESTAMPTZ,
    removed_by_kind                TEXT             NOT NULL DEFAULT '',
    removed_by_id                  TEXT             NOT NULL DEFAULT '',

    CONSTRAINT platform_experiments_status CHECK (status IN ('draft', 'open', 'running', 'closed')),
    CONSTRAINT platform_experiments_current_stage CHECK (current_stage >= 1),
    CONSTRAINT platform_experiments_budgets_non_negative CHECK (
        budget_accelerator_hours >= 0
    ),
    CONSTRAINT platform_experiments_max_agents CHECK (max_agents > 0),
    CONSTRAINT platform_experiments_report_interval CHECK (report_interval_seconds > 0),
    CONSTRAINT platform_experiments_default_quota_tier CHECK (default_quota_tier IN ('guaranteed', 'burst_only')),
    CONSTRAINT platform_experiments_max_concurrent_accelerators
        CHECK (max_concurrent_accelerators IS NULL OR max_concurrent_accelerators > 0),
    CONSTRAINT platform_experiments_created_by_kind
        CHECK (created_by_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified')),
    CONSTRAINT platform_experiments_accepted_by_kind
        CHECK (accepted_by_kind IN ('', 'user', 'key', 'agent', 'coordinator', 'staff', 'unverified')),
    CONSTRAINT platform_experiments_closed_by_kind
        CHECK (closed_by_kind IN ('', 'user', 'key', 'agent', 'coordinator', 'staff', 'unverified')),
    CONSTRAINT platform_experiments_removed_by_kind
        CHECK (removed_by_kind IN ('', 'user', 'key', 'agent', 'coordinator', 'staff', 'unverified')),
    CONSTRAINT platform_experiments_removed_closed CHECK (removed_at IS NULL OR status = 'closed'),
    CONSTRAINT platform_experiments_coordinator_agent_type FOREIGN KEY (coordinator_agent_type) REFERENCES agent_types(id)
);
CREATE INDEX idx_platform_experiments_project ON platform_experiments(project_id);
CREATE INDEX platform_experiments_accepts_at ON platform_experiments (accepts_at) WHERE accepts_at IS NOT NULL;
CREATE INDEX idx_platform_experiments_in_progress ON platform_experiments(status)
    WHERE status IN ('draft', 'open', 'running');

-- Role and quota_tier are fixed at signup: a change mid-run would rewrite who a past cut applied
-- to. replaces is the researcher this one was added in the place of, '' for none. An agent runs
-- the current version of agent_type; a NULL model or hyperparameters is the type's.
CREATE TABLE experiment_signups (
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    agent_id               TEXT        NOT NULL REFERENCES agents(id),
    role                   TEXT        NOT NULL DEFAULT 'competitor',
    quota_tier             TEXT        NOT NULL,
    signed_up_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    added_by_kind          TEXT        NOT NULL,
    added_by_id            TEXT        NOT NULL DEFAULT '',

    hosted                 BOOLEAN     NOT NULL DEFAULT false,
    agent_type             TEXT        NOT NULL,
    model                  TEXT,
    hyperparameters        JSONB,

    data_access_key_id     TEXT        NOT NULL DEFAULT '',
    data_secret_access_key TEXT        NOT NULL DEFAULT '',

    replaces               TEXT        NOT NULL DEFAULT '',
    finished_at            TIMESTAMPTZ,
    finished_reason        TEXT        NOT NULL DEFAULT '',
    idle_noticed_at        TIMESTAMPTZ,
    idle_detail            TEXT        NOT NULL DEFAULT '',
    idle_repeat            INT         NOT NULL DEFAULT 0,

    PRIMARY KEY (platform_experiment_id, agent_id),
    CONSTRAINT experiment_signups_role CHECK (role IN ('competitor', 'baseline', 'reviewer', 'planner')),
    CONSTRAINT experiment_signups_quota_tier CHECK (quota_tier IN ('guaranteed', 'burst_only')),
    CONSTRAINT experiment_signups_added_by_kind
        CHECK (added_by_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified')),
    CONSTRAINT experiment_signups_agent_type FOREIGN KEY (agent_type) REFERENCES agent_types(id)
);
CREATE UNIQUE INDEX experiment_signups_replaced_once ON experiment_signups (platform_experiment_id, replaces) WHERE replaces <> '';
CREATE INDEX idx_experiment_signups_agent    ON experiment_signups(agent_id);

-- The machine the platform runs for a hosted agent (agent_id) or for a platform experiment's hosted
-- coordinator (agent_id ''), written by its launcher's reconcile. name is what the machine is asked
-- for under, '' while none is; it is cleared when the machine is given up on, and a restart asks
-- again under a new name until stopped. restarted_at holds the restarts inside
-- domain.AgentInstanceRestartWindow, spent under launch: another launch gets a fresh budget.
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

-- Allocation only: desired usage is derived from experiments, observed usage is the metrics store's.
CREATE TABLE agent_quotas (
    id                           TEXT             PRIMARY KEY,
    agent_id                     TEXT             NOT NULL REFERENCES agents(id),
    platform_experiment_id       TEXT             NOT NULL REFERENCES platform_experiments(id),
    guaranteed_accelerator_hours DOUBLE PRECISION NOT NULL,
    burst_accelerator_hours      DOUBLE PRECISION NOT NULL,
    created_at                   TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at                   TIMESTAMPTZ      NOT NULL DEFAULT now(),
    CONSTRAINT agent_quotas_non_negative CHECK (
        guaranteed_accelerator_hours >= 0 AND burst_accelerator_hours >= 0
    ),
    UNIQUE (agent_id, platform_experiment_id)
);
CREATE INDEX idx_agent_quotas_platform ON agent_quotas(platform_experiment_id);

CREATE TABLE experiment_top3 (
    platform_experiment_id TEXT             NOT NULL REFERENCES platform_experiments(id),
    agent_id               TEXT             NOT NULL REFERENCES agents(id),
    final_metric           DOUBLE PRECISION NOT NULL,
    recorded_at            TIMESTAMPTZ      NOT NULL DEFAULT now(),
    PRIMARY KEY (platform_experiment_id, agent_id)
);
CREATE INDEX idx_experiment_top3_agent ON experiment_top3(agent_id);

-- reason is '' for a stage cut, max_agents_lowered for a cut of a lowered max_agents.
CREATE TABLE platform_experiment_cuts (
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    agent_id               TEXT        NOT NULL REFERENCES agents(id),
    stage_index            INTEGER     NOT NULL,
    reason                 TEXT        NOT NULL DEFAULT '',
    cut_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (platform_experiment_id, agent_id)
);

-- Committed with the boundary's cuts and quota moves, so a crash mid-advance resumes it.
CREATE TABLE platform_experiment_stage_advances (
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    stage_index            INTEGER     NOT NULL,
    advanced_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (platform_experiment_id, stage_index)
);

-- agent_id is an agent of the platform experiment, or 'coordinator' for its coordinator.
CREATE TABLE agent_messages (
    id                     BIGSERIAL   PRIMARY KEY,
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id) ON DELETE CASCADE,
    agent_id               TEXT        NOT NULL,
    sender                 TEXT        NOT NULL,
    text                   TEXT        NOT NULL,
    actor_kind             TEXT        NOT NULL,
    actor_id               TEXT        NOT NULL DEFAULT '',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT agent_messages_sender CHECK (sender IN ('coordinator', 'owner')),
    CONSTRAINT agent_messages_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'))
);
CREATE INDEX agent_messages_by_agent ON agent_messages (platform_experiment_id, agent_id, created_at);

-- A new platform experiment's form as a person left it; form is the client's own.
CREATE TABLE experiment_drafts (
    id         TEXT        PRIMARY KEY,
    project_id TEXT        NOT NULL REFERENCES projects(id),
    form       JSONB       NOT NULL,
    step       TEXT        NOT NULL,
    failure    TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX experiment_drafts_project ON experiment_drafts (project_id, updated_at DESC);

-- ---------------------------------------------------------------------------
-- Environments
-- ---------------------------------------------------------------------------

-- An image built for an org from a repository's .hypothesisloop/Dockerfile on one base (domain.Environment),
-- named by key so it is built once. cluster_name is the cluster its attempt-th build is assigned
-- to, '' while none. loaded_* is the image of a building environment one node already holds.
CREATE TABLE environments (
    key            TEXT        PRIMARY KEY,
    org_id         TEXT        NOT NULL REFERENCES orgs (id),
    project_id     TEXT        NOT NULL REFERENCES projects (id),
    git_repository TEXT        NOT NULL,
    base           TEXT        NOT NULL,
    tools          TEXT        NOT NULL DEFAULT '',
    dockerfile     TEXT        NOT NULL,
    image          TEXT        NOT NULL,
    state          TEXT        NOT NULL,
    fault          TEXT        NOT NULL DEFAULT '',
    log_tail       TEXT        NOT NULL DEFAULT '',
    failed_log     TEXT        NOT NULL DEFAULT '',
    cluster_name   TEXT        NOT NULL DEFAULT '',
    attempt        INTEGER     NOT NULL DEFAULT 0,
    assigned_at    TIMESTAMPTZ,
    progress       TEXT        NOT NULL DEFAULT '',
    progress_at    TIMESTAMPTZ,
    loaded_image   TEXT        NOT NULL DEFAULT '',
    loaded_node    TEXT        NOT NULL DEFAULT '',
    loaded_cluster TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT environments_state_check CHECK (
        state IN ('building', 'ready', 'failed') AND fault IN ('', 'file', 'platform') AND (state = 'failed') = (fault <> '')
        AND (cluster_name = '') = (assigned_at IS NULL))
);
CREATE INDEX environments_assigned ON environments (cluster_name) WHERE state = 'building';

-- An agent told environment.status once the build it asked for is loaded or ends.
CREATE TABLE environment_waiters (
    environment_key        TEXT   NOT NULL REFERENCES environments (key) ON DELETE CASCADE,
    platform_experiment_id TEXT   NOT NULL REFERENCES platform_experiments (id) ON DELETE CASCADE,
    agent_id               TEXT   NOT NULL REFERENCES agents (id),
    code_ref               TEXT   NOT NULL,
    image                  TEXT   NOT NULL,
    loaded                 BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (environment_key, platform_experiment_id, agent_id)
);

-- ---------------------------------------------------------------------------
-- Jobs (experiments)
-- ---------------------------------------------------------------------------

-- job_spec is what was submitted and is never rewritten; resolved_job_spec is it as the runtime
-- runs it, written at admission and cleared on requeue. eviction_reason explains only the attempt
-- the status is about. cluster_name is '' while placed on none; last_cluster keeps the last
-- placement. data_access_key_id is '' only on terminal rows from before per-job keys.
CREATE TABLE experiments (
    id                       TEXT             PRIMARY KEY,
    parent_id                TEXT             REFERENCES experiments(id),
    platform_experiment_id   TEXT             NOT NULL REFERENCES platform_experiments(id),
    agent_id                 TEXT             NOT NULL REFERENCES agents(id),
    actor_kind               TEXT             NOT NULL,
    actor_id                 TEXT             NOT NULL DEFAULT '',
    status                   TEXT             NOT NULL,
    hypothesis_id            TEXT             NOT NULL,
    hypothesis               TEXT             NOT NULL,
    theory                   TEXT             NOT NULL DEFAULT '',
    code_ref                 TEXT             NOT NULL,
    branch                   TEXT             NOT NULL DEFAULT '',
    config_hash              TEXT             NOT NULL,
    data_ref                 TEXT             NOT NULL,
    job_spec                 JSONB            NOT NULL DEFAULT '{}'::jsonb,
    resolved_job_spec        JSONB,
    secret_env               JSONB            NOT NULL DEFAULT '{}'::jsonb,
    environment_key          TEXT             REFERENCES environments (key),
    submission_key           TEXT             NOT NULL DEFAULT '',

    accelerator_type         TEXT             NOT NULL,
    accelerator_count        INTEGER          NOT NULL DEFAULT 1,
    ran_on_accelerator_type  TEXT             NOT NULL DEFAULT '',
    capacity_tier            TEXT             NOT NULL DEFAULT 'guaranteed',
    priority_score           DOUBLE PRECISION NOT NULL DEFAULT 0,
    novelty_score            DOUBLE PRECISION NOT NULL DEFAULT 0,
    estimated_duration_hours DOUBLE PRECISION NOT NULL,
    estimated_cost_acch      DOUBLE PRECISION NOT NULL DEFAULT 0,

    cluster_name             TEXT             NOT NULL DEFAULT '',
    last_cluster             TEXT             NOT NULL DEFAULT '',
    queued_at                TIMESTAMPTZ,
    submitted_at             TIMESTAMPTZ,
    run_reported_at          TIMESTAMPTZ,
    not_admitted_reason      TEXT,
    eviction_reason          TEXT,
    attempt_count            INTEGER          NOT NULL DEFAULT 0,
    infra_requeue_count      INTEGER          NOT NULL DEFAULT 0,
    infra_requeue_reason     TEXT             NOT NULL DEFAULT '',
    tried_clusters           JSONB            NOT NULL DEFAULT '[]'::jsonb,
    quota_settled_at         TIMESTAMPTZ,
    data_access_key_id       TEXT             NOT NULL DEFAULT '',
    data_secret_access_key   TEXT             NOT NULL DEFAULT '',

    underperforming_metric   TEXT,
    held_reason              TEXT,
    same_hypothesis          TEXT,

    created_at               TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ      NOT NULL DEFAULT now(),
    ended_at                 TIMESTAMPTZ,

    CONSTRAINT experiments_status
        CHECK (status IN ('SUBMITTED', 'QUEUED', 'RUNNING', 'COMPLETED', 'FAILED', 'EVICTED', 'REJECTED')),
    CONSTRAINT experiments_capacity_tier CHECK (capacity_tier IN ('guaranteed', 'burst')),
    CONSTRAINT experiments_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified')),
    CONSTRAINT experiments_queue_reason_consistent
        CHECK ((status = 'QUEUED') = (not_admitted_reason IS NOT NULL)),
    CONSTRAINT experiments_reason_of_attempt
        CHECK (eviction_reason IS NULL OR status NOT IN ('SUBMITTED', 'RUNNING', 'COMPLETED')),
    CONSTRAINT experiments_active_has_data_key
        CHECK (data_access_key_id <> '' OR status IN ('COMPLETED', 'FAILED', 'EVICTED', 'REJECTED'))
);
CREATE INDEX idx_experiments_status     ON experiments(status);
CREATE INDEX idx_experiments_platform   ON experiments(platform_experiment_id);
CREATE INDEX idx_experiments_hypothesis ON experiments(hypothesis_id);
CREATE INDEX idx_experiments_agent_platform_status ON experiments(agent_id, platform_experiment_id, status);
CREATE INDEX idx_experiments_desired ON experiments(cluster_name, status)
    WHERE status IN ('SUBMITTED', 'RUNNING');
CREATE INDEX idx_experiments_unsettled  ON experiments(updated_at)
    WHERE quota_settled_at IS NULL AND status IN ('COMPLETED', 'FAILED', 'EVICTED', 'REJECTED');
CREATE INDEX idx_experiments_recently_undesired ON experiments(last_cluster, updated_at)
    WHERE eviction_reason IS NOT NULL;
CREATE INDEX idx_experiments_tried_clusters ON experiments(updated_at)
    WHERE tried_clusters <> '[]'::jsonb;
CREATE UNIQUE INDEX idx_experiments_submission_key
    ON experiments(platform_experiment_id, agent_id, submission_key) WHERE submission_key <> '';

-- A re-declaration of a running platform experiment's baseline to a stronger job.
CREATE TABLE baseline_raises (
    id                     BIGSERIAL   PRIMARY KEY,
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id) ON DELETE CASCADE,
    from_job_id            TEXT        NOT NULL REFERENCES experiments(id),
    to_job_id              TEXT        NOT NULL REFERENCES experiments(id),
    reason                 TEXT        NOT NULL,
    detail                 TEXT        NOT NULL,
    actor_kind             TEXT        NOT NULL,
    actor_id               TEXT        NOT NULL DEFAULT '',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT baseline_raises_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'))
);
CREATE INDEX baseline_raises_by_experiment ON baseline_raises (platform_experiment_id, id);

-- ---------------------------------------------------------------------------
-- What hangs off hypotheses, which live in the graph store
-- ---------------------------------------------------------------------------

-- One finding per job: the write-up its agent files once it ended.
CREATE TABLE hypothesis_findings (
    id                     TEXT        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    hypothesis_id          TEXT        NOT NULL,
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    experiment_id          TEXT        NOT NULL REFERENCES experiments(id) UNIQUE,
    agent_id               TEXT        NOT NULL REFERENCES agents(id),
    summary                TEXT        NOT NULL,
    actor_kind             TEXT        NOT NULL,
    actor_id               TEXT        NOT NULL DEFAULT '',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT hypothesis_findings_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'))
);
CREATE INDEX idx_hypothesis_findings_hypothesis ON hypothesis_findings(hypothesis_id);
CREATE INDEX idx_hypothesis_findings_platform ON hypothesis_findings(platform_experiment_id, created_at);

-- agent_id is NULL on a comment no agent owns. author is the name an unverified comment stated,
-- written before credentials were kept.
CREATE TABLE hypothesis_comments (
    id                     TEXT        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    hypothesis_id          TEXT        NOT NULL,
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    agent_id               TEXT        REFERENCES agents(id),
    text                   TEXT        NOT NULL,
    actor_kind             TEXT        NOT NULL,
    actor_id               TEXT        NOT NULL DEFAULT '',
    author                 TEXT        NOT NULL DEFAULT '',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT hypothesis_comments_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'))
);
CREATE INDEX idx_hypothesis_comments_hypothesis ON hypothesis_comments(hypothesis_id);
CREATE INDEX idx_hypothesis_comments_platform ON hypothesis_comments(platform_experiment_id, created_at);

-- Each agent's last checkout of a hypothesis (domain.CheckoutClaim).
CREATE TABLE hypothesis_checkouts (
    hypothesis_id          TEXT        NOT NULL,
    agent_id               TEXT        NOT NULL REFERENCES agents(id),
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    branch                 TEXT        NOT NULL,
    checked_out_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (hypothesis_id, agent_id)
);
CREATE INDEX idx_hypothesis_checkouts_platform_experiment
    ON hypothesis_checkouts(platform_experiment_id, checked_out_at);

-- ---------------------------------------------------------------------------
-- Changes: what GET /watch delivers
-- ---------------------------------------------------------------------------

-- Every change a write announces, appended by the store in the write's own transaction under one
-- advisory lock, so ids commit in order and a reader that read up to an id misses nothing below
-- it. Held for a week (domain.ChangesRetention).
CREATE TABLE changes (
    id                     BIGSERIAL   PRIMARY KEY,
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    agent_id               TEXT        NOT NULL DEFAULT '',
    kind                   TEXT        NOT NULL,
    subject                TEXT        NOT NULL,
    value                  TEXT        NOT NULL DEFAULT '',
    detail                 TEXT        NOT NULL DEFAULT '',
    occurred_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT changes_kind CHECK (kind IN (
        'experiment.status', 'experiment.blocked', 'quota.changed', 'stage.boundary',
        'hypothesis.new', 'hypothesis.status', 'hypothesis.link', 'finding.new', 'comment.new',
        'agent.cut', 'agent.finished', 'agent.replaced', 'agent.idle', 'agent.instructions',
        'capacity.idle', 'agent.message',
        'platform_experiment.status', 'platform_experiment.description', 'platform_experiment.goal',
        'platform_experiment.proposal', 'platform_experiment.plan',
        'platform_experiment.baseline', 'platform_experiment.removed',
        'job.underperforming', 'job.held', 'job.duplicate', 'environment.status'
    ))
);
CREATE INDEX changes_by_experiment ON changes (platform_experiment_id, id);
CREATE INDEX changes_occurred ON changes (occurred_at);
