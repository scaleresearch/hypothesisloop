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
--   - Every *_xid column is the transaction that last moved the field beside it: the position of
--     the event GET /watch replays from it.

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
-- Clusters and launchers
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

-- A launcher is removed only while holds_machines is false.
CREATE TABLE launchers (
    name           TEXT        PRIMARY KEY,
    holds_machines BOOLEAN     NOT NULL DEFAULT false,
    answered_at    TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    agent_version  TEXT        NOT NULL DEFAULT ''
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

-- Immutable (agent_type_versions_immutable): every edit inserts the next version. release is that
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

-- Who changed an agent type, when and how, and the version it was at after. Append-only
-- (agent_type_events_append_only).
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
-- types (db.SeedBuiltins). The newest version is the one served. Append-only
-- (platform_rules_append_only).
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
-- proposed_at with no proposal is a declined draft. The *_changed_at, *_xid and summary_at columns
-- are maintained by platform_experiments_touch alone.
CREATE TABLE platform_experiments (
    id                             TEXT             PRIMARY KEY,
    project_id                     TEXT             NOT NULL REFERENCES projects(id),
    name                           TEXT             NOT NULL,
    description                    TEXT             NOT NULL DEFAULT '',
    goal                           TEXT             NOT NULL DEFAULT '',
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
    coordinator_launcher           TEXT             NOT NULL DEFAULT '',
    coordinator_model              TEXT,
    coordinator_agent_type         TEXT             NOT NULL DEFAULT 'coordinator',
    coordinator_instructions_xid   BIGINT,
    coordinator_reason             TEXT,
    coordinator_reason_xid         BIGINT,
    researchers_held               TEXT,
    researchers_held_xid           BIGINT,

    capacity_free_since            TIMESTAMPTZ,
    capacity_noticed_at            TIMESTAMPTZ,
    capacity_xid                   BIGINT,
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

    status_changed_xid             BIGINT,
    description_changed_xid        BIGINT,

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
    CONSTRAINT platform_experiments_coordinator_agent_type FOREIGN KEY (coordinator_agent_type) REFERENCES agent_types(id)
);
CREATE INDEX idx_platform_experiments_project ON platform_experiments(project_id);
CREATE INDEX idx_platform_experiments_coordinator_launcher ON platform_experiments(coordinator_launcher) WHERE coordinator_hosted;
CREATE INDEX platform_experiments_accepts_at ON platform_experiments (accepts_at) WHERE accepts_at IS NOT NULL;
CREATE INDEX idx_platform_experiments_in_progress ON platform_experiments(status)
    WHERE status IN ('draft', 'open', 'running');

CREATE TABLE planning_requests (
    id                     BIGSERIAL   PRIMARY KEY,
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id) ON DELETE CASCADE,
    message                TEXT        NOT NULL,
    actor_kind             TEXT        NOT NULL,
    actor_id               TEXT        NOT NULL DEFAULT '',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_xid            BIGINT      NOT NULL,
    CONSTRAINT planning_requests_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'))
);
CREATE INDEX planning_requests_by_experiment ON planning_requests (platform_experiment_id, id);

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
    launcher               TEXT        NOT NULL DEFAULT '',
    agent_type             TEXT        NOT NULL,
    instructions_xid       BIGINT,
    model                  TEXT,
    hyperparameters        JSONB,

    data_access_key_id     TEXT        NOT NULL DEFAULT '',
    data_secret_access_key TEXT        NOT NULL DEFAULT '',

    replaces               TEXT        NOT NULL DEFAULT '',
    replaces_xid           BIGINT,
    finished_at            TIMESTAMPTZ,
    finished_reason        TEXT        NOT NULL DEFAULT '',
    finished_xid           BIGINT,
    idle_noticed_at        TIMESTAMPTZ,
    idle_xid               BIGINT,
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
CREATE INDEX idx_experiment_signups_launcher ON experiment_signups(launcher) WHERE hosted;

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
    updated_xid                  BIGINT           NOT NULL DEFAULT pg_current_xact_id()::text::bigint,
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
    cut_xid                BIGINT      NOT NULL DEFAULT pg_current_xact_id()::text::bigint,
    PRIMARY KEY (platform_experiment_id, agent_id)
);

