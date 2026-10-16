-- The schema as it stood when migrations began: the end state of the idempotent schema.sql of
-- commit 7c630417, every in-place upgrade it carried already folded in. A database built by that
-- file is adopted at this version without running it (see db.Migrate).
--
-- Metrics (time-series values emitted during job execution) are never stored here: they live in
-- GreptimeDB, written via Prometheus remote-write and read back via PromQL. This database holds
-- only relational state: agents, experiments, quota, and cluster-agent coordination.

-- ---------------------------------------------------------------------------
-- Enum types
-- ---------------------------------------------------------------------------

-- Exactly the set domain.ValidExperimentStatus accepts.
CREATE TYPE experiment_status AS ENUM (
    'SUBMITTED',
    'QUEUED',
    'RUNNING',
    'COMPLETED',
    'FAILED',
    'EVICTED',
    'REJECTED'
);

-- accelerator_type is deliberately plain TEXT, not an ENUM: the Accelerator catalog is entirely
-- operator-defined via hypothesisloop.yaml's accelerator_types (see config.AcceleratorTypeConfig)
-- and any vendor's model name is valid (NVIDIA H100, AMD MI300X, ...).

CREATE TYPE platform_experiment_status AS ENUM (
    'draft',
    'open',
    'running',
    'closed'
);

CREATE TYPE capacity_tier AS ENUM (
    'guaranteed',
    'burst'
);

-- ---------------------------------------------------------------------------
-- Tenancy
-- ---------------------------------------------------------------------------