-- Committed with the boundary's cuts and quota moves, so a crash mid-advance resumes it.
CREATE TABLE platform_experiment_stage_advances (
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    stage_index            INTEGER     NOT NULL,
    advanced_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    advanced_xid           BIGINT      NOT NULL DEFAULT pg_current_xact_id()::text::bigint,
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
    created_xid            BIGINT      NOT NULL,
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
    state                  TEXT   NOT NULL DEFAULT '',
    detail                 TEXT   NOT NULL DEFAULT '',
    ended_xid              BIGINT,
    loaded_xid             BIGINT,
    loaded_detail          TEXT   NOT NULL DEFAULT '',
    PRIMARY KEY (environment_key, platform_experiment_id, agent_id)
);
CREATE INDEX environment_waiters_ended ON environment_waiters (platform_experiment_id, ended_xid) WHERE ended_xid IS NOT NULL;
CREATE INDEX environment_waiters_loaded ON environment_waiters (platform_experiment_id, loaded_xid) WHERE loaded_xid IS NOT NULL;

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
    updated_xid              BIGINT           NOT NULL DEFAULT pg_current_xact_id()::text::bigint,
    status_xid               BIGINT           NOT NULL DEFAULT pg_current_xact_id()::text::bigint,
    not_admitted_xid         BIGINT           NOT NULL DEFAULT pg_current_xact_id()::text::bigint,
    queued_xid               BIGINT,
    submitted_xid            BIGINT,
    underperforming_xid      BIGINT,
    held_xid                 BIGINT,
    same_hypothesis_xid      BIGINT,

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
    created_xid            BIGINT      NOT NULL,
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
    created_xid            BIGINT      NOT NULL DEFAULT pg_current_xact_id()::text::bigint,
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
    created_xid            BIGINT      NOT NULL DEFAULT pg_current_xact_id()::text::bigint,
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
-- Change notification: the source of GET /watch's live event stream
-- ---------------------------------------------------------------------------
--
-- Every event about a Postgres row is emitted by an AFTER trigger on the row that changed, so the
-- NOTIFY is part of the very transaction that wrote it: a rolled-back write emits nothing, and no
-- event can ever describe a state the database did not commit. Hypotheses live in the graph store,
-- which has no NOTIFY, so their events are sent by the registry after the graph write commits,
-- through the same channel and payload.
--
-- The payload is a small typed record (kind, subject, new value, a qualifying detail, the two
-- scoping ids, and a cursor), never a copy of the row. A client wanting detail follows with a
-- normal GET, so no read path is duplicated here and nothing is persisted twice.
--
-- An event's position is the id of the transaction that wrote it, recorded in the *_xid column
-- beside each timestamp replay reads. Its cursor is the xmin of the writer's snapshot: every
-- transaction below it had finished, so its NOTIFY was delivered first, and a client resuming
-- from it misses nothing. Replay (see db.EventsStore) re-derives events from the *_xid columns,
-- so a live event and its replayed twin carry the same position. Timestamps are not commit
-- ordered and are not used for this.
--
-- The *_changed_at and *_xid columns record WHEN one field of their own row last moved: a row
-- timestamp, not a second copy of the field. They are NULL until the field first moves, and no Go
-- writer sets them: one clock per column, and no write path can forget it.