-- id is the identity provider's subject. name is the display name the provider last stated for
-- the person, kept so that what they did reads by name to others and after they are gone; it is
-- empty until the provider states one. No email: the provider owns it and nothing here shows it.
CREATE TABLE users (
    id         TEXT        PRIMARY KEY,
    staff      BOOLEAN     NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    name       TEXT        NOT NULL DEFAULT ''
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

-- An invite is a link: the platform stores no email and sends none. token_hash is the sha256 of
-- the whole token, which is shown once and stored nowhere. It is accepted once, before
-- expires_at and while not revoked. created_by is the person answerable for it.
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

-- Org is always derived (job -> platform experiment -> project -> org). Org 'default' and its
-- project 'default' exist on every database.
INSERT INTO orgs (id, name) VALUES ('default', 'default');
INSERT INTO projects (id, org_id, name) VALUES ('default', 'default', 'default');

-- key_hash is the sha256 of the whole token; the token itself is shown once and stored nowhere.
-- project_id NULL means the key reaches every project of its org. minted_by is the key an API
-- key was created with, NULL when a person created it. A key is read as revoked while any key it
-- descends from is.
CREATE TABLE api_keys (
    id         TEXT        PRIMARY KEY,
    key_hash   TEXT        NOT NULL UNIQUE,
    org_id     TEXT        NOT NULL REFERENCES orgs(id),
    project_id TEXT        REFERENCES projects(id),
    created_by TEXT        NOT NULL REFERENCES users(id),
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ,
    minted_by  TEXT        REFERENCES api_keys(id)
);
CREATE INDEX idx_api_keys_org ON api_keys(org_id);

-- A cluster exists for the platform once an org registers its name: a runtime token for an
-- unregistered name is refused, so a mistyped name cannot bring a cluster into being.
CREATE TABLE clusters (
    name       TEXT        PRIMARY KEY,
    org_id     TEXT        NOT NULL REFERENCES orgs(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The platform's decision that every org's jobs may be placed on the cluster; only staff set
    -- it. Without it a cluster takes the jobs of its own org alone.
    shared     BOOLEAN     NOT NULL DEFAULT false,
    -- The identity the cluster's runtime states about itself, bound to the name on its first
    -- report. The scheduler knows a connected cluster by it.
    reported_id TEXT       UNIQUE,
    -- The one image the cluster runs, as its runtime last reported it. Empty for a cluster that
    -- runs whatever image a job names. Placement reads it.
    job_image  TEXT        NOT NULL DEFAULT '',
    -- Orders the clusters a job may be placed on, higher first. The org that registered a cluster
    -- sets 1 to 1000 on it. A shared cluster is the platform's and holds 0, so every org's own
    -- compute is tried before it.
    priority   INTEGER     NOT NULL DEFAULT 100,
    -- What the org that registered the cluster sets on it in place of the scheduler's defaults
    -- (domain.ClusterSettings). NULL is the default.
    scale_up_timeout_seconds       INTEGER CHECK (scale_up_timeout_seconds > 0 AND scale_up_timeout_seconds < 1800),
    max_speculative_accelerators   INTEGER CHECK (max_speculative_accelerators > 0),
    stuck_pending_timeout_seconds  INTEGER CHECK (stuck_pending_timeout_seconds > 0),
    status_silence_ceiling_seconds INTEGER CHECK (status_silence_ceiling_seconds > 0),
    -- Whether the cluster's runtime builds environments, as it last reported it.
    builds     BOOLEAN     NOT NULL DEFAULT false,
    CONSTRAINT clusters_priority CHECK (
        (shared AND priority = 0) OR (NOT shared AND priority BETWEEN 1 AND 1000)
    )
);
CREATE INDEX idx_clusters_org ON clusters(org_id);

-- An org turns a cluster on or off for its own jobs, whether the cluster is its own or one the
-- platform shares. A row is the org's decision that none of its jobs is placed on the cluster
-- from then on; the jobs already placed there finish. No row is enabled.
CREATE TABLE org_disabled_clusters (
    org_id       TEXT        NOT NULL REFERENCES orgs(id),
    cluster_name TEXT        NOT NULL REFERENCES clusters(name) ON DELETE CASCADE,
    disabled_by  TEXT        NOT NULL,
    disabled_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster_name, org_id)
);

-- An agent-launcher the platform knows. Its token works while its row exists, as a runtime's
-- does while its cluster is registered.
CREATE TABLE launchers (
    name           TEXT PRIMARY KEY,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Whether the launcher holds machines, as its last question left it: it listed some, or its
    -- answer asked for some. A launcher is removed only while this is false.
    holds_machines BOOLEAN NOT NULL DEFAULT false,
    -- When the launcher was last answered, NULL for one never answered. A hosted agent waits
    -- without a machine while its launcher is silent (launcher_unreachable).
    answered_at    TIMESTAMPTZ
);

-- An installation of the platform's GitHub App that an org connected: a GitHub account, with the
-- repositories its owner chose on GitHub. Every installation token is minted when asked for and
-- stored nowhere. account_login is the owner part of every repository it covers.
CREATE TABLE org_github_installations (
    org_id          TEXT        NOT NULL REFERENCES orgs(id),
    installation_id BIGINT      NOT NULL,
    account_login   TEXT        NOT NULL,
    connected_by    TEXT        NOT NULL,
    connected_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Whether connected_by is a user's id or an API key's, or 'staff' for one an operator
    -- connected with hl-admin, or 'unverified' where who connected it was not kept.
    connected_by_kind TEXT      NOT NULL DEFAULT '',
    -- User or Organization as GitHub says at connect; '' until the account is connected again on
    -- a row from before it was kept.
    account_type    TEXT        NOT NULL DEFAULT '',
    -- The account a new repository is created on when a request names none, for an org that
    -- connected several: at most one per org, and gone with its row when it is disconnected.
    is_default      BOOLEAN     NOT NULL DEFAULT false,
    PRIMARY KEY (org_id, installation_id)
);
CREATE UNIQUE INDEX idx_org_github_installations_default ON org_github_installations(org_id) WHERE is_default;

-- An install of the GitHub App a person started for an org: GitHub hands the state back with the
-- installation it made or changed, and the connect takes the row once. Only the state's hash is
-- kept, so the table does not hold what would finish someone's install. return_to is the page of
-- the UI the person left from.
CREATE TABLE github_install_states (
    state_hash  BYTEA       PRIMARY KEY,
    org_id      TEXT        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    actor_kind  TEXT        NOT NULL,
    actor_id    TEXT        NOT NULL,
    return_to   TEXT        NOT NULL,
    issued_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_github_install_states_expires ON github_install_states(expires_at);

-- ---------------------------------------------------------------------------
-- agent_types: the prompt and settings an agent runs with (domain.AgentType)
-- ---------------------------------------------------------------------------

-- A built-in type has no org: control-service seeds it from the repository at start
-- (db.SeedBuiltinAgentTypes) and nothing else writes it. An org's own types are its owners'.
-- current_version is the version new agents are pinned to.
CREATE TABLE agent_types (
    id              TEXT        PRIMARY KEY,
    org_id          TEXT        REFERENCES orgs(id),
    name            TEXT        NOT NULL,
    role            TEXT        NOT NULL,
    current_version INTEGER     NOT NULL,
    archived_at     TIMESTAMPTZ,
    created_by_kind TEXT        NOT NULL DEFAULT '',
    created_by_id   TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT agent_types_role CHECK (role IN ('researcher', 'baseline', 'coordinator', 'planner'))
);
CREATE INDEX idx_agent_types_org ON agent_types(org_id);

-- Every edit inserts the next version, and none is changed or deleted (the trigger below refuses
-- both): an agent pins one, and what it ran with stays readable.
CREATE TABLE agent_type_versions (
    type_id         TEXT        NOT NULL REFERENCES agent_types(id),
    version         INTEGER     NOT NULL,
    prompt          TEXT        NOT NULL,
    settings        JSONB       NOT NULL,
    digest          TEXT        NOT NULL,
    created_by_kind TEXT        NOT NULL DEFAULT '',
    created_by_id   TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (type_id, version),
    CONSTRAINT agent_type_versions_positive CHECK (version > 0)
);

ALTER TABLE agent_types ADD CONSTRAINT agent_types_current_version
    FOREIGN KEY (id, current_version) REFERENCES agent_type_versions(type_id, version) DEFERRABLE INITIALLY DEFERRED;

-- ---------------------------------------------------------------------------
-- agents
-- ---------------------------------------------------------------------------

CREATE TABLE agents (
    id                TEXT             PRIMARY KEY,
    name              TEXT             NOT NULL,
    performance_score DOUBLE PRECISION NOT NULL DEFAULT 0.5,
    created_at        TIMESTAMPTZ      NOT NULL DEFAULT now(),
    org_id            TEXT             NOT NULL REFERENCES orgs(id)
);
CREATE INDEX idx_agents_org ON agents(org_id);

-- ---------------------------------------------------------------------------
-- platform_experiments: operator-defined compute envelopes
-- ---------------------------------------------------------------------------

CREATE TABLE platform_experiments (
    id                   TEXT                       PRIMARY KEY,
    name                 TEXT                       NOT NULL,
    description          TEXT                       NOT NULL DEFAULT '',
    budget_accelerator_hours DOUBLE PRECISION       NOT NULL,
    max_agents           INTEGER                    NOT NULL DEFAULT 100,
    -- The run's clock. A running one holds its start in starts_at, and an open one the moment
    -- its baseline is measured from. ends_at is what closes it: a draft's planning or proposal
    -- limit, an open one's baseline deadline, a running one's deadline for a researcher's machine
    -- to be running, and once started_at is written, starts_at plus duration_hours.
    starts_at            TIMESTAMPTZ                NOT NULL,
    ends_at              TIMESTAMPTZ                NOT NULL,
    status               platform_experiment_status NOT NULL DEFAULT 'draft',
    metrics              JSONB                      NOT NULL DEFAULT '[]',
    report_interval_seconds INTEGER                 NOT NULL DEFAULT 30,
    -- The elimination ladder: an ordered list of {length_pct, evict_pct}, fixed at creation.
    -- Validated by domain.ValidateStages before insert.
    stages               JSONB                      NOT NULL DEFAULT '[{"length_pct":40,"evict_pct":75},{"length_pct":60,"evict_pct":0}]',
    -- 1-based index into stages of the stage currently running. Zero or negative indexes the
    -- ladder out of bounds, which the reconcile loop then hits on every tick.
    current_stage        INTEGER                    NOT NULL DEFAULT 1,
    -- The coordinator's narrative verdict on the finished run. Prose and nothing else: the
    -- standings are derived from the metrics store on read (GET /platform-experiments/{id}/results).
    summary              TEXT                       NOT NULL DEFAULT '',
    -- Empty marks experiments created before this policy existed and resolves to guaranteed
    -- (domain.ResolveQuotaTier); new ones store 'guaranteed' explicitly.
    default_quota_tier   TEXT                       NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ                NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ                NOT NULL DEFAULT now(),
    -- The sentence a draft was created from (domain.PlatformExpDraft), and whose credential
    -- created the platform experiment (domain.Actor: kind and id).
    goal                 TEXT                       NOT NULL DEFAULT '',
    created_by_kind      TEXT                       NOT NULL DEFAULT '',
    created_by_id        TEXT                       NOT NULL DEFAULT '',
    -- What a draft's planner proposed (quota.Proposal as JSON) and when it answered. Both NULL
    -- until it has, and proposed_at NULL again while a revision is being planned. A decline
    -- answers too: it sets proposed_at and clears proposal, and is read as declined_at
    -- (domain.SetAnswered). The launcher keeps the planner's machine while proposed_at is NULL,
    -- and after the answer until planner_ends_at.
    proposal             JSONB,
    proposed_at          TIMESTAMPTZ,
    -- Why a draft's planner declined to propose; the next proposal clears it.
    declined_reason      TEXT                       NOT NULL DEFAULT '',
    -- The platform's record of a close (quota.recordClose), kept beside the coordinator's summary
    -- and answered after it. Empty until written.
    close_record         TEXT                       NOT NULL DEFAULT '',
    -- How each agent's run stood at the moment of the close, the agent lines of its record, taken
    -- by quota.Close before the close is written. Empty until taken.
    close_agents         TEXT                       NOT NULL DEFAULT '',
    -- When the summary was last written, NULL while it never was.
    summary_at           TIMESTAMPTZ,
    -- The completed job that measured the baseline, and the estimate of a job that states none:
    -- the hours that job was observed running, with headroom (0 when it was too short to be
    -- observed). Written when the baseline is declared; a platform experiment starts only with them.
    baseline_job_id      TEXT                       NOT NULL DEFAULT '',
    job_estimated_duration_hours DOUBLE PRECISION   NOT NULL DEFAULT 0,
    -- Until when the launcher keeps the machine of a draft's planner once it answered: the answer
    -- plus planning.planner_kept_minutes, never later than the planning limit. NULL while the
    -- planner works, and again once a revision gave the draft back to it.
    planner_ends_at      TIMESTAMPTZ,
    -- Accelerators this platform experiment may hold in flight (SUBMITTED+RUNNING) at once,
    -- across all its agents' jobs. NULL = the config default
    -- (quota.default_max_concurrent_accelerators).
    max_concurrent_accelerators INTEGER,
    -- domain.ResearcherPool: the researchers a platform experiment is to have at work,
    -- [{agent_type, model, hyperparameters, count}], at most max_agents in all.
    researchers          JSONB                      NOT NULL DEFAULT '[]',
    -- Row timestamps for the events of GET /watch (see "Change notification" below): when one
    -- field of this row last moved, and the transaction that moved it. Maintained by triggers.
    status_changed_at    TIMESTAMPTZ,
    description_changed_at TIMESTAMPTZ,
    status_changed_xid   BIGINT,
    description_changed_xid BIGINT,
    -- The status_reason its coordinator was last told the experiment waits for it with, NULL
    -- while it waits for someone else. A change of status clears it.
    coordinator_reason   TEXT,
    coordinator_reason_xid BIGINT,
    -- A running platform experiment whose capacity holds places beyond its queued jobs, told so
    -- (capacity.idle). capacity_free_since is when the sweep first saw places free, NULL once it
    -- saw none. capacity_repeat is 0 on the first notice of a stretch and counts the ones after.
    capacity_free_since  TIMESTAMPTZ,
    capacity_noticed_at  TIMESTAMPTZ,
    capacity_xid         BIGINT,
    capacity_detail      TEXT                       NOT NULL DEFAULT '',
    capacity_repeat      INT                        NOT NULL DEFAULT 0,
    project_id           TEXT                       NOT NULL REFERENCES projects(id),
    -- When the platform experiment closed, why (domain.CloseReason), whose credential closed it,
    -- and what that caller said of why.
    closed_at            TIMESTAMPTZ,
    close_reason         TEXT                       NOT NULL DEFAULT '',
    closed_by_kind       TEXT                       NOT NULL DEFAULT '',
    closed_by_id         TEXT                       NOT NULL DEFAULT '',
    close_message        TEXT                       NOT NULL DEFAULT '',
    -- A coordinator the platform runs: one more machine of coordinator_launcher beside the
    -- agents', on coordinator_model. Fixed at creation, and changed only by a reopen. The
    -- coordinator_instance_* columns say what became of its machine, exactly as the instance_*
    -- columns of experiment_signups do for an agent's.
    coordinator_hosted   BOOLEAN                    NOT NULL DEFAULT false,
    coordinator_launcher TEXT                       NOT NULL DEFAULT '',
    coordinator_model    TEXT                       NOT NULL DEFAULT '',
    coordinator_instance_name        TEXT           NOT NULL DEFAULT '',
    coordinator_instance_asked_at    TIMESTAMPTZ,
    coordinator_instance_restarts    INTEGER        NOT NULL DEFAULT 0,
    coordinator_instance_restarted_at TIMESTAMPTZ[] NOT NULL DEFAULT '{}',
    coordinator_instance_stopped     BOOLEAN        NOT NULL DEFAULT false,
    coordinator_instance_exited_at   TIMESTAMPTZ,
    coordinator_instance_exit_log    TEXT           NOT NULL DEFAULT '',
    coordinator_instance_seen_at     TIMESTAMPTZ,
    coordinator_instance_launch      TEXT           NOT NULL DEFAULT '',
    coordinator_instance_state       TEXT           NOT NULL DEFAULT '',
    coordinator_instance_progress    TEXT           NOT NULL DEFAULT '',
    coordinator_instance_progress_at TIMESTAMPTZ,
    coordinator_instance_end_reason  TEXT           NOT NULL DEFAULT '',
    coordinator_instance_renewals    INTEGER        NOT NULL DEFAULT 0,
    coordinator_instance_activity    TEXT           NOT NULL DEFAULT '',
    coordinator_instance_activity_at TIMESTAMPTZ,
    coordinator_instance_machine     TEXT           NOT NULL DEFAULT '',
    -- How many machines the platform may run for this platform experiment, its coordinator
    -- included. Fixed at creation from config agents.max_hosted_per_platform_experiment.
    max_hosted           INTEGER                    NOT NULL DEFAULT 0,
    -- The platform's decision that every account may read the platform experiment and everything
    -- in it; only staff set it. Nobody outside its org writes to it.
    shared               BOOLEAN                    NOT NULL DEFAULT false,
    -- The code a platform experiment works on, owner/name on the git host, the address it is
    -- cloned from, and the image its hosted agents and coordinator run (empty is the platform's
    -- own, config agents.image). Fixed at creation.
    repository           TEXT                       NOT NULL DEFAULT '',
    agent_image          TEXT                       NOT NULL DEFAULT '',
    repository_url       TEXT                       NOT NULL DEFAULT '',
    -- A platform experiment whose repository the platform creates is stored without it, and
    -- repository_account is the account it is created on: the repository pass
    -- (quota.ProvideRepositories) creates it, and none of its machines starts before it is there.
    -- repository_error is what the git host answered on one closed repository_not_created.
    repository_account   TEXT                       NOT NULL DEFAULT '',
    repository_error     TEXT                       NOT NULL DEFAULT '',
    -- The run's length, counted from its start, and the moment a researcher was first at work in
    -- the running platform experiment.
    duration_hours       DOUBLE PRECISION           NOT NULL DEFAULT 0,
    started_at           TIMESTAMPTZ,
    -- The time an open platform experiment's baseline waited on the platform, building the
    -- environment of one of its jobs or starting one on its cluster, which moved its
    -- baseline_due_at (ends_at) out by as much: credited so far, and the last sweep that saw it
    -- waiting, NULL when the last one did not.
    baseline_held_seconds DOUBLE PRECISION          NOT NULL DEFAULT 0,
    baseline_held_at     TIMESTAMPTZ,
    -- When the platform accepts a proposed draft as proposed (planning.auto_accept_seconds after
    -- the proposal), unless a person accepts it sooner, holds it to edit it, revises it or closes
    -- it. NULL on a draft that waits for a person, and on anything that is no proposed draft.
    accepts_at           TIMESTAMPTZ,
    -- The closed run of the same project and repository this one continues, NULL for one that
    -- starts fresh. Its agents read that run, and start from its baseline's commit.
    continues            TEXT                       REFERENCES platform_experiments (id),
    -- When a draft's proposal was accepted, whose credential accepted it (empty when the platform
    -- accepted it by itself at accepts_at) and the answers given to its questions, in their order.
    accepted_at          TIMESTAMPTZ,
    accepted_by_kind     TEXT                       NOT NULL DEFAULT '',
    accepted_by_id       TEXT                       NOT NULL DEFAULT '',
    accepted_answers     JSONB,
    -- The coordinator's agent type and pinned version, and the transaction that last moved the
    -- pin, for the replay of agent.instructions to it.
    coordinator_agent_type         TEXT             NOT NULL DEFAULT 'coordinator',
    coordinator_agent_type_version INTEGER,
    coordinator_instructions_xid   BIGINT,
    CONSTRAINT platform_experiments_current_stage CHECK (current_stage >= 1),
    CONSTRAINT platform_experiments_budgets_non_negative CHECK (
        budget_accelerator_hours >= 0
    ),
    CONSTRAINT platform_experiments_max_agents CHECK (max_agents > 0),
    CONSTRAINT platform_experiments_report_interval CHECK (report_interval_seconds > 0),
    CONSTRAINT platform_experiments_default_quota_tier CHECK (default_quota_tier IN ('', 'guaranteed', 'burst_only')),
    CONSTRAINT platform_experiments_max_concurrent_accelerators
        CHECK (max_concurrent_accelerators IS NULL OR max_concurrent_accelerators > 0),
    CONSTRAINT platform_experiments_coordinator_agent_type
        FOREIGN KEY (coordinator_agent_type, coordinator_agent_type_version) REFERENCES agent_type_versions(type_id, version)
);
CREATE INDEX idx_platform_experiments_project ON platform_experiments(project_id);
CREATE INDEX idx_platform_experiments_coordinator_launcher ON platform_experiments(coordinator_launcher) WHERE coordinator_hosted;
CREATE INDEX platform_experiments_accepts_at ON platform_experiments (accepts_at) WHERE accepts_at IS NOT NULL;

-- What a person asked a draft's planner to change in its proposal, one row per request, and
-- whose credential asked (domain.Actor: kind and id). created_xid is the transaction that
-- recorded the request, which platform_experiment.revision replays from.
CREATE TABLE planning_requests (
    id                     BIGSERIAL   PRIMARY KEY,
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id) ON DELETE CASCADE,
    message                TEXT        NOT NULL,
    actor_kind             TEXT        NOT NULL,
    actor_id               TEXT        NOT NULL DEFAULT '',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_xid            BIGINT
);
CREATE INDEX planning_requests_by_experiment ON planning_requests (platform_experiment_id, id);

-- ---------------------------------------------------------------------------
-- environments
-- ---------------------------------------------------------------------------

-- An environment is an image the platform builds for an org from a repository's
-- .hypothesisloop/Dockerfile on one base image (domain.Environment). key names them, so one is
-- built once. A row is made building, and the scheduler assigns its build to a cluster that
-- builds (clusters.builds), the attempt-th time at assigned_at, cluster_name empty while it is
-- assigned to none. That cluster reports it ready with image, repository@digest, or failed with
-- fault file. A build assigned for the last attempt allowed and not finished fails with fault
-- platform. A failed row is made building again by the next submit that names it.
CREATE TABLE environments (
    key          TEXT        PRIMARY KEY,
    org_id       TEXT        NOT NULL REFERENCES orgs (id),
    base         TEXT        NOT NULL,
    dockerfile   TEXT        NOT NULL,
    image        TEXT        NOT NULL,
    state        TEXT        NOT NULL,
    fault        TEXT        NOT NULL DEFAULT '',
    log_tail     TEXT        NOT NULL DEFAULT '',
    cluster_name TEXT        NOT NULL DEFAULT '',
    attempt      INTEGER     NOT NULL DEFAULT 0,
    assigned_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The step the assigned cluster last reported of the attempt it builds, at progress_at. A new
    -- assignment clears it.
    progress     TEXT        NOT NULL DEFAULT '',
    progress_at  TIMESTAMPTZ,
    -- The end of the output of the file's last failed build, kept while a later submit builds the
    -- same file once more, so the job waiting for it says so. Cleared once it is built.
    failed_log   TEXT        NOT NULL DEFAULT '',
    -- The image of a building environment, repository@digest, once cluster loaded_cluster holds
    -- it in the image store of its node loaded_node while the push goes on: a job pinned to that
    -- node runs it before it is ready. A push that failed keeps it, so the node runs the build's
    -- jobs while it is built again. Empty otherwise, and cleared once the build is ready or failed.
    loaded_image   TEXT      NOT NULL DEFAULT '',
    loaded_node    TEXT      NOT NULL DEFAULT '',
    loaded_cluster TEXT      NOT NULL DEFAULT '',
    -- The image an environment's last layer takes the platform's tools from
    -- (domain.PlatformTools), repository@digest, empty for one built on its job's image itself.
    -- base is then that image without them, so a release that changes only the tools keeps the
    -- layers the file built.
    tools        TEXT        NOT NULL DEFAULT '',
    -- The repository whose environment file this is, of project project_id: its build reads it
    -- (domain.EnvironmentGitSecret), and the key names it.
    project_id     TEXT      NOT NULL DEFAULT '',
    git_repository TEXT      NOT NULL DEFAULT '',
    CONSTRAINT environments_state_check CHECK (
        state IN ('building', 'ready', 'failed') AND fault IN ('', 'file', 'platform') AND (state = 'failed') = (fault <> '')
        AND (cluster_name = '') = (assigned_at IS NULL))
);
CREATE INDEX environments_assigned ON environments (cluster_name) WHERE state = 'building';