-- When the transaction at an event's position began. Replay re-derives events from rows, and a
-- row keeps a time beside few of its *_xid columns, so the time of a position is kept here once,
-- by the one function every event passes through. It holds no event: one row per transaction.
CREATE TABLE event_times (
    xid         BIGINT      PRIMARY KEY,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE FUNCTION hypothesisloop_notify_event(
    kind TEXT, subject TEXT, new_value TEXT, detail TEXT,
    pe_id TEXT, owner_agent_id TEXT
) RETURNS void AS $$
BEGIN
    INSERT INTO event_times (xid, occurred_at) VALUES (pg_current_xact_id()::text::bigint, now())
        ON CONFLICT (xid) DO NOTHING;
    PERFORM pg_notify('hypothesisloop_events', json_build_object(
        'kind', kind,
        'subject', subject,
        'value', new_value,
        'detail', COALESCE(detail, ''),
        'platform_experiment_id', pe_id,
        'agent_id', COALESCE(owner_agent_id, ''),
        'position', pg_current_xact_id()::text::bigint,
        'occurred_at', to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
        'cursor', pg_snapshot_xmin(pg_current_snapshot())::text::bigint
    )::text);
END;
$$ LANGUAGE plpgsql;

-- updated_xid moves on every write, as updated_at does. A waypoint's xid moves when its
-- timestamp does; a waypoint is only ever set from NULL, so the comparison is exact. status_xid
-- and not_admitted_xid move only with the column the live event announces, so a replayed event
-- carries the position its live twin did even after an unrelated later write to the row.
CREATE FUNCTION hypothesisloop_experiments_touch() RETURNS trigger AS $$
BEGIN
    NEW.updated_xid := pg_current_xact_id()::text::bigint;
    IF TG_OP = 'INSERT' OR NEW.status IS DISTINCT FROM OLD.status THEN
        NEW.status_xid := NEW.updated_xid;
    END IF;
    IF TG_OP = 'INSERT' OR NEW.not_admitted_reason IS DISTINCT FROM OLD.not_admitted_reason THEN
        NEW.not_admitted_xid := NEW.updated_xid;
    END IF;
    IF TG_OP = 'INSERT' OR NEW.queued_at IS DISTINCT FROM OLD.queued_at THEN
        NEW.queued_xid := CASE WHEN NEW.queued_at IS NULL THEN NULL ELSE NEW.updated_xid END;
    END IF;
    IF TG_OP = 'INSERT' OR NEW.submitted_at IS DISTINCT FROM OLD.submitted_at THEN
        NEW.submitted_xid := CASE WHEN NEW.submitted_at IS NULL THEN NULL ELSE NEW.updated_xid END;
    END IF;
    IF TG_OP = 'INSERT' OR NEW.underperforming_metric IS DISTINCT FROM OLD.underperforming_metric THEN
        NEW.underperforming_xid := CASE WHEN NEW.underperforming_metric IS NULL THEN NULL ELSE NEW.updated_xid END;
    END IF;
    IF TG_OP = 'INSERT' OR NEW.held_reason IS DISTINCT FROM OLD.held_reason THEN
        NEW.held_xid := CASE WHEN NEW.held_reason IS NULL THEN NULL ELSE NEW.updated_xid END;
    END IF;
    IF TG_OP = 'INSERT' OR NEW.same_hypothesis IS DISTINCT FROM OLD.same_hypothesis THEN
        NEW.same_hypothesis_xid := CASE WHEN NEW.same_hypothesis IS NULL THEN NULL ELSE NEW.updated_xid END;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER experiments_touch BEFORE INSERT OR UPDATE ON experiments
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_experiments_touch();

CREATE FUNCTION hypothesisloop_experiments_notify() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' OR NEW.status IS DISTINCT FROM OLD.status THEN
        PERFORM hypothesisloop_notify_event('experiment.status', NEW.id, NEW.status::text,
            COALESCE(NEW.eviction_reason, ''), NEW.platform_experiment_id, NEW.agent_id);
    END IF;
    -- The queue reason is what an agent polls hardest for: it changes while the status does not,
    -- so it needs an event of its own or a waiting agent learns nothing until admission. A job
    -- nobody has judged yet has no reason to tell (replayExperiments leaves it out too).
    IF split_part(NEW.not_admitted_reason, ': ', 1) <> 'awaiting_admission'
       AND (TG_OP = 'INSERT' OR NEW.not_admitted_reason IS DISTINCT FROM OLD.not_admitted_reason) THEN
        PERFORM hypothesisloop_notify_event('experiment.blocked', NEW.id,
            split_part(NEW.not_admitted_reason, ': ', 1),
            CASE WHEN position(': ' in NEW.not_admitted_reason) > 0
                 THEN substr(NEW.not_admitted_reason, position(': ' in NEW.not_admitted_reason) + 2) ELSE '' END,
            NEW.platform_experiment_id, NEW.agent_id);
    END IF;
    IF NEW.held_reason IS NOT NULL
       AND (TG_OP = 'INSERT' OR NEW.held_reason IS DISTINCT FROM OLD.held_reason) THEN
        PERFORM hypothesisloop_notify_event('job.held', NEW.id,
            split_part(NEW.held_reason, ': ', 1),
            CASE WHEN position(': ' in NEW.held_reason) > 0
                 THEN substr(NEW.held_reason, position(': ' in NEW.held_reason) + 2) ELSE '' END,
            NEW.platform_experiment_id, NEW.agent_id);
    END IF;
    IF NEW.same_hypothesis IS NOT NULL
       AND (TG_OP = 'INSERT' OR NEW.same_hypothesis IS DISTINCT FROM OLD.same_hypothesis) THEN
        PERFORM hypothesisloop_notify_event('job.duplicate', NEW.id, NEW.hypothesis_id, NEW.same_hypothesis,
            NEW.platform_experiment_id, NEW.agent_id);
    END IF;
    IF NEW.underperforming_metric IS NOT NULL
       AND (TG_OP = 'INSERT' OR NEW.underperforming_metric IS DISTINCT FROM OLD.underperforming_metric) THEN
        PERFORM hypothesisloop_notify_event('job.underperforming', NEW.id, NEW.underperforming_metric, '',
            NEW.platform_experiment_id, NEW.agent_id);
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER experiments_notify AFTER INSERT OR UPDATE ON experiments
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_experiments_notify();

CREATE FUNCTION hypothesisloop_agent_quotas_touch() RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    NEW.updated_xid := pg_current_xact_id()::text::bigint;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER agent_quotas_touch BEFORE UPDATE ON agent_quotas
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_agent_quotas_touch();

CREATE FUNCTION hypothesisloop_agent_quotas_notify() RETURNS trigger AS $$
BEGIN
    -- The value is the agent's share, all it may spend at either priority, as GET /quota answers
    -- it (domain.AgentQuota.Share): its guaranteed hours, or its burst hours when it has none
    -- guaranteed. Burst hours come out of the share and are never added to it. replayQuotas
    -- answers the same figure.
    PERFORM hypothesisloop_notify_event('quota.changed', NEW.agent_id,
        (CASE WHEN NEW.guaranteed_accelerator_hours > 0 THEN NEW.guaranteed_accelerator_hours
              ELSE NEW.burst_accelerator_hours END)::text,
        '', NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER agent_quotas_notify AFTER INSERT OR UPDATE ON agent_quotas
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_agent_quotas_notify();

CREATE FUNCTION hypothesisloop_platform_experiments_touch() RETURNS trigger AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        NEW.status_changed_xid := pg_current_xact_id()::text::bigint;
        NEW.coordinator_reason := NULL;
        NEW.researchers_held := NULL;
    END IF;
    IF NEW.coordinator_reason IS DISTINCT FROM OLD.coordinator_reason THEN
        NEW.coordinator_reason_xid := pg_current_xact_id()::text::bigint;
    END IF;
    IF NEW.researchers_held IS DISTINCT FROM OLD.researchers_held THEN
        NEW.researchers_held_xid := pg_current_xact_id()::text::bigint;
    END IF;
    -- The run's length is part of what every agent plans its work by, as the brief is.
    IF NEW.description IS DISTINCT FROM OLD.description OR NEW.duration_hours IS DISTINCT FROM OLD.duration_hours THEN
        NEW.description_changed_xid := pg_current_xact_id()::text::bigint;
    END IF;
    IF NEW.summary IS DISTINCT FROM OLD.summary THEN
        NEW.summary_at := now();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER platform_experiments_touch BEFORE UPDATE ON platform_experiments
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_platform_experiments_touch();

CREATE FUNCTION hypothesisloop_platform_experiments_notify() RETURNS trigger AS $$
BEGIN
    -- The run opening, closing or moving to running is a stop condition for every agent in it,
    -- and nothing told them: a closed platform experiment looked exactly like a quiet one.
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        PERFORM hypothesisloop_notify_event('platform_experiment.status', NEW.id, NEW.status::text,
            CASE WHEN NEW.status = 'closed' THEN NEW.close_message ELSE '' END, NEW.id, '');
    END IF;
    -- The status stayed and what it waits for became its coordinator's move. The machine of the
    -- last researcher stopped for good, and no event told the coordinator the run was its to close.
    IF NEW.coordinator_reason IS NOT NULL AND NEW.coordinator_reason IS DISTINCT FROM OLD.coordinator_reason THEN
        PERFORM hypothesisloop_notify_event('platform_experiment.status', NEW.id, NEW.status::text,
            NEW.coordinator_reason, NEW.id, '');
    END IF;
    -- The platform keeps places of the researchers empty, or no longer does.
    IF NEW.researchers_held IS NOT NULL AND NEW.researchers_held IS DISTINCT FROM OLD.researchers_held THEN
        PERFORM hypothesisloop_notify_event('platform_experiment.status', NEW.id, NEW.status::text,
            NEW.researchers_held, NEW.id, '');
    END IF;
    -- The brief is the question every agent is working on, and it is edited mid-run when the
    -- coordinator resolves or redirects something. The new text is deliberately NOT carried: a
    -- description is unbounded, and an event is a pointer — a subscriber re-reads it with the
    -- ordinary GET, exactly as it would have anyway. A new duration_hours is told the same way.
    IF NEW.description IS DISTINCT FROM OLD.description OR NEW.duration_hours IS DISTINCT FROM OLD.duration_hours THEN
        PERFORM hypothesisloop_notify_event('platform_experiment.description', NEW.id, '', '',
            NEW.id, '');
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER platform_experiments_notify AFTER UPDATE ON platform_experiments
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_platform_experiments_notify();

-- Findings and comments both name the hypothesis they hang off as their subject, not their own
-- id: the hypothesis is what a reader would GET, and it is what the pool is organised around.
CREATE FUNCTION hypothesisloop_findings_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('finding.new', NEW.hypothesis_id, NEW.experiment_id, '',
        NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER hypothesis_findings_notify AFTER INSERT ON hypothesis_findings
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_findings_notify();

CREATE FUNCTION hypothesisloop_comments_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('comment.new', NEW.hypothesis_id, '', '',
        NEW.platform_experiment_id, COALESCE(NEW.agent_id, ''));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER hypothesis_comments_notify AFTER INSERT ON hypothesis_comments
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_comments_notify();

CREATE FUNCTION hypothesisloop_stage_advance_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('stage.boundary', NEW.platform_experiment_id,
        NEW.stage_index::text, 'advanced', NEW.platform_experiment_id, '');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER stage_advances_notify AFTER INSERT ON platform_experiment_stage_advances
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_stage_advance_notify();

-- A cut is committed in the same transaction as the advance that computed it, so a subscriber
-- sees the boundary and who it fell on together or not at all.
CREATE FUNCTION hypothesisloop_cuts_notify() RETURNS trigger AS $$
BEGIN
    -- A cut for a lowered max_agents is no move of the ladder.
    IF NEW.reason = '' THEN
        PERFORM hypothesisloop_notify_event('stage.boundary', NEW.platform_experiment_id,
            NEW.stage_index::text, 'cut', NEW.platform_experiment_id, NEW.agent_id);
    END IF;
    -- The same row, said the other way round. stage.boundary is about the ladder and reaches
    -- everyone; agent.cut is about one agent and is the only event that tells it, without a GET,
    -- that the run is over for IT. It is agent-owned, so an agent's own subscription receives its
    -- own cut and is not woken by every rival's.
    PERFORM hypothesisloop_notify_event('agent.cut', NEW.agent_id,
        NEW.stage_index::text, NEW.reason, NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER pe_cuts_notify AFTER INSERT ON platform_experiment_cuts
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_cuts_notify();

CREATE FUNCTION hypothesisloop_agent_finished_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('agent.finished', NEW.agent_id,
        '', NEW.finished_reason, NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER signup_finished_notify AFTER UPDATE OF finished_at ON experiment_signups
    FOR EACH ROW WHEN (OLD.finished_at IS NULL AND NEW.finished_at IS NOT NULL)
    EXECUTE FUNCTION hypothesisloop_agent_finished_notify();

CREATE FUNCTION hypothesisloop_agent_replaced_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('agent.replaced', NEW.agent_id,
        NEW.replaces, (SELECT CASE WHEN gone.finished_at IS NOT NULL THEN 'it finished: ' || gone.finished_reason
                                   ELSE 'its machine stopped with no restart left' END
                       FROM experiment_signups gone
                       WHERE gone.platform_experiment_id = NEW.platform_experiment_id AND gone.agent_id = NEW.replaces),
        NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER signup_replaced_notify AFTER INSERT ON experiment_signups
    FOR EACH ROW WHEN (NEW.replaces <> '')
    EXECUTE FUNCTION hypothesisloop_agent_replaced_notify();

CREATE FUNCTION hypothesisloop_agent_idle_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('agent.idle', NEW.agent_id,
        CASE WHEN NEW.idle_repeat = 0 THEN 'new' ELSE 'continues' END, NEW.idle_detail, NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER signup_idle_notify AFTER UPDATE OF idle_xid ON experiment_signups
    FOR EACH ROW WHEN (NEW.idle_xid IS DISTINCT FROM OLD.idle_xid)
    EXECUTE FUNCTION hypothesisloop_agent_idle_notify();

CREATE FUNCTION hypothesisloop_capacity_idle_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('capacity.idle', NEW.id,
        CASE WHEN NEW.capacity_repeat = 0 THEN 'new' ELSE 'continues' END, NEW.capacity_detail, NEW.id, '');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER platform_experiments_capacity_idle_notify AFTER UPDATE OF capacity_xid ON platform_experiments
    FOR EACH ROW WHEN (NEW.capacity_xid IS DISTINCT FROM OLD.capacity_xid)
    EXECUTE FUNCTION hypothesisloop_capacity_idle_notify();

-- A revision asked of a draft's planner. A pointer: what was asked is read from the draft.
CREATE FUNCTION hypothesisloop_planning_request_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('platform_experiment.revision', NEW.platform_experiment_id,
        '', '', NEW.platform_experiment_id, '');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER planning_requests_notify AFTER INSERT ON planning_requests
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_planning_request_notify();

CREATE FUNCTION hypothesisloop_baseline_raise_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('platform_experiment.baseline', NEW.platform_experiment_id,
        NEW.to_job_id, NEW.detail, NEW.platform_experiment_id, '');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER baseline_raises_notify AFTER INSERT ON baseline_raises
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_baseline_raise_notify();

CREATE FUNCTION hypothesisloop_agent_message_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('agent.message', NEW.agent_id,
        NEW.sender, NEW.text, NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER agent_messages_notify AFTER INSERT ON agent_messages
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_agent_message_notify();

CREATE FUNCTION hypothesisloop_environment_ended_notify() RETURNS trigger AS $$
DECLARE
    w environment_waiters;
BEGIN
    FOR w IN
        UPDATE environment_waiters SET state = NEW.state, ended_xid = pg_current_xact_id()::text::bigint,
            detail = 'the environment commit ' || left(right(code_ref, 40), 12) || ' declares in .hypothesisloop/Dockerfile '
                || CASE WHEN NEW.state = 'ready' THEN 'is built on ' || image || ': a job of that commit on that image runs in it'
                        WHEN NEW.fault = 'file' THEN 'did not build on ' || image || ': `hl env status` at that commit prints the end of its output. Fix the file, commit and build again'
                        ELSE 'was not built on ' || image || ', no fault of the file''s: the platform could not build it, and `hl env build` builds it again' END
        WHERE environment_key = NEW.key AND ended_xid IS NULL
        RETURNING *
    LOOP
        IF NOT (w.state = 'ready' AND w.loaded_xid IS NOT NULL) THEN
            PERFORM hypothesisloop_notify_event('environment.status', w.environment_key, w.state, w.detail,
                w.platform_experiment_id, w.agent_id);
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER environments_ended_notify AFTER UPDATE OF state ON environments
    FOR EACH ROW WHEN (OLD.state = 'building' AND NEW.state <> 'building')
    EXECUTE FUNCTION hypothesisloop_environment_ended_notify();

CREATE FUNCTION hypothesisloop_environment_loaded_notify() RETURNS trigger AS $$
DECLARE
    w environment_waiters;
BEGIN
    FOR w IN
        UPDATE environment_waiters SET loaded_xid = pg_current_xact_id()::text::bigint,
            loaded_detail = 'the environment commit ' || left(right(code_ref, 40), 12) || ' declares in .hypothesisloop/Dockerfile is built on ' || image
                || ' and loaded on cluster ' || NEW.loaded_cluster || ' while it is pushed to the registry: submit its jobs now. One that fits on that cluster''s node starts there at once, '
                || 'any other starts once the push ends, by itself. A failed push or build is told as an update'
        WHERE environment_key = NEW.key AND ended_xid IS NULL AND loaded_xid IS NULL
        RETURNING *
    LOOP
        PERFORM hypothesisloop_notify_event('environment.status', w.environment_key, 'loaded', w.loaded_detail,
            w.platform_experiment_id, w.agent_id);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER environments_loaded_notify AFTER UPDATE OF loaded_image ON environments
    FOR EACH ROW WHEN (NEW.state = 'building' AND NEW.loaded_image <> '' AND OLD.loaded_image IS DISTINCT FROM NEW.loaded_image)
    EXECUTE FUNCTION hypothesisloop_environment_loaded_notify();

CREATE FUNCTION hypothesisloop_agent_type_versions_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'agent_type_versions are immutable: insert the next version instead';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER agent_type_versions_immutable BEFORE UPDATE OR DELETE ON agent_type_versions
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_agent_type_versions_immutable();

CREATE FUNCTION hypothesisloop_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER platform_rules_append_only BEFORE UPDATE OR DELETE ON platform_rules
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_append_only();

CREATE TRIGGER agent_type_events_append_only BEFORE UPDATE OR DELETE ON agent_type_events
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_append_only();

CREATE FUNCTION hypothesisloop_agent_instructions_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('agent.instructions', NEW.agent_id,
        (SELECT t.current_version::text FROM agent_types t WHERE t.id = NEW.agent_type), NEW.agent_type, NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER signup_instructions_notify AFTER UPDATE OF instructions_xid ON experiment_signups
    FOR EACH ROW WHEN (NEW.instructions_xid IS DISTINCT FROM OLD.instructions_xid)
    EXECUTE FUNCTION hypothesisloop_agent_instructions_notify();

CREATE FUNCTION hypothesisloop_coordinator_instructions_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('agent.instructions', 'coordinator',
        (SELECT t.current_version::text FROM agent_types t WHERE t.id = NEW.coordinator_agent_type), NEW.coordinator_agent_type, NEW.id, 'coordinator');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER platform_experiments_coordinator_instructions_notify AFTER UPDATE OF coordinator_instructions_xid ON platform_experiments
    FOR EACH ROW WHEN (NEW.coordinator_instructions_xid IS DISTINCT FROM OLD.coordinator_instructions_xid)
    EXECUTE FUNCTION hypothesisloop_coordinator_instructions_notify();