-- ---------------------------------------------------------------------------
-- experiments
-- ---------------------------------------------------------------------------

CREATE TABLE experiments (
    id                       TEXT              PRIMARY KEY,
    parent_id                TEXT              REFERENCES experiments(id),
    agent_id                 TEXT              NOT NULL REFERENCES agents(id),
    -- The cluster the job is placed on, '' while it is placed on none.
    cluster_name             TEXT              NOT NULL DEFAULT '',
    -- Every job belongs to exactly one platform experiment, so quota and the hypothesis pool
    -- always have one to key off.
    platform_experiment_id   TEXT              NOT NULL REFERENCES platform_experiments(id),
    code_ref                 TEXT              NOT NULL,
    config_hash              TEXT              NOT NULL,
    data_ref                 TEXT              NOT NULL,
    job_spec                 JSONB             NOT NULL DEFAULT '{}'::jsonb,
    -- job_spec as the runtime runs it (a slot's size, the image, the grace periods), written once
    -- at admission (ClaimSubmitted) onto the specific cluster the job landed on, reset to NULL on
    -- requeue (MarkQueued). job_spec itself is never rewritten, so a GET always shows exactly
    -- what was submitted (see domain.Experiment.ResolvedJob/EffectiveJob).
    resolved_job_spec        JSONB,
    -- A hypothesis in the graph store (shared/graphdb), validated there at submit.
    hypothesis_id            TEXT              NOT NULL,
    hypothesis               TEXT              NOT NULL,
    theory                   TEXT              NOT NULL DEFAULT '',
    novelty_score            DOUBLE PRECISION  NOT NULL DEFAULT 0,
    accelerator_type         TEXT              NOT NULL,
    accelerator_count        INTEGER           NOT NULL DEFAULT 1,
    capacity_tier            capacity_tier     NOT NULL DEFAULT 'guaranteed',
    status                   experiment_status NOT NULL,
    priority_score           DOUBLE PRECISION  NOT NULL DEFAULT 0,
    estimated_duration_hours DOUBLE PRECISION  NOT NULL,
    estimated_cost_acch      DOUBLE PRECISION  NOT NULL DEFAULT 0,
    queued_at                TIMESTAMPTZ,
    submitted_at             TIMESTAMPTZ,
    -- When a runtime first reported the end of the run it measured under the job's current
    -- admission. One before submitted_at is an earlier admission's. The run is placed in time by
    -- it, so every later report of the same run places it where the first did.
    run_reported_at          TIMESTAMPTZ,
    -- Why the job's last attempt ended, as "code" or "code: detail": written by the requeue or
    -- the terminal write that ended it and cleared by the claim that starts the next, so it never
    -- explains an attempt other than the one the status is about (experiments_reason_of_attempt).
    -- Earlier attempts are counted in attempt_count and infra_requeue_count and listed in
    -- tried_clusters.
    eviction_reason          TEXT,
    -- Current scheduler decision for a QUEUED job; overwritten, not historical.
    not_admitted_reason      TEXT,
    -- Set once this terminal experiment's final observed usage has been durably written to the
    -- metrics DB. NULL means settlement is outstanding, the durable signal a background reconciler
    -- scans for to retry writing it.
    quota_settled_at         TIMESTAMPTZ,
    -- How many attempts of this experiment have already run and failed. 0 on the first attempt.
    -- Only the control plane's gang retry writes it (see RequeueForRetry): a single-pod job's
    -- retries are the runtime's BackoffLimit and never reach here.
    attempt_count            INTEGER           NOT NULL DEFAULT 0,
    -- How many of attempt_count's attempts were ended by the environment (an
    -- infrastructure-class eviction reason) and requeued for free. The agent's max_retries
    -- allowance is attempt_count - infra_requeue_count.
    infra_requeue_count      INTEGER           NOT NULL DEFAULT 0,
    -- The eviction reason of the last of those requeues, written with the count and never
    -- cleared: eviction_reason explains only the attempt that ended, and goes with the next claim.
    infra_requeue_reason     TEXT              NOT NULL DEFAULT '',
    -- The placements of this job the environment failed, as a JSON array of
    -- {"cluster_id": ..., "at": <RFC3339>, "reason": <eviction reason code>}. Appended by
    -- RequeueInfrastructureFault in the write that spends infra_requeue_count; read at candidate
    -- selection to leave out a cluster that failed this job within
    -- scheduler.tried_cluster_ttl_seconds.
    tried_clusters           JSONB             NOT NULL DEFAULT '[]'::jsonb,
    -- The accelerator type the job ran on, written by the write that marks it RUNNING and kept
    -- for good: what it has run is billed at the type the row carries, so every later placement
    -- is on this type. '' for a job that has not run or holds no accelerator.
    ran_on_accelerator_type  TEXT              NOT NULL DEFAULT '',
    -- The job's permanent object-store key, created in the store in the same transaction that
    -- inserts the row and never rotated or deleted. '' only on terminal rows that predate
    -- per-job keys (experiments_active_has_data_key).
    data_access_key_id       TEXT              NOT NULL DEFAULT '',
    data_secret_access_key   TEXT              NOT NULL DEFAULT '',
    -- The job's secret_env, name to value, kept out of job_spec because every read of a job
    -- returns job_spec. Handed to the runtime that runs the job and to nothing else.
    secret_env               JSONB             NOT NULL DEFAULT '{}'::jsonb,
    created_at               TIMESTAMPTZ       NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ       NOT NULL DEFAULT now(),
    -- The ranking metric the job's owner was told it trails its cohort on, written once.
    underperforming_metric   TEXT,
    -- The queue reason the job's owner was told it is held for, written once, when it had waited
    -- longer than it was to run.
    held_reason              TEXT,
    -- What its submitter was told at submit of other agents' jobs on the same hypothesis, queued,
    -- running or just completed, written once with the row: the coordinator is told it as
    -- job.duplicate, live and replayed.
    same_hypothesis          TEXT,
    -- The cluster the job was last placed on, written by the claim and kept when a requeue or a
    -- termination clears cluster_name. It is what lets a cluster be told of the jobs that just
    -- stopped being its own, and of no other cluster's.
    last_cluster             TEXT              NOT NULL DEFAULT '',
    -- Whose credential submitted the job, beside agent_id, which is whose job it is.
    actor_kind               TEXT              NOT NULL,
    actor_id                 TEXT              NOT NULL DEFAULT '',
    -- The git branch the job's commit was pushed on.
    branch                   TEXT              NOT NULL DEFAULT '',
    -- The key a submit stated to be safe to repeat: one agent's submits with one key are one job.
    submission_key           TEXT              NOT NULL DEFAULT '',
    updated_xid              BIGINT            NOT NULL DEFAULT pg_current_xact_id()::text::bigint,
    queued_xid               BIGINT,
    submitted_xid            BIGINT,
    status_xid               BIGINT            NOT NULL DEFAULT pg_current_xact_id()::text::bigint,
    not_admitted_xid         BIGINT            NOT NULL DEFAULT pg_current_xact_id()::text::bigint,
    underperforming_xid      BIGINT,
    held_xid                 BIGINT,
    same_hypothesis_xid      BIGINT,
    -- The environment a job's commit declares: admission claims the job only once it is ready, in
    -- its image. NULL for a commit that declares none.
    environment_key          TEXT              REFERENCES environments (key),
    CONSTRAINT experiments_queue_reason_consistent
        CHECK ((status = 'QUEUED') = (not_admitted_reason IS NOT NULL)),
    CONSTRAINT experiments_reason_of_attempt
        CHECK (eviction_reason IS NULL OR status NOT IN ('SUBMITTED', 'RUNNING', 'COMPLETED')),
    CONSTRAINT experiments_active_has_data_key
        CHECK (data_access_key_id <> '' OR status IN ('COMPLETED', 'FAILED', 'EVICTED', 'REJECTED'))
);

CREATE INDEX idx_experiments_agent_id   ON experiments(agent_id);
CREATE INDEX idx_experiments_status     ON experiments(status);
-- Backs the settlement reconciler's scan for terminal experiments whose final usage hasn't been
-- durably written yet; stays tiny since settled rows drop out of it.
CREATE INDEX idx_experiments_unsettled  ON experiments(updated_at)
    WHERE quota_settled_at IS NULL AND status IN ('COMPLETED', 'FAILED', 'EVICTED', 'REJECTED');
CREATE INDEX idx_experiments_platform   ON experiments(platform_experiment_id);
CREATE INDEX idx_experiments_hypothesis ON experiments(hypothesis_id);
-- Every cluster-agent polls its desired workload set continuously, and ClaimSubmitted re-reads it
-- while holding the cross-replica admission lock. Partial on the two desired statuses so the
-- index stays proportional to what is in flight rather than to everything ever run.
CREATE INDEX idx_experiments_desired ON experiments(cluster_name, status)
    WHERE status IN ('SUBMITTED', 'RUNNING');
-- Quota and stage sweeps ask per (agent, platform experiment, status), see
-- GetAgentRunningExperiments/GetAgentQueuedExperiments, called once per agent per reconcile tick.
CREATE INDEX idx_experiments_agent_platform_status ON experiments(agent_id, platform_experiment_id, status);
CREATE UNIQUE INDEX idx_experiments_submission_key
    ON experiments(platform_experiment_id, agent_id, submission_key) WHERE submission_key <> '';

-- ---------------------------------------------------------------------------
-- hypothesis_findings: the post-run write-up an agent files after a job reaches a terminal
-- state, attached to the hypothesis the job tested. A hypothesis is the shared, reusable unit of
-- research knowledge in a platform experiment's idea pool, while a job is just one attempt at
-- testing it. One finding per job (UNIQUE on experiment_id). See
-- services/scheduler.WriteExperimentSummary.
-- ---------------------------------------------------------------------------

CREATE TABLE hypothesis_findings (
    id             TEXT        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    -- hypothesis_id names a hypothesis in the graph store; platform_experiment_id is its platform
    -- experiment, carried here so replay and the notify trigger need nothing outside this row.
    hypothesis_id  TEXT        NOT NULL,
    platform_experiment_id TEXT NOT NULL REFERENCES platform_experiments(id),
    experiment_id  TEXT        NOT NULL REFERENCES experiments(id) UNIQUE,
    agent_id       TEXT        NOT NULL REFERENCES agents(id),
    summary        TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Whose credential filed the finding, beside agent_id, which is whose finding it is.
    actor_kind     TEXT        NOT NULL,
    actor_id       TEXT        NOT NULL DEFAULT '',
    created_xid    BIGINT      NOT NULL DEFAULT pg_current_xact_id()::text::bigint
);
CREATE INDEX idx_hypothesis_findings_hypothesis ON hypothesis_findings(hypothesis_id);
CREATE INDEX idx_hypothesis_findings_platform ON hypothesis_findings(platform_experiment_id, created_at);

-- ---------------------------------------------------------------------------
-- hypothesis_comments: a freeform, job-independent note on a hypothesis (amend, abandon,
-- revise, cross-reference), as opposed to hypothesis_findings which is the measured result of
-- one terminal job. No idempotency key: an occasional duplicate under crash-restart is low-harm
-- noise.
-- ---------------------------------------------------------------------------

CREATE TABLE hypothesis_comments (
    id             TEXT        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    -- Same pair as hypothesis_findings above.
    hypothesis_id  TEXT        NOT NULL,
    platform_experiment_id TEXT NOT NULL REFERENCES platform_experiments(id),
    -- NULL on a comment no agent owns.
    agent_id       TEXT        REFERENCES agents(id),
    -- Kept for the rows that have it and no longer read or written: who wrote a comment is its
    -- actor, and whose it is agent_id.
    source         TEXT        NOT NULL DEFAULT 'agent',
    author         TEXT        NOT NULL DEFAULT '',
    text           TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Whose credential wrote the comment. A row from before the credential was kept is
    -- 'unverified' with no id, and author holds the name it stated.
    actor_kind     TEXT        NOT NULL,
    actor_id       TEXT        NOT NULL DEFAULT '',
    created_xid    BIGINT      NOT NULL DEFAULT pg_current_xact_id()::text::bigint
);
CREATE INDEX idx_hypothesis_comments_hypothesis ON hypothesis_comments(hypothesis_id);
CREATE INDEX idx_hypothesis_comments_platform ON hypothesis_comments(platform_experiment_id, created_at);

-- Each agent's last hl hypothesis checkout of a hypothesis: who works on an idea is seen from the
-- checkout, before a job of theirs shows it (domain.CheckoutClaim).
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
-- experiment_signups: agents enrolled in a platform experiment
-- ---------------------------------------------------------------------------

CREATE TABLE experiment_signups (
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    agent_id               TEXT        NOT NULL REFERENCES agents(id),
    signed_up_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- domain.SignupRole. Fixed at signup and never updated: a role change mid-run would
    -- retroactively rewrite who a completed cut applied to.
    role                   TEXT        NOT NULL DEFAULT 'competitor',
    -- domain.QuotaTier, or '' for the default (domain.ResolveQuotaTier). Fixed at signup for the
    -- same reason role is.
    quota_tier             TEXT        NOT NULL DEFAULT '',
    -- The agent's own object-store key, created on its first request and never rotated; '' until then.
    data_access_key_id     TEXT        NOT NULL DEFAULT '',
    data_secret_access_key TEXT        NOT NULL DEFAULT '',
    -- An agent that said it has nothing left to do: from then its machine is no longer wanted.
    -- Written once, by the agent, with the transaction that wrote it for the replay of agent.finished.
    finished_at            TIMESTAMPTZ,
    finished_reason        TEXT        NOT NULL DEFAULT '',
    finished_xid           BIGINT,
    -- A researcher of a running platform experiment told it has gone idle (agent.idle): no job of
    -- its own ran for the platform's agents.idle_notice_minutes and none is queued or running.
    -- When it was told, the transaction that told it for the replay, and what it was told. Told
    -- again every idle_notice_minutes while the stretch lasts: idle_repeat is 0 on the first
    -- notice of a stretch and counts the ones after it.
    idle_noticed_at        TIMESTAMPTZ,
    idle_xid               BIGINT,
    idle_detail            TEXT        NOT NULL DEFAULT '',
    idle_repeat            INT         NOT NULL DEFAULT 0,
    -- What an agent is launched with, fixed when it joins. hosted means the platform runs it, and
    -- launcher names the launcher that does: written here and never read from config again, so a
    -- change of the configured launcher moves new agents only. flavor is the id of the agent
    -- type it runs.
    hosted                 BOOLEAN     NOT NULL DEFAULT false,
    launcher               TEXT        NOT NULL DEFAULT '',
    flavor                 TEXT        NOT NULL DEFAULT '',
    hyperparameters        JSONB       NOT NULL DEFAULT '{}',
    model                  TEXT        NOT NULL DEFAULT '',
    -- The researcher whose place the platform added this one in, keeping its platform
    -- experiment's researchers (quota.KeepResearchers), '' for one nobody replaced, and the
    -- transaction that added it for the replay of agent.replaced. A researcher is replaced once.
    replaces               TEXT        NOT NULL DEFAULT '',
    replaces_xid           BIGINT,
    -- What became of a hosted agent's machine, written by its launcher's reconcile call.
    -- instance_name is the name it was last asked for under, kept so that an agent whose run is
    -- over is still named as it was created when config has changed since. A machine reported as
    -- exited counts one restart and is asked for again under a new name, until instance_stopped:
    -- then the agent is no longer asked for, and instance_exited_at and instance_exit_log say how
    -- it ended.
    instance_name          TEXT        NOT NULL DEFAULT '',
    instance_restarts      INTEGER     NOT NULL DEFAULT 0,
    -- When each restart counted in the current domain.AgentInstanceRestartWindow was spent.
    instance_restarted_at  TIMESTAMPTZ[] NOT NULL DEFAULT '{}',
    instance_stopped       BOOLEAN     NOT NULL DEFAULT false,
    instance_exited_at     TIMESTAMPTZ,
    instance_exit_log      TEXT        NOT NULL DEFAULT '',
    -- When instance_name was set: the moment a machine still starting is timed from.
    instance_asked_at      TIMESTAMPTZ,
    -- When the launcher first reported the name: a machine asked for and never reported, one
    -- reported and gone, and one that stays starting are told apart by it.
    instance_seen_at       TIMESTAMPTZ,
    -- What the restarts were spent under (the image and the launcher's own launch inputs): a
    -- machine asked for under another gets a fresh budget, so that an agent stopped by a fault of
    -- the platform's is started again once the fault is fixed.
    instance_launch        TEXT        NOT NULL DEFAULT '',
    -- What the launcher last reported of the machine asked for: its state, the progress marker
    -- its command last printed, and when that marker was last seen to change, on this database's
    -- clock. end_reason is why the last machine that ended was given up on.
    instance_state         TEXT        NOT NULL DEFAULT '',
    instance_progress      TEXT        NOT NULL DEFAULT '',
    instance_progress_at   TIMESTAMPTZ,
    instance_end_reason    TEXT        NOT NULL DEFAULT '',
    -- Machines that ended through no fault of their command and were asked for again under a new
    -- name with no restart spent.
    instance_renewals      INTEGER     NOT NULL DEFAULT 0,
    -- What the agent on the machine asked for last said it is doing, a short sentence its harness
    -- prints beside its progress marker, and when the platform first read that sentence. The
    -- latest only. Empty while no machine is asked for and until one says something.
    instance_activity      TEXT        NOT NULL DEFAULT '',
    instance_activity_at   TIMESTAMPTZ,
    -- The provider's id of the machine the launcher last reported instance_name by, empty until it
    -- reports one and after the name is cleared. The launcher reads it by id when its provider's
    -- list leaves it out, so a list that trails its machines is never taken for one that lost them.
    instance_machine       TEXT        NOT NULL DEFAULT '',
    -- Whose credential added the agent (domain.Actor: kind and id).
    added_by_kind          TEXT        NOT NULL DEFAULT '',
    added_by_id            TEXT        NOT NULL DEFAULT '',
    -- The version of agent type flavor the signup is pinned to: written when it joins, and moved
    -- by an edit of its type while its run goes on, in the transaction that recorded
    -- instructions_xid for the replay of agent.instructions.
    agent_type_version     INTEGER,
    instructions_xid       BIGINT,
    PRIMARY KEY (platform_experiment_id, agent_id),
    CONSTRAINT experiment_signups_quota_tier CHECK (quota_tier IN ('', 'guaranteed', 'burst_only')),
    CONSTRAINT experiment_signups_agent_type
        FOREIGN KEY (flavor, agent_type_version) REFERENCES agent_type_versions(type_id, version)
);
CREATE UNIQUE INDEX experiment_signups_replaced_once ON experiment_signups (platform_experiment_id, replaces) WHERE replaces <> '';
CREATE INDEX idx_experiment_signups_platform ON experiment_signups(platform_experiment_id);
CREATE INDEX idx_experiment_signups_agent    ON experiment_signups(agent_id);
CREATE INDEX idx_experiment_signups_launcher ON experiment_signups(launcher) WHERE hosted;

-- ---------------------------------------------------------------------------
-- agent_quotas: per-agent allocation per platform experiment
-- ---------------------------------------------------------------------------

-- Allocation only. Current desired usage is derived from experiment rows in PostgreSQL;
-- observed terminal consumption lives in the metrics store. No usage total is persisted here.
CREATE TABLE agent_quotas (
    id                     TEXT             PRIMARY KEY,
    agent_id               TEXT             NOT NULL REFERENCES agents(id),
    platform_experiment_id TEXT             NOT NULL REFERENCES platform_experiments(id),
    guaranteed_accelerator_hours DOUBLE PRECISION NOT NULL,
    burst_accelerator_hours      DOUBLE PRECISION NOT NULL,
    created_at             TIMESTAMPTZ      NOT NULL DEFAULT now(),
    -- Lets a reconnecting watcher ask which allocations changed since its cursor: an allocation
    -- is rewritten in place by stage moves. Maintained by a trigger below, not by any writer.
    updated_at             TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_xid            BIGINT           NOT NULL DEFAULT pg_current_xact_id()::text::bigint,
    -- An allocation is a quantity of hours; a negative one is a bug, not a state.
    CONSTRAINT agent_quotas_non_negative CHECK (
        guaranteed_accelerator_hours >= 0 AND burst_accelerator_hours >= 0
    ),
    UNIQUE (agent_id, platform_experiment_id)
);
CREATE INDEX idx_agent_quotas_platform ON agent_quotas(platform_experiment_id);
CREATE INDEX idx_agent_quotas_agent    ON agent_quotas(agent_id);

-- ---------------------------------------------------------------------------
-- experiment_top3: top-3 agent placements per platform experiment
-- ---------------------------------------------------------------------------

CREATE TABLE experiment_top3 (
    platform_experiment_id TEXT             NOT NULL REFERENCES platform_experiments(id),
    agent_id               TEXT             NOT NULL REFERENCES agents(id),
    final_metric           DOUBLE PRECISION NOT NULL,
    recorded_at            TIMESTAMPTZ      NOT NULL DEFAULT now(),
    PRIMARY KEY (platform_experiment_id, agent_id)
);
CREATE INDEX idx_experiment_top3_agent ON experiment_top3(agent_id);

-- ---------------------------------------------------------------------------
-- platform_experiment_cuts: agents cut at a stage boundary. Terminal: jobs stopped and further
-- submissions rejected 422 for the rest of the experiment.
-- ---------------------------------------------------------------------------

CREATE TABLE platform_experiment_cuts (
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    agent_id               TEXT        NOT NULL REFERENCES agents(id),
    stage_index            INTEGER     NOT NULL,
    cut_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Why the platform cut the agent other than at a stage boundary: max_agents_lowered for one
    -- of the newest researchers above a max_agents lowered below those at work. Empty for a stage cut.
    reason                 TEXT        NOT NULL DEFAULT '',
    cut_xid                BIGINT      NOT NULL DEFAULT pg_current_xact_id()::text::bigint,
    PRIMARY KEY (platform_experiment_id, agent_id)
);
CREATE INDEX idx_pe_cuts_platform ON platform_experiment_cuts(platform_experiment_id);

-- ---------------------------------------------------------------------------
-- platform_experiment_stage_advances: one row per boundary crossed, committed with that
-- boundary's cuts and quota moves (see AdvanceStage). Quota moves would double-apply on a naive
-- retry; this row is what makes a crash mid-advance resume rather than re-run.
-- ---------------------------------------------------------------------------

CREATE TABLE platform_experiment_stage_advances (
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    stage_index            INTEGER     NOT NULL,
    advanced_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    advanced_xid           BIGINT      NOT NULL DEFAULT pg_current_xact_id()::text::bigint,
    PRIMARY KEY (platform_experiment_id, stage_index)
);

-- Each re-declaration of a running platform experiment's baseline to a stronger job that measured
-- the control: the experiment's history of its baseline, answered on its read and in the record of
-- its close. created_xid is the transaction that raised it, which platform_experiment.baseline
-- replays from.
CREATE TABLE baseline_raises (
    id                     BIGSERIAL   PRIMARY KEY,
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id) ON DELETE CASCADE,
    from_job_id            TEXT        NOT NULL,
    to_job_id              TEXT        NOT NULL,
    reason                 TEXT        NOT NULL,
    detail                 TEXT        NOT NULL,
    actor_kind             TEXT        NOT NULL,
    actor_id               TEXT        NOT NULL DEFAULT '',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_xid            BIGINT      NOT NULL
);
CREATE INDEX baseline_raises_by_experiment ON baseline_raises (platform_experiment_id, id);

-- Each direct message from a platform experiment's coordinator or owner to one of its agents,
-- told to that agent as agent.message, with its text, live and replayed from created_xid. sender
-- is coordinator or owner, what the agent is told it is from; actor_kind and actor_id say whose
-- credential sent it.
CREATE TABLE agent_messages (
    id                     BIGSERIAL   PRIMARY KEY,
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id) ON DELETE CASCADE,
    agent_id               TEXT        NOT NULL,
    sender                 TEXT        NOT NULL,
    text                   TEXT        NOT NULL,
    actor_kind             TEXT        NOT NULL,
    actor_id               TEXT        NOT NULL DEFAULT '',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_xid            BIGINT      NOT NULL
);
CREATE INDEX agent_messages_by_agent ON agent_messages (platform_experiment_id, agent_id, created_at);

-- Each agent that asked for the build of an environment (hl env build) while it was building,
-- told environment.status once that build ends, ready or failed, with what to do next: a job
-- waiting for it is told by its own status, and an agent that asked ahead had no job to be told
-- by. code_ref and image are what it last asked with. ended_xid is the transaction that ended
-- the build, NULL while it builds, which the event replays from with the state and detail it was
-- told. Asking again once it ended waits for the next build.
CREATE TABLE environment_waiters (
    environment_key        TEXT   NOT NULL REFERENCES environments (key) ON DELETE CASCADE,
    platform_experiment_id TEXT   NOT NULL REFERENCES platform_experiments (id) ON DELETE CASCADE,
    agent_id               TEXT   NOT NULL,
    code_ref               TEXT   NOT NULL,
    image                  TEXT   NOT NULL,
    state                  TEXT   NOT NULL DEFAULT '',
    detail                 TEXT   NOT NULL DEFAULT '',
    ended_xid              BIGINT,
    -- The transaction that first loaded the build into a node of one cluster while it builds,
    -- which told the agent environment.status loaded with loaded_detail: its jobs run there from
    -- then on, and wait for the push by themselves elsewhere, so a build that then ends ready
    -- tells that agent nothing more. A build that fails still tells it.
    loaded_xid             BIGINT,
    loaded_detail          TEXT   NOT NULL DEFAULT '',
    PRIMARY KEY (environment_key, platform_experiment_id, agent_id)
);
CREATE INDEX environment_waiters_ended ON environment_waiters (platform_experiment_id, ended_xid) WHERE ended_xid IS NOT NULL;
CREATE INDEX environment_waiters_loaded ON environment_waiters (platform_experiment_id, loaded_xid) WHERE loaded_xid IS NOT NULL;

-- A new platform experiment's form as a person left it, to be finished later. form is the
-- client's own and read by nothing here; failure is why the last create from it was refused.
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
        NEW.status_changed_at := now();
        NEW.status_changed_xid := pg_current_xact_id()::text::bigint;
        NEW.coordinator_reason := NULL;
    END IF;
    IF NEW.coordinator_reason IS DISTINCT FROM OLD.coordinator_reason THEN
        NEW.coordinator_reason_xid := pg_current_xact_id()::text::bigint;
    END IF;
    -- The run's length is part of what every agent plans its work by, as the brief is.
    IF NEW.description IS DISTINCT FROM OLD.description OR NEW.duration_hours IS DISTINCT FROM OLD.duration_hours THEN
        NEW.description_changed_at := now();
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

CREATE FUNCTION hypothesisloop_agent_instructions_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('agent.instructions', NEW.agent_id,
        NEW.agent_type_version::text, NEW.flavor, NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER signup_instructions_notify AFTER UPDATE OF instructions_xid ON experiment_signups
    FOR EACH ROW WHEN (NEW.instructions_xid IS DISTINCT FROM OLD.instructions_xid)
    EXECUTE FUNCTION hypothesisloop_agent_instructions_notify();

CREATE FUNCTION hypothesisloop_coordinator_instructions_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('agent.instructions', 'coordinator',
        NEW.coordinator_agent_type_version::text, NEW.coordinator_agent_type, NEW.id, 'coordinator');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER platform_experiments_coordinator_instructions_notify AFTER UPDATE OF coordinator_instructions_xid ON platform_experiments
    FOR EACH ROW WHEN (NEW.coordinator_instructions_xid IS DISTINCT FROM OLD.coordinator_instructions_xid)
    EXECUTE FUNCTION hypothesisloop_coordinator_instructions_notify();
