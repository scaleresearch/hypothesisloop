-- HypothesisLoop Autonomous Research Platform — canonical schema
-- Single source of truth; no migration history.
--
-- Every statement here is idempotent, and that is a hard requirement rather than a nicety:
-- "no migration history" only works if applying this file to an existing database is a no-op for
-- everything already present and adds everything that is not. It is applied on EVERY control-plane
-- start (controlplane/infra/podman.sh), not just the first — postgres only runs its
-- docker-entrypoint-initdb.d scripts against an empty data directory, so a stack that keeps its
-- volume across a redeploy would otherwise never see a single column added after day one, and the
-- new code would query fields the database does not have.
--
-- So: CREATE TABLE/INDEX use IF NOT EXISTS, CREATE TYPE is wrapped in a duplicate_object guard
-- (enums have no IF NOT EXISTS), functions use CREATE OR REPLACE, triggers are dropped first, and
-- a column added to a table definition above also gets an ALTER ... ADD COLUMN IF NOT EXISTS
-- below it, with one DEFAULT clause serving both paths so a fresh apply and an in-place add land
-- on the same backfill.
--
-- Metrics (time-series values emitted during job execution) are never stored here — they
-- live entirely in GreptimeDB, written via Prometheus remote-write and read back via PromQL
-- (see controlplane/services/registry). This database holds only relational state: agents,
-- experiments, quota, and cluster-agent coordination.

BEGIN;

-- ---------------------------------------------------------------------------
-- Enum types
-- ---------------------------------------------------------------------------

-- Exactly the set domain.ValidExperimentStatus accepts. It used to also carry DRAFT and
-- PROMOTED, which no Go constant named and nothing could ever write -- a row could only reach
-- them by a hand-written UPDATE, and every reader would then treat it as an unknown status.
DO $$ BEGIN
    CREATE TYPE experiment_status AS ENUM (
        'SUBMITTED',
        'QUEUED',
        'RUNNING',
        'COMPLETED',
        'FAILED',
        'EVICTED',
        'REJECTED'
    );
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

-- accelerator_type is deliberately plain TEXT, not an ENUM: the Accelerator catalog is entirely
-- operator-defined via hypothesisloop.yaml's accelerator_types (see config.AcceleratorTypeConfig) and any
-- vendor's model name is valid (NVIDIA H100, AMD MI300X, ...) — a closed Postgres enum here
-- would silently reject any Accelerator type the operator adds without a schema change, defeating the
-- whole point of the config-driven catalog.

DO $$ BEGIN
    CREATE TYPE platform_experiment_status AS ENUM (
        'draft',
        'open',
        'running',
        'closed'
    );
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE TYPE capacity_tier AS ENUM (
        'guaranteed',
        'burst'
    );
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

-- ---------------------------------------------------------------------------
-- agents
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS agents (
    id                TEXT             PRIMARY KEY,
    name              TEXT             NOT NULL,
    performance_score DOUBLE PRECISION NOT NULL DEFAULT 0.5,
    created_at        TIMESTAMPTZ      NOT NULL DEFAULT now()
);

ALTER TABLE agents DROP COLUMN IF EXISTS top3_count;
-- An agent row once said whether it stood for a person (kind 'human'). A person acts with their
-- own credential now and each record's actor says who wrote it, so the column and its check go.
ALTER TABLE agents DROP COLUMN IF EXISTS kind;

-- ---------------------------------------------------------------------------
-- platform_experiments — operator-defined compute envelopes
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS platform_experiments (
    id                   TEXT                       PRIMARY KEY,
    name                 TEXT                       NOT NULL,
    description          TEXT                       NOT NULL DEFAULT '',
    budget_accelerator_hours      DOUBLE PRECISION           NOT NULL,
    max_agents           INTEGER                    NOT NULL DEFAULT 100,
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
    CONSTRAINT platform_experiments_current_stage CHECK (current_stage >= 1),
    CONSTRAINT platform_experiments_budgets_non_negative CHECK (
        budget_accelerator_hours >= 0
    ),
    CONSTRAINT platform_experiments_max_agents CHECK (max_agents > 0),
    CONSTRAINT platform_experiments_report_interval CHECK (report_interval_seconds > 0),
    -- Operator's narrative verdict on the finished run: what was learned, which result won and
    -- why, what to carry into the next run. Deliberately prose and nothing else — the standings
    -- themselves are never stored here, they are derived from the metrics store on read (see
    -- GET /platform-experiments/{id}/results), so there is one source of truth for a number.
    summary              TEXT                       NOT NULL DEFAULT '',
    -- Empty marks experiments created before this policy existed; new API-created experiments
    -- explicitly store 'guaranteed'. An empty row resolves to guaranteed (domain.ResolveQuotaTier).
    default_quota_tier TEXT NOT NULL DEFAULT '',
    CONSTRAINT platform_experiments_default_quota_tier CHECK (default_quota_tier IN ('', 'guaranteed', 'burst_only')),
    created_at           TIMESTAMPTZ                NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ                NOT NULL DEFAULT now()
);

-- Who may register a hypothesis or submit a job was once gated per platform experiment on
-- whether the submitter was a person or an agent. Both columns go, and their checks with them.
ALTER TABLE platform_experiments DROP COLUMN IF EXISTS hypothesis_submit_policy;
ALTER TABLE platform_experiments DROP COLUMN IF EXISTS job_submit_policy;
-- The sentence a draft was created from (domain.PlatformExpDraft), and whose credential created
-- the platform experiment (domain.Actor: kind and id). Both empty on a row from before either.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS goal            TEXT NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS created_by_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS created_by_id   TEXT NOT NULL DEFAULT '';
-- What a draft's planner proposed (quota.Proposal as JSON) and when it answered. Both NULL until
-- it has, and proposed_at NULL again while a revision is being planned. A decline answers too:
-- it sets proposed_at and clears proposal, and is read as declined_at (domain.SetAnswered). The launcher keeps the planner's
-- machine while proposed_at is NULL, and after the answer until planner_ends_at.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS proposal    JSONB;
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS proposed_at TIMESTAMPTZ;
-- Why a draft's planner declined to propose: the goal could not be made into an experiment. A
-- decline answers the goal as a proposal does, so it sets proposed_at and clears proposal, and
-- the next proposal clears it. Empty when the planner never declined.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS declined_reason TEXT NOT NULL DEFAULT '';
-- The platform's record of a close (quota.recordClose), kept beside the coordinator's summary and
-- answered after it, so a summary written before the close never stands alone as the record.
-- Empty until written. A record that once was written as the summary itself moves here.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS close_record TEXT NOT NULL DEFAULT '';
UPDATE platform_experiments SET close_record = summary, summary = ''
WHERE close_record = '' AND summary LIKE 'Recorded by the platform:%';
-- How each agent's run stood at the moment of the close, the agent lines of its record, taken by
-- quota.Close before the close is written. The record waits for every job to settle, and by then
-- the machines are released and what their agents last said is gone. Empty until taken.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS close_agents TEXT NOT NULL DEFAULT '';
-- When the summary was last written, NULL while it never was.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS summary_at TIMESTAMPTZ;
-- The completed job that measured the baseline, and the estimate of a job that states none: the
-- hours that job was observed running, with headroom (0 when it was too short to be observed).
-- Both are written when the baseline is declared, and a platform experiment starts only with
-- them. A proposal stored before the two columns existed carried them, and gives them up here.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS baseline_job_id              TEXT             NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS job_estimated_duration_hours DOUBLE PRECISION NOT NULL DEFAULT 0;
UPDATE platform_experiments
SET baseline_job_id = proposal->>'baseline_job_id',
    job_estimated_duration_hours = COALESCE((proposal->>'job_estimated_duration_hours')::double precision, 0),
    proposal = proposal - 'baseline_job_id' - 'job_estimated_duration_hours'
WHERE proposal ? 'baseline_job_id';

-- What a person asked a draft's planner to change in its proposal, one row per request, and
-- whose credential asked (domain.Actor: kind and id). A draft has no hypothesis a comment could
-- hang on, so the requests are kept with the draft and read with it.
CREATE TABLE IF NOT EXISTS planning_requests (
    id                     BIGSERIAL   PRIMARY KEY,
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id) ON DELETE CASCADE,
    message                TEXT        NOT NULL,
    actor_kind             TEXT        NOT NULL,
    actor_id               TEXT        NOT NULL DEFAULT '',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS planning_requests_by_experiment ON planning_requests (platform_experiment_id, id);
-- The transaction that recorded the request, which platform_experiment.revision replays from.
-- NULL on a request from before the event, which is replayed to nobody.
ALTER TABLE planning_requests ADD COLUMN IF NOT EXISTS created_xid BIGINT;
-- Until when the launcher keeps the machine of a draft's planner once it answered, so that a
-- revision asked for soon after reaches the planner that is still there: the answer plus
-- planning.planner_kept_minutes, and never later than the planning limit the machine was rented
-- with. NULL while the planner works, and again once a revision gave the draft back to it. An
-- answer from before the column released its machine at once.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS planner_ends_at TIMESTAMPTZ;
UPDATE platform_experiments SET planner_ends_at = proposed_at WHERE proposed_at IS NOT NULL AND planner_ends_at IS NULL;
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS default_quota_tier TEXT NOT NULL DEFAULT '';

DO $$ BEGIN
    ALTER TABLE platform_experiments ADD CONSTRAINT platform_experiments_default_quota_tier
        CHECK (default_quota_tier IN ('', 'guaranteed', 'burst_only'));
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

-- Accelerators this platform experiment may hold in flight (SUBMITTED+RUNNING) at once, across
-- all its agents' jobs. NULL = use the config default (quota.default_max_concurrent_accelerators).
-- Speculative submission (see the settings of a cluster, below) makes capacity appear on demand and removes
-- the live-capacity ceiling that used to bound concurrency implicitly — this is the replacement.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS max_concurrent_accelerators INTEGER;

DO $$ BEGIN
    ALTER TABLE platform_experiments ADD CONSTRAINT platform_experiments_max_concurrent_accelerators
        CHECK (max_concurrent_accelerators IS NULL OR max_concurrent_accelerators > 0);
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

-- Per-agent CPU-hours and RAM/storage-hours quota tracking were never reachable through the UI
-- and are fully deleted: no code reads or writes these columns any more.
ALTER TABLE platform_experiments DROP COLUMN IF EXISTS budget_cpu_core_hours;
ALTER TABLE platform_experiments DROP COLUMN IF EXISTS budget_ram_gb_hours;
ALTER TABLE platform_experiments DROP COLUMN IF EXISTS budget_storage_gb_hours;

-- ---------------------------------------------------------------------------
-- experiments
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS experiments (
    id                       TEXT              PRIMARY KEY,
    parent_id                TEXT              REFERENCES experiments(id),
    agent_id                 TEXT              NOT NULL REFERENCES agents(id),
    cluster_name             TEXT              NOT NULL DEFAULT '',
    -- Every job must belong to exactly one platform experiment — there is no such thing as
    -- an unscoped job. Required so quota and the hypothesis pool below
    -- all have an unambiguous platform experiment to key off.
    platform_experiment_id   TEXT              NOT NULL REFERENCES platform_experiments(id),
    code_ref                 TEXT              NOT NULL,
    config_hash              TEXT              NOT NULL,
    data_ref                 TEXT              NOT NULL,
    job_spec                 JSONB             NOT NULL DEFAULT '{}'::jsonb,
    -- resolved_job_spec is job_spec as the runtime runs it (a slot's size, the image, the grace
    -- periods), written once at admission (ClaimSubmitted) onto the specific cluster the job
    -- landed on, reset to NULL on requeue (MarkQueued). NULL until admitted. job_spec itself is
    -- NEVER rewritten, so a
    -- GET always shows exactly what was submitted — see domain.Experiment.ResolvedJob/EffectiveJob.
    resolved_job_spec        JSONB,
    -- A hypothesis in the graph store (shared/graphdb), validated there at submit.
    hypothesis_id            TEXT              NOT NULL,
    hypothesis               TEXT              NOT NULL,
    theory                   TEXT              NOT NULL DEFAULT '',
    novelty_score            DOUBLE PRECISION  NOT NULL DEFAULT 0,
    accelerator_type                 TEXT              NOT NULL,
    accelerator_count                INTEGER           NOT NULL DEFAULT 1,
    capacity_tier            capacity_tier     NOT NULL DEFAULT 'guaranteed',
    status                   experiment_status NOT NULL,
    priority_score           DOUBLE PRECISION  NOT NULL DEFAULT 0,
    estimated_duration_hours DOUBLE PRECISION  NOT NULL,
    estimated_cost_acch       DOUBLE PRECISION  NOT NULL DEFAULT 0,
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
    -- quota_settled_at: set once this terminal experiment's final observed usage has been
    -- durably written to the metrics DB. NULL means settlement is outstanding (never attempted,
    -- or attempted and failed) — the durable signal a background reconciler scans for to retry
    -- writing it, surviving any crash/restart between the status transition and that write.
    quota_settled_at         TIMESTAMPTZ,
    -- attempt_count: how many attempts of this experiment have already run and failed. 0 on the
    -- first attempt. Only the control plane's gang retry writes it (see RequeueForRetry): a
    -- single-pod job's retries are the runtime's BackoffLimit and never reach here.
    attempt_count            INTEGER           NOT NULL DEFAULT 0,
    -- infra_requeue_count: how many of attempt_count's attempts were ended by the environment
    -- (an infrastructure-class eviction reason) and requeued for free. The agent's max_retries
    -- allowance is attempt_count - infra_requeue_count, so a job that keeps landing on broken
    -- hardware never spends the budget meant for its own bugs — while attempt_count still
    -- advances, which is what makes each requeue a distinct desired state the runtime rebuilds.
    infra_requeue_count      INTEGER           NOT NULL DEFAULT 0,
    -- The eviction reason of the last of those requeues, written with the count and never
    -- cleared: eviction_reason explains only the attempt that ended, and goes with the next claim.
    infra_requeue_reason     TEXT              NOT NULL DEFAULT '',
    -- tried_clusters: the placements of this job the environment failed, as a JSON array of
    -- {"cluster_id": ..., "at": <RFC3339>, "reason": <eviction reason code>}. Appended by
    -- RequeueInfrastructureFault in the write that spends infra_requeue_count; read at candidate
    -- selection to leave out a cluster that failed this job within
    -- scheduler.tried_cluster_ttl_seconds. The list delays a return to a cluster;
    -- infra_requeue_count is what bounds the attempts.
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
    updated_at               TIMESTAMPTZ       NOT NULL DEFAULT now()
);

-- No migration history: a fresh apply gets this from the column definition above, an existing
-- database from the ALTER, and both land on the same 0 backfill from the one DEFAULT clause.
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS attempt_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS infra_requeue_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS infra_requeue_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS resolved_job_spec JSONB;
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS tried_clusters JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS ran_on_accelerator_type TEXT NOT NULL DEFAULT '';
-- Jobs that ran before the column existed and can be placed again: the ones running, and the
-- preempted ones still waiting under the reason that used to stand for the fact.
UPDATE experiments SET ran_on_accelerator_type = accelerator_type
WHERE ran_on_accelerator_type = '' AND accelerator_type <> ''
  AND (status = 'RUNNING' OR (status = 'QUEUED' AND eviction_reason = 'preempted_for_guaranteed'));
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS data_access_key_id TEXT NOT NULL DEFAULT '';
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS data_secret_access_key TEXT NOT NULL DEFAULT '';
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS secret_env JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS run_reported_at TIMESTAMPTZ;

-- artifacts held a file list no code ever wrote and no code ever read. Jobs may push metrics and
-- nothing else, so a job could never report its own files; the real bytes live in the object
-- store and GET /experiments/{id}/data lists them there. A copy here would only drift.
ALTER TABLE experiments DROP COLUMN IF EXISTS artifacts;

-- objective was free text every job repeated. The experiment's metrics state what is optimized,
-- and a job says what it tests with its hypothesis and its theory.
ALTER TABLE experiments DROP COLUMN IF EXISTS objective;

-- Per-agent CPU-hours and RAM/storage-hours quota tracking were never reachable through the UI
-- and are fully deleted.
ALTER TABLE experiments DROP COLUMN IF EXISTS estimated_cpu_core_hours;
ALTER TABLE experiments DROP COLUMN IF EXISTS estimated_ram_gb_hours;
ALTER TABLE experiments DROP COLUMN IF EXISTS estimated_storage_gb_hours;

ALTER TABLE experiments ADD COLUMN IF NOT EXISTS not_admitted_reason TEXT;
DO $$ BEGIN
    ALTER TABLE experiments ADD CONSTRAINT experiments_queue_reason_consistent
        CHECK ((status = 'QUEUED') = (not_admitted_reason IS NOT NULL));
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

-- A placed or completed job has no ended attempt to explain. A FAILED row from before a failure
-- recorded its cause carries the reason of an attempt that was requeued.
UPDATE experiments SET eviction_reason = NULL
WHERE eviction_reason IS NOT NULL
  AND (status IN ('SUBMITTED', 'RUNNING', 'COMPLETED')
       OR (status = 'FAILED' AND split_part(eviction_reason, ':', 1) NOT IN ('container_failed', 'oom_killed')));
DO $$ BEGIN
    ALTER TABLE experiments ADD CONSTRAINT experiments_reason_of_attempt
        CHECK (eviction_reason IS NULL OR status NOT IN ('SUBMITTED', 'RUNNING', 'COMPLETED'));
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

-- The ranking metric the job's owner was told it trails its cohort on, written once.
ALTER TABLE experiments DROP COLUMN IF EXISTS underperforming_notified_at;
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS underperforming_metric TEXT;

-- The queue reason the job's owner was told it is held for, written once, when it had waited
-- longer than it was to run.
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS held_reason TEXT;

-- A job holds a cluster only while it is placed. The column once defaulted to the name
-- 'default', which is no cluster: a queued row carrying it is cleared.
ALTER TABLE experiments ALTER COLUMN cluster_name SET DEFAULT '';
UPDATE experiments SET cluster_name = '' WHERE cluster_name = 'default' AND status = 'QUEUED';

-- last_cluster is the cluster the job was last placed on, written by the claim and kept when a
-- requeue or a termination clears cluster_name. It is what lets a cluster be told of the jobs
-- that just stopped being its own, and of no other cluster's.
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS last_cluster TEXT NOT NULL DEFAULT '';
UPDATE experiments SET last_cluster = cluster_name
WHERE last_cluster = '' AND cluster_name <> '' AND submitted_at IS NOT NULL;

-- A row from before per-job keys has no store key and would run with empty AWS_* credentials, so
-- an active one is cancelled the way CancelExperiment would, and no active row may lack a key.
UPDATE experiments
SET status = CASE WHEN status IN ('QUEUED', 'SUBMITTED') THEN 'REJECTED' ELSE 'EVICTED' END::experiment_status,
    eviction_reason = 'cancelled: submitted before per-job storage keys, resubmit it',
    not_admitted_reason = NULL
WHERE data_access_key_id = '' AND status IN ('QUEUED', 'SUBMITTED', 'RUNNING');
DO $$ BEGIN
    ALTER TABLE experiments ADD CONSTRAINT experiments_active_has_data_key
        CHECK (data_access_key_id <> '' OR status IN ('COMPLETED', 'FAILED', 'EVICTED', 'REJECTED'));
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

CREATE INDEX IF NOT EXISTS idx_experiments_agent_id   ON experiments(agent_id);
CREATE INDEX IF NOT EXISTS idx_experiments_status     ON experiments(status);
-- Partial index backing the settlement reconciler's scan for terminal experiments whose final
-- usage hasn't been durably written yet — stays tiny since settled rows drop out of it.
CREATE INDEX IF NOT EXISTS idx_experiments_unsettled  ON experiments(updated_at)
    WHERE quota_settled_at IS NULL AND status IN ('COMPLETED', 'FAILED', 'EVICTED', 'REJECTED');
CREATE INDEX IF NOT EXISTS idx_experiments_platform   ON experiments(platform_experiment_id);
CREATE INDEX IF NOT EXISTS idx_experiments_hypothesis ON experiments(hypothesis_id);
-- Every cluster-agent polls its desired workload set continuously, and ClaimSubmitted re-reads it
-- while holding the cross-replica admission lock. Partial on the two desired statuses so the
-- index stays proportional to what is in flight rather than to everything ever run — without it
-- both sequential-scan the whole table as it grows, the second one inside the lock.
CREATE INDEX IF NOT EXISTS idx_experiments_desired ON experiments(cluster_name, status)
    WHERE status IN ('SUBMITTED', 'RUNNING');
-- Quota and stage sweeps ask per (agent, platform experiment, status) — see
-- GetAgentRunningExperiments/GetAgentQueuedExperiments, called once per agent per reconcile tick.
CREATE INDEX IF NOT EXISTS idx_experiments_agent_platform_status ON experiments(agent_id, platform_experiment_id, status);

-- ---------------------------------------------------------------------------
-- hypothesis_findings — the post-run write-up an agent files after a job reaches a
-- terminal state, attached to the hypothesis the job tested (not the job itself). This is
-- deliberately where write-ups live: a hypothesis is the shared, reusable unit of research
-- knowledge in a platform experiment's idea pool, while a job is just one attempt at testing
-- it — other agents deciding whether to test the same hypothesis again want the accumulated
-- findings across every job that tried it, not one buried on a single job record. One
-- finding per job (UNIQUE on experiment_id): a job produces exactly one write-up, but a
-- hypothesis accumulates one per job that tested it. See services/scheduler.WriteExperimentSummary.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS hypothesis_findings (
    id             TEXT        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    -- hypothesis_id names a hypothesis in the graph store; platform_experiment_id is its platform
    -- experiment, carried here so replay and the notify trigger need nothing outside this row.
    hypothesis_id  TEXT        NOT NULL,
    platform_experiment_id TEXT NOT NULL REFERENCES platform_experiments(id),
    experiment_id  TEXT        NOT NULL REFERENCES experiments(id) UNIQUE,
    agent_id       TEXT        NOT NULL REFERENCES agents(id),
    summary        TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Nullable here only so an existing table can take the column; db.DropLegacyHypotheses backfills
-- it and sets NOT NULL.
ALTER TABLE hypothesis_findings ADD COLUMN IF NOT EXISTS platform_experiment_id TEXT REFERENCES platform_experiments(id);

CREATE INDEX IF NOT EXISTS idx_hypothesis_findings_hypothesis ON hypothesis_findings(hypothesis_id);
CREATE INDEX IF NOT EXISTS idx_hypothesis_findings_platform ON hypothesis_findings(platform_experiment_id, created_at);

-- ---------------------------------------------------------------------------
-- hypothesis_comments — a freeform, job-independent note on a hypothesis (amend, abandon,
-- revise, cross-reference), as opposed to hypothesis_findings which is the measured result of
-- one terminal job. Lets an agent record "abandoning this, ruled out by X" without having to
-- burn a trial first. No idempotency key: an occasional duplicate under crash-restart is
-- low-harm noise, not a correctness bug — see plan.md.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS hypothesis_comments (
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
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- No migration history: one DEFAULT clause backfills existing rows to 'agent'/'' whether the
-- table is created fresh or altered in place. platform_experiment_id as on hypothesis_findings.
ALTER TABLE hypothesis_comments ALTER COLUMN agent_id DROP NOT NULL;
ALTER TABLE hypothesis_comments ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'agent';
ALTER TABLE hypothesis_comments ADD COLUMN IF NOT EXISTS author TEXT NOT NULL DEFAULT '';
ALTER TABLE hypothesis_comments ADD COLUMN IF NOT EXISTS platform_experiment_id TEXT REFERENCES platform_experiments(id);

-- Whose credential submitted the job and filed the finding, beside agent_id, which is whose job
-- and finding they are. A row from before the credential was kept is 'unverified' with no id.
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS actor_kind TEXT NOT NULL DEFAULT 'unverified';
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS actor_id   TEXT NOT NULL DEFAULT '';
-- The git branch the job's commit was pushed on. A job from before branches were kept has none.
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS branch     TEXT NOT NULL DEFAULT '';
-- The key a submit stated to be safe to repeat: one agent's submits with one key are one job.
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS submission_key TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_experiments_submission_key
    ON experiments(platform_experiment_id, agent_id, submission_key) WHERE submission_key <> '';
ALTER TABLE experiments ALTER COLUMN actor_kind DROP DEFAULT;
ALTER TABLE hypothesis_findings ADD COLUMN IF NOT EXISTS actor_kind TEXT NOT NULL DEFAULT 'unverified';
ALTER TABLE hypothesis_findings ADD COLUMN IF NOT EXISTS actor_id   TEXT NOT NULL DEFAULT '';
ALTER TABLE hypothesis_findings ALTER COLUMN actor_kind DROP DEFAULT;

-- Whose credential wrote the comment, beside agent_id, which is whose comment it is. A row from
-- before the credential was kept is 'unverified' with no id, and author holds the name it stated;
-- no new row sets author.
ALTER TABLE hypothesis_comments ADD COLUMN IF NOT EXISTS actor_kind TEXT NOT NULL DEFAULT 'unverified';
ALTER TABLE hypothesis_comments ADD COLUMN IF NOT EXISTS actor_id   TEXT NOT NULL DEFAULT '';
ALTER TABLE hypothesis_comments ALTER COLUMN actor_kind DROP DEFAULT;

CREATE INDEX IF NOT EXISTS idx_hypothesis_comments_hypothesis ON hypothesis_comments(hypothesis_id);
CREATE INDEX IF NOT EXISTS idx_hypothesis_comments_platform ON hypothesis_comments(platform_experiment_id, created_at);

-- Agents no longer ask each other for quota: one with none left finishes.
DROP TABLE IF EXISTS donation_requests;

-- ---------------------------------------------------------------------------
-- experiment_signups — agents enrolled in a platform experiment
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS experiment_signups (
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    agent_id               TEXT        NOT NULL REFERENCES agents(id),
    signed_up_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- domain.SignupRole. Fixed at signup and never updated: a role change mid-run would
    -- retroactively rewrite who a completed cut applied to. Only ranking reads it.
    role                   TEXT        NOT NULL DEFAULT 'competitor',
    -- domain.QuotaTier, or '' to defer to the agent's kind (domain.ResolveQuotaTier). Fixed at
    -- signup for the same reason role is: the allocation it decides is made once, at start, and
    -- changing the input afterwards would describe a split that never happened.
    quota_tier             TEXT        NOT NULL DEFAULT '',
    -- The agent's own object-store key, created on its first request and never rotated; '' until then.
    data_access_key_id     TEXT        NOT NULL DEFAULT '',
    data_secret_access_key TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (platform_experiment_id, agent_id),
    CONSTRAINT experiment_signups_quota_tier CHECK (quota_tier IN ('', 'guaranteed', 'burst_only'))
);
-- An agent that said it has nothing left to do: from then its machine is no longer wanted.
-- Written once, by the agent, with the transaction that wrote it for the replay of agent.finished.
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS finished_at     TIMESTAMPTZ;
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS finished_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS finished_xid    BIGINT;
-- A researcher of a running platform experiment told it has gone idle (agent.idle): no job or
-- check of its own ran for the platform's agents.idle_notice_minutes and none is queued or
-- running. When it was told, the transaction that told it for the replay, and what it was told.
-- Told again every idle_notice_minutes while the stretch lasts: idle_repeat is 0 on the first
-- notice of a stretch and counts the ones after it.
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS idle_noticed_at TIMESTAMPTZ;
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS idle_xid        BIGINT;
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS idle_detail     TEXT NOT NULL DEFAULT '';
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS idle_repeat     INT  NOT NULL DEFAULT 0;

ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS role       TEXT NOT NULL DEFAULT 'competitor';
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS quota_tier TEXT NOT NULL DEFAULT '';
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS data_access_key_id     TEXT NOT NULL DEFAULT '';
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS data_secret_access_key TEXT NOT NULL DEFAULT '';
-- What an agent is launched with, fixed when it joins. hosted means the platform runs it, and
-- launcher names the launcher that does: written here and never read from config again, so a
-- change of the configured launcher moves new agents only.
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS hosted          BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS launcher        TEXT    NOT NULL DEFAULT '';
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS flavor          TEXT    NOT NULL DEFAULT '';
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS hyperparameters JSONB   NOT NULL DEFAULT '{}';
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS model           TEXT    NOT NULL DEFAULT '';
-- What became of a hosted agent's machine, written by its launcher's reconcile call.
-- instance_name is the name it was last asked for under, kept so that an agent whose run is over
-- is still named as it was created when config has changed since. A machine reported as exited
-- counts one restart and is asked for again under a new name, until instance_stopped: then the
-- agent is no longer asked for, and instance_exited_at and instance_exit_log say how it ended.
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_name      TEXT        NOT NULL DEFAULT '';
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_restarts  INTEGER     NOT NULL DEFAULT 0;
-- When each restart counted in the current domain.AgentInstanceRestartWindow was spent.
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_restarted_at TIMESTAMPTZ[] NOT NULL DEFAULT '{}';
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_stopped   BOOLEAN     NOT NULL DEFAULT false;
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_exited_at TIMESTAMPTZ;
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_exit_log  TEXT        NOT NULL DEFAULT '';
-- When instance_name was set: the moment a machine still starting is timed from.
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_asked_at  TIMESTAMPTZ;
-- A machine asked for before the column existed is timed from this migration: with no time its
-- exit would never be recorded.
UPDATE experiment_signups SET instance_asked_at = now() WHERE instance_name <> '' AND instance_asked_at IS NULL;
-- instance_seen_at is when the launcher first reported the name: a machine asked for and never
-- reported, one reported and gone, and one that stays starting are told apart by it.
-- instance_launch identifies what the restarts were spent under (the image and the launcher's own
-- launch inputs): a machine asked for under another gets a fresh budget, so that an agent stopped
-- by a fault of the platform's is started again once the fault is fixed.
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_seen_at TIMESTAMPTZ;
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_launch  TEXT NOT NULL DEFAULT '';
-- What the launcher last reported of the machine asked for: its state, the progress marker its
-- command last printed, and when that marker was last seen to change, on this database's clock.
-- end_reason is why the last machine that ended was given up on.
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_state TEXT NOT NULL DEFAULT '';
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_progress TEXT NOT NULL DEFAULT '';
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_progress_at TIMESTAMPTZ;
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_end_reason TEXT NOT NULL DEFAULT '';
-- Machines that ended through no fault of their command and were asked for again under a new
-- name with no restart spent.
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_renewals INTEGER NOT NULL DEFAULT 0;
-- What the agent on the machine asked for last said it is doing, a short sentence its harness
-- prints beside its progress marker, and when the platform first read that sentence. The latest
-- only: there is no history. Empty while no machine is asked for and until one says something.
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_activity TEXT NOT NULL DEFAULT '';
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_activity_at TIMESTAMPTZ;
-- The provider's id of the machine the launcher last reported instance_name by, empty until it
-- reports one and after the name is cleared. The launcher reads it by id when its provider's list
-- leaves it out, so a list that trails its machines is never taken for one that lost them.
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS instance_machine TEXT NOT NULL DEFAULT '';

-- The inline CONSTRAINT runs only where the table does not exist yet, so an existing database
-- gets the check here.
DO $$ BEGIN
    ALTER TABLE experiment_signups ADD CONSTRAINT experiment_signups_quota_tier
        CHECK (quota_tier IN ('', 'guaranteed', 'burst_only'));
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

CREATE INDEX IF NOT EXISTS idx_experiment_signups_platform ON experiment_signups(platform_experiment_id);
CREATE INDEX IF NOT EXISTS idx_experiment_signups_agent    ON experiment_signups(agent_id);
CREATE INDEX IF NOT EXISTS idx_experiment_signups_launcher ON experiment_signups(launcher) WHERE hosted;

-- ---------------------------------------------------------------------------
-- agent_quotas — per-agent allocation per platform experiment
-- ---------------------------------------------------------------------------

-- Allocation only. Current desired usage is derived from experiment rows in PostgreSQL;
-- observed terminal consumption lives in the metrics store. No usage total is persisted here.
CREATE TABLE IF NOT EXISTS agent_quotas (
    id                     TEXT             PRIMARY KEY,
    agent_id               TEXT             NOT NULL REFERENCES agents(id),
    platform_experiment_id TEXT             NOT NULL REFERENCES platform_experiments(id),
    guaranteed_accelerator_hours    DOUBLE PRECISION NOT NULL,
    burst_accelerator_hours         DOUBLE PRECISION NOT NULL,
    -- An allocation is a quantity of hours; a negative one is a bug, not a state. Without this,
    -- a stale snapshot or a negative stage delta silently produced one, and the agent
    -- got rejections explaining it had "-3.2 hours remaining".
    CONSTRAINT agent_quotas_non_negative CHECK (
        guaranteed_accelerator_hours >= 0 AND burst_accelerator_hours >= 0
    ),
    created_at             TIMESTAMPTZ      NOT NULL DEFAULT now(),
    UNIQUE (agent_id, platform_experiment_id)
);

-- updated_at exists so a reconnecting watcher can ask "which allocations changed since my
-- cursor" — an allocation is rewritten in place by stage moves, so without it the
-- row carries no evidence that it ever moved. Maintained by a trigger below, not by any writer:
-- there is one clock for this column and no write path can forget it.
ALTER TABLE agent_quotas ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- Per-agent CPU-hours and RAM/storage-hours quota tracking were never reachable through the UI
-- and are fully deleted.
ALTER TABLE agent_quotas DROP COLUMN IF EXISTS guaranteed_cpu_core_hours;
ALTER TABLE agent_quotas DROP COLUMN IF EXISTS burst_cpu_core_hours;
ALTER TABLE agent_quotas DROP COLUMN IF EXISTS guaranteed_ram_gb_hours;
ALTER TABLE agent_quotas DROP COLUMN IF EXISTS burst_ram_gb_hours;
ALTER TABLE agent_quotas DROP COLUMN IF EXISTS guaranteed_storage_gb_hours;
ALTER TABLE agent_quotas DROP COLUMN IF EXISTS burst_storage_gb_hours;
-- A draft's planner once held a quota, the planning allowance its baseline job ran against. A
-- planner runs no job now, so the rows left from then go.
DELETE FROM agent_quotas q USING experiment_signups s
WHERE s.platform_experiment_id = q.platform_experiment_id AND s.agent_id = q.agent_id AND s.flavor = 'planner';

CREATE INDEX IF NOT EXISTS idx_agent_quotas_platform ON agent_quotas(platform_experiment_id);
CREATE INDEX IF NOT EXISTS idx_agent_quotas_agent    ON agent_quotas(agent_id);

-- ---------------------------------------------------------------------------
-- experiment_top3 — top-3 agent placements per platform experiment
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS experiment_top3 (
    platform_experiment_id TEXT             NOT NULL REFERENCES platform_experiments(id),
    agent_id               TEXT             NOT NULL REFERENCES agents(id),
    final_metric           DOUBLE PRECISION NOT NULL,
    recorded_at            TIMESTAMPTZ      NOT NULL DEFAULT now(),
    PRIMARY KEY (platform_experiment_id, agent_id)
);

CREATE INDEX IF NOT EXISTS idx_experiment_top3_agent ON experiment_top3(agent_id);

-- ---------------------------------------------------------------------------
-- platform_experiment_cuts — agents cut at a stage boundary. Terminal: jobs stopped and
-- further submissions rejected 422 for the rest of the experiment.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS platform_experiment_cuts (
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    agent_id               TEXT        NOT NULL REFERENCES agents(id),
    stage_index            INTEGER     NOT NULL,
    cut_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (platform_experiment_id, agent_id)
);

CREATE INDEX IF NOT EXISTS idx_pe_cuts_platform ON platform_experiment_cuts(platform_experiment_id);

-- ---------------------------------------------------------------------------
-- platform_experiment_stage_advances — one row per boundary crossed, committed with that
-- boundary's cuts and quota moves (see AdvanceStage). Quota moves would double-apply on a naive
-- retry; this row is what makes a crash mid-advance resume rather than re-run.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS platform_experiment_stage_advances (
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    stage_index            INTEGER     NOT NULL,
    advanced_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (platform_experiment_id, stage_index)
);

-- ---------------------------------------------------------------------------
-- Change notification — the source of GET /watch's live event stream
-- ---------------------------------------------------------------------------
--
-- Every event about a Postgres row is emitted by an AFTER trigger on the row that changed, so the
-- NOTIFY is part of the very transaction that wrote it: a rolled-back write emits nothing, and no
-- event can ever describe a state the database did not commit. Hypotheses live in the graph store,
-- which has no NOTIFY, so their events are sent by the registry after the graph write commits,
-- through the same channel and payload.
--
-- The payload is a small typed record — kind, subject, new value, a qualifying detail, the two
-- scoping ids, and a cursor — never a copy of the row. A client wanting detail follows with a
-- normal GET, so no read path is duplicated here and nothing is persisted twice.
--
-- An event's position is the id of the transaction that wrote it, recorded in the *_xid column
-- beside each timestamp replay reads. Its cursor is the xmin of the writer's snapshot: every
-- transaction below it had finished, so its NOTIFY was delivered first, and a client resuming
-- from it misses nothing. Replay (see db.EventsStore) re-derives events from the *_xid columns,
-- so a live event and its replayed twin carry the same position. Timestamps are not commit
-- ordered and are not used for this.
--
-- The columns below exist for that second half and live here, beside the triggers that maintain
-- them, because that is the only thing they are for. Each records WHEN one field of its own row
-- last moved — a row timestamp, not a second copy of the field: the value itself is still read
-- from the one column that holds it, and these say nothing a reader could disagree with it about.
-- Without them a rewritten-in-place row carries no evidence it ever changed, and a reconnecting
-- watcher could not tell a verdict settled an hour ago from one settled while it was away. They
-- are NULL until the field first moves — a row still holding what it was created with has no such
-- moment — and no Go writer sets them: one clock per column, and no write path can forget it.
ALTER TABLE platform_experiments  ADD COLUMN IF NOT EXISTS status_changed_at      TIMESTAMPTZ;
ALTER TABLE platform_experiments  ADD COLUMN IF NOT EXISTS description_changed_at TIMESTAMPTZ;
ALTER TABLE platform_experiments  ADD COLUMN IF NOT EXISTS status_changed_xid      BIGINT;
ALTER TABLE platform_experiments  ADD COLUMN IF NOT EXISTS description_changed_xid BIGINT;
-- The status_reason its coordinator was last told the experiment waits for it with, NULL while it
-- waits for someone else. A status_reason is derived on every read and stored nowhere, so a
-- change of it writes no row: this write is the event, live and replayed, as held_reason is of a
-- job. A change of status clears it, so what it holds is of the status the row has.
ALTER TABLE platform_experiments  ADD COLUMN IF NOT EXISTS coordinator_reason     TEXT;
ALTER TABLE platform_experiments  ADD COLUMN IF NOT EXISTS coordinator_reason_xid BIGINT;
ALTER TABLE experiments           ADD COLUMN IF NOT EXISTS updated_xid  BIGINT NOT NULL DEFAULT pg_current_xact_id()::text::bigint;
ALTER TABLE experiments           ADD COLUMN IF NOT EXISTS queued_xid    BIGINT;
ALTER TABLE experiments           ADD COLUMN IF NOT EXISTS submitted_xid BIGINT;
ALTER TABLE experiments           ADD COLUMN IF NOT EXISTS status_xid       BIGINT NOT NULL DEFAULT pg_current_xact_id()::text::bigint;
ALTER TABLE experiments           ADD COLUMN IF NOT EXISTS not_admitted_xid BIGINT NOT NULL DEFAULT pg_current_xact_id()::text::bigint;
ALTER TABLE experiments           ADD COLUMN IF NOT EXISTS underperforming_xid BIGINT;
ALTER TABLE experiments           ADD COLUMN IF NOT EXISTS held_xid BIGINT;
ALTER TABLE agent_quotas          ADD COLUMN IF NOT EXISTS updated_xid   BIGINT NOT NULL DEFAULT pg_current_xact_id()::text::bigint;
ALTER TABLE hypothesis_findings   ADD COLUMN IF NOT EXISTS created_xid   BIGINT NOT NULL DEFAULT pg_current_xact_id()::text::bigint;
ALTER TABLE hypothesis_comments   ADD COLUMN IF NOT EXISTS created_xid   BIGINT NOT NULL DEFAULT pg_current_xact_id()::text::bigint;
ALTER TABLE platform_experiment_stage_advances ADD COLUMN IF NOT EXISTS advanced_xid BIGINT NOT NULL DEFAULT pg_current_xact_id()::text::bigint;
ALTER TABLE platform_experiment_cuts           ADD COLUMN IF NOT EXISTS cut_xid      BIGINT NOT NULL DEFAULT pg_current_xact_id()::text::bigint;
UPDATE platform_experiments SET status_changed_xid = pg_current_xact_id()::text::bigint
    WHERE status_changed_at IS NOT NULL AND status_changed_xid IS NULL;
UPDATE platform_experiments SET description_changed_xid = pg_current_xact_id()::text::bigint
    WHERE description_changed_at IS NOT NULL AND description_changed_xid IS NULL;
UPDATE experiments SET queued_xid = pg_current_xact_id()::text::bigint WHERE queued_at IS NOT NULL AND queued_xid IS NULL;
UPDATE experiments SET submitted_xid = pg_current_xact_id()::text::bigint WHERE submitted_at IS NOT NULL AND submitted_xid IS NULL;

-- When the transaction at an event's position began. Replay re-derives events from rows, and a
-- row keeps a time beside few of its *_xid columns, so the time of a position is kept here once,
-- by the one function every event passes through. It holds no event: one row per transaction.
CREATE TABLE IF NOT EXISTS event_times (
    xid         BIGINT      PRIMARY KEY,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE OR REPLACE FUNCTION hypothesisloop_notify_event(
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

DROP FUNCTION IF EXISTS hypothesisloop_notify_event(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, TIMESTAMPTZ);

-- updated_xid moves on every write, as updated_at does. A waypoint's xid moves when its
-- timestamp does; a waypoint is only ever set from NULL, so the comparison is exact. status_xid
-- and not_admitted_xid move only with the column the live event announces, so a replayed event
-- carries the position its live twin did even after an unrelated later write to the row.
CREATE OR REPLACE FUNCTION hypothesisloop_experiments_touch() RETURNS trigger AS $$
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
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS experiments_touch ON experiments;
CREATE TRIGGER experiments_touch BEFORE INSERT OR UPDATE ON experiments
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_experiments_touch();

CREATE OR REPLACE FUNCTION hypothesisloop_experiments_notify() RETURNS trigger AS $$
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
    IF NEW.underperforming_metric IS NOT NULL
       AND (TG_OP = 'INSERT' OR NEW.underperforming_metric IS DISTINCT FROM OLD.underperforming_metric) THEN
        PERFORM hypothesisloop_notify_event('job.underperforming', NEW.id, NEW.underperforming_metric, '',
            NEW.platform_experiment_id, NEW.agent_id);
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS experiments_notify ON experiments;
CREATE TRIGGER experiments_notify AFTER INSERT OR UPDATE ON experiments
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_experiments_notify();

CREATE OR REPLACE FUNCTION hypothesisloop_agent_quotas_touch() RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    NEW.updated_xid := pg_current_xact_id()::text::bigint;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS agent_quotas_touch ON agent_quotas;
CREATE TRIGGER agent_quotas_touch BEFORE UPDATE ON agent_quotas
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_agent_quotas_touch();

CREATE OR REPLACE FUNCTION hypothesisloop_agent_quotas_notify() RETURNS trigger AS $$
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

DROP TRIGGER IF EXISTS agent_quotas_notify ON agent_quotas;
CREATE TRIGGER agent_quotas_notify AFTER INSERT OR UPDATE ON agent_quotas
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_agent_quotas_notify();

CREATE OR REPLACE FUNCTION hypothesisloop_platform_experiments_touch() RETURNS trigger AS $$
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

DROP TRIGGER IF EXISTS platform_experiments_touch ON platform_experiments;
CREATE TRIGGER platform_experiments_touch BEFORE UPDATE ON platform_experiments
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_platform_experiments_touch();

CREATE OR REPLACE FUNCTION hypothesisloop_platform_experiments_notify() RETURNS trigger AS $$
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

DROP TRIGGER IF EXISTS platform_experiments_notify ON platform_experiments;
CREATE TRIGGER platform_experiments_notify AFTER UPDATE ON platform_experiments
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_platform_experiments_notify();

-- Findings and comments both name the hypothesis they hang off as their subject, not their own
-- id: the hypothesis is what a reader would GET, and it is what the pool is organised around.
CREATE OR REPLACE FUNCTION hypothesisloop_findings_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('finding.new', NEW.hypothesis_id, NEW.experiment_id, '',
        NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS hypothesis_findings_notify ON hypothesis_findings;
CREATE TRIGGER hypothesis_findings_notify AFTER INSERT ON hypothesis_findings
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_findings_notify();

-- A comment's value is the kind of credential that wrote it and its detail the comment's id, so
-- a reader tells a person's comment from an agent's and reads that one comment.
CREATE OR REPLACE FUNCTION hypothesisloop_comments_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('comment.new', NEW.hypothesis_id, NEW.actor_kind, NEW.id,
        NEW.platform_experiment_id, COALESCE(NEW.agent_id, ''));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS hypothesis_comments_notify ON hypothesis_comments;
CREATE TRIGGER hypothesis_comments_notify AFTER INSERT ON hypothesis_comments
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_comments_notify();

CREATE OR REPLACE FUNCTION hypothesisloop_stage_advance_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('stage.boundary', NEW.platform_experiment_id,
        NEW.stage_index::text, 'advanced', NEW.platform_experiment_id, '');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS stage_advances_notify ON platform_experiment_stage_advances;
CREATE TRIGGER stage_advances_notify AFTER INSERT ON platform_experiment_stage_advances
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_stage_advance_notify();

-- A cut is committed in the same transaction as the advance that computed it, so a subscriber
-- sees the boundary and who it fell on together or not at all.
CREATE OR REPLACE FUNCTION hypothesisloop_cuts_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('stage.boundary', NEW.platform_experiment_id,
        NEW.stage_index::text, 'cut', NEW.platform_experiment_id, NEW.agent_id);
    -- The same row, said the other way round. stage.boundary is about the ladder and reaches
    -- everyone; agent.cut is about one agent and is the only event that tells it, without a GET,
    -- that the run is over for IT. It is agent-owned, so an agent's own subscription receives its
    -- own cut and is not woken by every rival's.
    PERFORM hypothesisloop_notify_event('agent.cut', NEW.agent_id,
        NEW.stage_index::text, '', NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS pe_cuts_notify ON platform_experiment_cuts;
CREATE TRIGGER pe_cuts_notify AFTER INSERT ON platform_experiment_cuts
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_cuts_notify();

CREATE OR REPLACE FUNCTION hypothesisloop_agent_finished_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('agent.finished', NEW.agent_id,
        '', NEW.finished_reason, NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS signup_finished_notify ON experiment_signups;
CREATE TRIGGER signup_finished_notify AFTER UPDATE OF finished_at ON experiment_signups
    FOR EACH ROW WHEN (OLD.finished_at IS NULL AND NEW.finished_at IS NOT NULL)
    EXECUTE FUNCTION hypothesisloop_agent_finished_notify();

CREATE OR REPLACE FUNCTION hypothesisloop_agent_idle_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('agent.idle', NEW.agent_id,
        CASE WHEN NEW.idle_repeat = 0 THEN 'new' ELSE 'continues' END, NEW.idle_detail, NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS signup_idle_notify ON experiment_signups;
CREATE TRIGGER signup_idle_notify AFTER UPDATE OF idle_xid ON experiment_signups
    FOR EACH ROW WHEN (NEW.idle_xid IS DISTINCT FROM OLD.idle_xid)
    EXECUTE FUNCTION hypothesisloop_agent_idle_notify();

-- A revision asked of a draft's planner. A pointer: what was asked is read from the draft.
CREATE OR REPLACE FUNCTION hypothesisloop_planning_request_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('platform_experiment.revision', NEW.platform_experiment_id,
        '', '', NEW.platform_experiment_id, '');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS planning_requests_notify ON planning_requests;
CREATE TRIGGER planning_requests_notify AFTER INSERT ON planning_requests
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_planning_request_notify();

-- ---------------------------------------------------------------------------
-- Tenancy
-- ---------------------------------------------------------------------------

-- id is the identity provider's subject. name is the display name the provider last stated for
-- the person, kept so that what they did reads by name to others and after they are gone; it is
-- empty until the provider states one. No email: the provider owns it and nothing here shows it.
CREATE TABLE IF NOT EXISTS users (
    id         TEXT        PRIMARY KEY,
    staff      BOOLEAN     NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE users ADD COLUMN IF NOT EXISTS name TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS orgs (
    id         TEXT        PRIMARY KEY,
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS org_members (
    org_id     TEXT        NOT NULL REFERENCES orgs(id),
    user_id    TEXT        NOT NULL REFERENCES users(id),
    role       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id),
    CONSTRAINT org_members_role CHECK (role IN ('owner', 'member'))
);
CREATE INDEX IF NOT EXISTS idx_org_members_user ON org_members(user_id);

-- An invite is a link: the platform stores no email and sends none. token_hash is the sha256 of
-- the whole token, which is shown once and stored nowhere. It is accepted once, before
-- expires_at and while not revoked. created_by is the person answerable for it.
CREATE TABLE IF NOT EXISTS org_invites (
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
CREATE INDEX IF NOT EXISTS idx_org_invites_org ON org_invites(org_id);

CREATE TABLE IF NOT EXISTS projects (
    id         TEXT        PRIMARY KEY,
    org_id     TEXT        NOT NULL REFERENCES orgs(id),
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);

-- key_hash is the sha256 of the whole token; the token itself is shown once and stored nowhere.
-- project_id NULL means the key reaches every project of its org.
CREATE TABLE IF NOT EXISTS api_keys (
    id         TEXT        PRIMARY KEY,
    key_hash   TEXT        NOT NULL UNIQUE,
    org_id     TEXT        NOT NULL REFERENCES orgs(id),
    project_id TEXT        REFERENCES projects(id),
    created_by TEXT        NOT NULL REFERENCES users(id),
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_api_keys_org ON api_keys(org_id);

-- A cluster exists for the platform once an org registers its name: a runtime token for an
-- unregistered name is refused, so a mistyped name cannot bring a cluster into being.
CREATE TABLE IF NOT EXISTS clusters (
    name       TEXT        PRIMARY KEY,
    org_id     TEXT        NOT NULL REFERENCES orgs(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_clusters_org ON clusters(org_id);
-- shared is the platform's decision that every org's jobs may be placed on the cluster; only
-- staff set it. Without it a cluster takes the jobs of its own org alone.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS shared BOOLEAN NOT NULL DEFAULT false;
-- reported_id is the identity the cluster's runtime states about itself, bound to the name on
-- its first report. The scheduler knows a connected cluster by it.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS reported_id TEXT UNIQUE;
-- job_image is the one image the cluster runs, as its runtime last reported it. Empty for a
-- cluster that runs whatever image a job names. Placement reads it.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS job_image TEXT NOT NULL DEFAULT '';
-- job_image_default marked a job_image that was only where a job naming none ran. Such a cluster
-- runs any image. A job placed before admission wrote the image keeps the one it runs in.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'clusters' AND column_name = 'job_image_default') THEN
        UPDATE experiments e SET resolved_job_spec = e.resolved_job_spec || jsonb_build_object('image', c.job_image)
        FROM clusters c
        WHERE c.name = e.cluster_name AND c.job_image <> '' AND e.status IN ('SUBMITTED', 'RUNNING')
          AND COALESCE(e.resolved_job_spec ->> 'image', '') = '';
        UPDATE clusters SET job_image = '' WHERE job_image_default;
        ALTER TABLE clusters DROP COLUMN job_image_default;
    END IF;
END $$;
-- priority orders the clusters a job may be placed on, higher first. The org that registered a
-- cluster sets 1 to 1000 on it. A shared cluster is the platform's and holds 0, so every org's
-- own compute is tried before it.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS priority INTEGER NOT NULL DEFAULT 100;
UPDATE clusters SET priority = 0 WHERE shared AND priority <> 0;
DO $$ BEGIN
    ALTER TABLE clusters ADD CONSTRAINT clusters_priority CHECK (
        (shared AND priority = 0) OR (NOT shared AND priority BETWEEN 1 AND 1000)
    );
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

-- What the org that registered a cluster sets on it in place of the scheduler's defaults
-- (domain.ClusterSettings). NULL is the default. They were rows of cluster_settings, keyed by
-- reported_id, which no cluster had before its agent first connected.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS scale_up_timeout_seconds INTEGER
    CHECK (scale_up_timeout_seconds > 0 AND scale_up_timeout_seconds < 1800);
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS max_speculative_accelerators INTEGER
    CHECK (max_speculative_accelerators > 0);
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS stuck_pending_timeout_seconds INTEGER
    CHECK (stuck_pending_timeout_seconds > 0);
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS status_silence_ceiling_seconds INTEGER
    CHECK (status_silence_ceiling_seconds > 0);
DO $$ BEGIN
    IF to_regclass('cluster_settings') IS NOT NULL THEN
        UPDATE clusters c SET scale_up_timeout_seconds = s.scale_up_timeout_seconds,
                              max_speculative_accelerators = s.max_speculative_accelerators,
                              stuck_pending_timeout_seconds = s.stuck_pending_timeout_seconds
        FROM cluster_settings s WHERE s.cluster_id = c.reported_id;
        DROP TABLE cluster_settings;
    END IF;
END $$;

-- An org turns a cluster on or off for its own jobs, whether the cluster is its own or one the
-- platform shares. A row is the org's decision that none of its jobs is placed on the cluster
-- from then on; the jobs already placed there finish. No row is enabled.
CREATE TABLE IF NOT EXISTS org_disabled_clusters (
    org_id       TEXT        NOT NULL REFERENCES orgs(id),
    cluster_name TEXT        NOT NULL REFERENCES clusters(name) ON DELETE CASCADE,
    disabled_by  TEXT        NOT NULL,
    disabled_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster_name, org_id)
);

-- minted_by is the key an API key was created with, NULL when a person created it. A key is
-- read as revoked while any key it descends from is.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS minted_by TEXT REFERENCES api_keys(id);

-- Everything that predates tenancy belongs to org 'default' and its project 'default'. Org is
-- always derived (job -> platform experiment -> project -> org), so these two columns are the
-- only places it is written. Both are added nullable, backfilled, then tightened, which leaves a
-- fresh database and an upgraded one on the same definition.
INSERT INTO orgs (id, name) VALUES ('default', 'default') ON CONFLICT (id) DO NOTHING;
INSERT INTO projects (id, org_id, name) VALUES ('default', 'default', 'default') ON CONFLICT (id) DO NOTHING;

ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS project_id TEXT REFERENCES projects(id);
UPDATE platform_experiments SET project_id = 'default' WHERE project_id IS NULL;
ALTER TABLE platform_experiments ALTER COLUMN project_id SET NOT NULL;
CREATE INDEX IF NOT EXISTS idx_platform_experiments_project ON platform_experiments(project_id);

ALTER TABLE agents ADD COLUMN IF NOT EXISTS org_id TEXT REFERENCES orgs(id);
UPDATE agents SET org_id = 'default' WHERE org_id IS NULL;
ALTER TABLE agents ALTER COLUMN org_id SET NOT NULL;
CREATE INDEX IF NOT EXISTS idx_agents_org ON agents(org_id);

-- A job's project is its platform experiment's. The column was a free-text tag before projects
-- existed.
ALTER TABLE experiments DROP COLUMN IF EXISTS project_id;

-- The clusters that ran jobs before clusters were registered belong to org 'default', or their
-- runtimes would be refused from the first boot on. An empty table is that first boot: later a
-- cluster comes into being only by an org registering it, and one its org removed must not come
-- back from the rows of the jobs it once ran, so this never runs against a table with a row.
-- last_cluster names every cluster a job was placed on, and only a placed job has one.
INSERT INTO clusters (name, org_id)
SELECT DISTINCT e.last_cluster, 'default' FROM experiments e
WHERE e.last_cluster <> '' AND NOT EXISTS (SELECT 1 FROM clusters)
ON CONFLICT (name) DO NOTHING;

-- human_writes held the account behind a person's hypothesis, link or comment, in a table no
-- read joined. Each record now carries its actor, and db.MoveHumanWrites folds the rows of an
-- older database into the records and drops the table.

-- When the platform experiment closed. An agent's token outlives the close by a fixed grace, and
-- updated_at moves again when the summary is filed.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS closed_at TIMESTAMPTZ;
-- Why it closed (domain.CloseReason) and, when a caller closed it, whose credential did. Both
-- are empty on one still running and on one closed before this was kept.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS close_reason   TEXT NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS closed_by_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS closed_by_id   TEXT NOT NULL DEFAULT '';
-- What the caller who closed it said of why, empty when it said nothing.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS close_message  TEXT NOT NULL DEFAULT '';
-- A coordinator the platform runs: one more machine of coordinator_launcher beside the agents',
-- on coordinator_model. Fixed at creation. The coordinator_instance_* columns say what became
-- of its machine, exactly as the instance_* columns of experiment_signups do for an agent's.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_hosted             BOOLEAN     NOT NULL DEFAULT false;
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_launcher           TEXT        NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_model              TEXT        NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_name      TEXT        NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_asked_at  TIMESTAMPTZ;
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_restarts  INTEGER     NOT NULL DEFAULT 0;
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_restarted_at TIMESTAMPTZ[] NOT NULL DEFAULT '{}';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_stopped   BOOLEAN     NOT NULL DEFAULT false;
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_exited_at TIMESTAMPTZ;
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_exit_log  TEXT        NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_seen_at   TIMESTAMPTZ;
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_launch    TEXT        NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_state TEXT NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_progress TEXT NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_progress_at TIMESTAMPTZ;
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_end_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_renewals INTEGER NOT NULL DEFAULT 0;
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_activity TEXT NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_activity_at TIMESTAMPTZ;
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS coordinator_instance_machine TEXT NOT NULL DEFAULT '';
-- How many machines the platform may run for this platform experiment, its coordinator included.
-- Fixed at creation from config agents.max_hosted_per_platform_experiment.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS max_hosted INTEGER NOT NULL DEFAULT 0;
-- shared is the platform's decision that every account may read the platform experiment and
-- everything in it; only staff set it. Nobody outside its org writes to it.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS shared BOOLEAN NOT NULL DEFAULT false;

-- An agent-launcher the platform knows. Its token works while its row exists, as a runtime's
-- does while its cluster is registered.
CREATE TABLE IF NOT EXISTS launchers (
    name       TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Whether the launcher holds machines, as its last question left it: it listed some, or its
-- answer asked for some. A launcher is removed only while this is false.
ALTER TABLE launchers ADD COLUMN IF NOT EXISTS holds_machines BOOLEAN NOT NULL DEFAULT false;
-- When the launcher was last answered, NULL for one never answered. A hosted agent waits without
-- a machine while its launcher is silent, and its status_reason says so (launcher_unreachable).
ALTER TABLE launchers ADD COLUMN IF NOT EXISTS answered_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_platform_experiments_coordinator_launcher ON platform_experiments(coordinator_launcher) WHERE coordinator_hosted;
UPDATE platform_experiments SET closed_at = updated_at WHERE status = 'closed' AND closed_at IS NULL;

-- A project once held one repository. The repository is the platform experiment's now.
DROP TABLE IF EXISTS project_repos;

-- An installation of the platform's GitHub App that an org connected: a GitHub account, with the
-- repositories its owner chose on GitHub. Every installation token is minted when asked for and
-- stored nowhere. account_login is the owner part of every repository it covers.
CREATE TABLE IF NOT EXISTS org_github_installations (
    org_id          TEXT        NOT NULL REFERENCES orgs(id),
    installation_id BIGINT      NOT NULL,
    account_login   TEXT        NOT NULL,
    connected_by    TEXT        NOT NULL,
    connected_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, installation_id)
);
-- Who connected it is an actor: connected_by_kind says whether connected_by is a user's id or
-- an API key's, or 'staff' for one an operator connected with hl-admin. A row from before the
-- kind was kept gets it from what its connected_by names. One with an empty connected_by, as
-- a staff token with no user behind it left, is 'unverified': who connected it was not kept.
-- The schema refuses to load while one names neither a user nor a key, or both: nothing is
-- guessed.
ALTER TABLE org_github_installations ADD COLUMN IF NOT EXISTS connected_by_kind TEXT NOT NULL DEFAULT '';
UPDATE org_github_installations i SET connected_by_kind = CASE
    WHEN i.connected_by = '' THEN 'unverified'
    WHEN i.connected_by = 'hl-admin' THEN 'staff'
    WHEN EXISTS (SELECT 1 FROM users u WHERE u.id = i.connected_by)
         AND NOT EXISTS (SELECT 1 FROM api_keys k WHERE k.id = i.connected_by) THEN 'user'
    WHEN EXISTS (SELECT 1 FROM api_keys k WHERE k.id = i.connected_by)
         AND NOT EXISTS (SELECT 1 FROM users u WHERE u.id = i.connected_by) THEN 'key'
    ELSE '' END
WHERE i.connected_by_kind = '';
DO $$
DECLARE unresolved TEXT;
BEGIN
    SELECT string_agg(org_id || '/' || installation_id || ' by ' || quote_literal(connected_by), ', ')
      INTO unresolved FROM org_github_installations WHERE connected_by_kind NOT IN ('user', 'key', 'staff', 'unverified');
    IF unresolved IS NOT NULL THEN
        RAISE EXCEPTION 'org_github_installations: who connected these is neither one user nor one API key: %', unresolved;
    END IF;
END $$;
-- Whether the account is a person's own or an organisation, User or Organization as GitHub
-- says at connect. Only GitHub knows it of a row from before it was kept, so that row holds ''
-- until the account is connected again.
ALTER TABLE org_github_installations ADD COLUMN IF NOT EXISTS account_type TEXT NOT NULL DEFAULT '';
-- On a person's own account GitHub creates a repository for that person alone, so the
-- authorisation they connected it with is kept, sealed by the store under a key derived from the
-- deployment's signing secret, and replaced at each use. NULL on an organisation, and on a
-- personal account connected by staff or before it was kept sealed: user_refresh_token held it
-- in the clear, and its owner connects the account again.
ALTER TABLE org_github_installations DROP COLUMN IF EXISTS user_refresh_token;
ALTER TABLE org_github_installations ADD COLUMN IF NOT EXISTS user_authorization BYTEA;
-- The code a platform experiment works on, owner/name on GitHub, and the image its hosted agents
-- and coordinator run. An empty image is the platform's own (config agents.image). Both are
-- fixed at creation.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS repository  TEXT NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS agent_image TEXT NOT NULL DEFAULT '';
-- The address the repository is cloned from, as the deployment's git host gave it at creation.
-- Every repository named before the column was on GitHub.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS repository_url TEXT NOT NULL DEFAULT '';
UPDATE platform_experiments SET repository_url = 'https://github.com/' || repository || '.git'
WHERE repository <> '' AND repository_url = '';
-- A platform experiment or draft whose repository the platform creates is stored without it, and
-- repository_account is the account it is created on: the repository pass
-- (quota.ProvideRepositories) creates it after the create answered, and none of its machines
-- starts before it is there. repository_error is what the git host answered on one closed
-- repository_not_created.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS repository_account TEXT NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS repository_error   TEXT NOT NULL DEFAULT '';

-- A draft's planner once had role baseline and was told apart by its flavor. It has its own role.
UPDATE experiment_signups SET role = 'planner' WHERE flavor = 'planner' AND role <> 'planner';
-- Whose credential added the agent (domain.Actor: kind and id). Both empty on a signup from
-- before this was kept.
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS added_by_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS added_by_id   TEXT NOT NULL DEFAULT '';
-- The run's clock. duration_hours is the run's length, counted from its start, which a running
-- one holds in starts_at, and started_at the moment a researcher was first at work in the running
-- platform experiment. ends_at is what closes it: a draft's planning or proposal limit, an open
-- one's baseline deadline, a running one's deadline for a researcher's machine to be running, and
-- once started_at is written, starts_at plus duration_hours. A row from
-- before the two columns, which has no duration_hours, counted its length from its start time or
-- its creation: it keeps that length and that end, and one that ran started then.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS duration_hours DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS started_at     TIMESTAMPTZ;
UPDATE platform_experiments pe
SET started_at = GREATEST(pe.starts_at, pe.created_at)
WHERE pe.started_at IS NULL AND pe.duration_hours = 0
  AND (pe.status = 'running'
       OR pe.status = 'closed' AND EXISTS (
           SELECT 1 FROM agent_quotas q JOIN experiment_signups s
             ON s.platform_experiment_id = q.platform_experiment_id AND s.agent_id = q.agent_id
           WHERE q.platform_experiment_id = pe.id AND s.role NOT IN ('baseline', 'planner')));
UPDATE platform_experiments
SET duration_hours = EXTRACT(EPOCH FROM ends_at - GREATEST(starts_at, created_at)) / 3600
WHERE duration_hours = 0 AND ends_at > GREATEST(starts_at, created_at)
  AND (status IN ('open', 'running') OR status = 'closed' AND (goal = '' OR starts_at > '0001-01-02'));

-- A baseline was once stated by its creator in place of a measured one, with the commit it stood
-- for kept beside it. Every baseline is measured by its declared job again, so both columns go
-- and the block the platform writes into the brief says the baseline was measured.
ALTER TABLE platform_experiments DROP COLUMN IF EXISTS stated_baseline;
ALTER TABLE platform_experiments DROP COLUMN IF EXISTS baseline_code_ref;
UPDATE platform_experiments
SET description = replace(description, 'BASELINE (written by the platform)', 'MEASURED BASELINE (written by the platform from the declared job)')
WHERE description LIKE '%BASELINE (written by the platform)%';

-- An open or running platform experiment holds in starts_at when its clock started: an open
-- one's baseline is measured from then, a running one's run. One created with none stated kept
-- the zero time, and its clock started at its creation, or at started_at where a researcher
-- already works.
UPDATE platform_experiments SET starts_at = COALESCE(started_at, created_at)
WHERE status IN ('open', 'running') AND starts_at < '0001-01-02';

-- An environment is an image the platform builds for an org from a repository's
-- .hypothesisloop/Dockerfile on one base image (domain.Environment). key names the three, so one
-- is built once. A row is made building, and the scheduler assigns its build to a cluster that
-- builds (clusters.builds), the attempt-th time at assigned_at, cluster_name empty while it is
-- assigned to none. That cluster reports it ready with image, repository@digest, or failed with
-- fault file. A build assigned for the last attempt allowed and not finished fails with fault
-- platform. A failed row is made building again by the next submit that names it.
CREATE TABLE IF NOT EXISTS environments (
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
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Rows from when one builder the control plane dialled built every org's files, keyed without
-- the org: each takes the org of the first job that named it, one no job named goes, and an
-- abandoned one is failed as the platform's fault.
ALTER TABLE environments ADD COLUMN IF NOT EXISTS org_id TEXT REFERENCES orgs (id);
ALTER TABLE environments ADD COLUMN IF NOT EXISTS fault TEXT NOT NULL DEFAULT '';
ALTER TABLE environments ADD COLUMN IF NOT EXISTS cluster_name TEXT NOT NULL DEFAULT '';
ALTER TABLE environments ADD COLUMN IF NOT EXISTS attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE environments ADD COLUMN IF NOT EXISTS assigned_at TIMESTAMPTZ;
-- The step the assigned cluster last reported of the attempt it builds, at progress_at. A new
-- assignment clears it.
ALTER TABLE environments ADD COLUMN IF NOT EXISTS progress TEXT NOT NULL DEFAULT '';
ALTER TABLE environments ADD COLUMN IF NOT EXISTS progress_at TIMESTAMPTZ;
-- The end of the output of the file's last failed build, kept while a later submit builds the
-- same file once more, so the job waiting for it says so. Cleared once it is built.
ALTER TABLE environments ADD COLUMN IF NOT EXISTS failed_log TEXT NOT NULL DEFAULT '';
ALTER TABLE environments DROP CONSTRAINT IF EXISTS environments_state_check;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'environments' AND column_name = 'attempts') THEN
        UPDATE environments v SET org_id = (
            SELECT p.org_id FROM experiments e
            JOIN platform_experiments pe ON pe.id = e.platform_experiment_id
            JOIN projects p ON p.id = pe.project_id
            WHERE e.environment_key = v.key ORDER BY e.created_at LIMIT 1)
        WHERE v.org_id IS NULL;
        DELETE FROM environments v WHERE v.org_id IS NULL;
        UPDATE environments SET fault = CASE state WHEN 'failed' THEN 'file' WHEN 'abandoned' THEN 'platform' ELSE '' END,
            state = CASE state WHEN 'abandoned' THEN 'failed' ELSE state END;
        ALTER TABLE environments DROP COLUMN attempts;
    END IF;
END $$;
ALTER TABLE environments ALTER COLUMN org_id SET NOT NULL;
ALTER TABLE environments ADD CONSTRAINT environments_state_check CHECK (
    state IN ('building', 'ready', 'failed') AND fault IN ('', 'file', 'platform') AND (state = 'failed') = (fault <> '')
    AND (cluster_name = '') = (assigned_at IS NULL));
DROP INDEX IF EXISTS environments_building;
CREATE INDEX IF NOT EXISTS environments_assigned ON environments (cluster_name) WHERE state = 'building';
-- builds is whether the cluster's runtime builds environments, as it last reported it.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS builds BOOLEAN NOT NULL DEFAULT false;
-- The environment a job's commit declares: admission claims the job only once it is ready, in
-- its image. NULL for a commit that declares none.
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS environment_key TEXT REFERENCES environments (key);
-- Agent machines run the platform's agent image and build nothing: only jobs run in an
-- environment.
ALTER TABLE platform_experiments DROP COLUMN IF EXISTS agent_environment_key;

COMMIT;
-- The time an open platform experiment's baseline waited on the platform, building the
-- environment of one of its jobs or starting one on its cluster, which moved its baseline_due_at
-- (ends_at) out by as much: baseline_held_seconds credited so far, and baseline_held_at the last
-- sweep that saw it waiting, NULL when the last one did not.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS baseline_held_seconds DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS baseline_held_at      TIMESTAMPTZ;
-- When the platform accepts a proposed draft as proposed (planning.auto_accept_seconds after the
-- proposal), unless a person accepts it sooner, holds it to edit it, revises it or closes it.
-- NULL on a draft that waits for a person, and on anything that is no proposed draft. While it is
-- set, the baseline agent the proposal signed up has its machine.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS accepts_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS platform_experiments_accepts_at ON platform_experiments (accepts_at) WHERE accepts_at IS NOT NULL;
-- The image of a building environment, repository@digest, once cluster loaded_cluster holds it in
-- the image store of its node loaded_node while the push goes on: a job pinned to that node runs it
-- before it is ready. A push that failed keeps it, so the node runs the build's jobs while it is
-- built again; the last attempt that loaded it names it. Empty otherwise, and cleared once the
-- build is ready or failed.
ALTER TABLE environments ADD COLUMN IF NOT EXISTS loaded_image   TEXT NOT NULL DEFAULT '';
ALTER TABLE environments ADD COLUMN IF NOT EXISTS loaded_node    TEXT NOT NULL DEFAULT '';
ALTER TABLE environments ADD COLUMN IF NOT EXISTS loaded_cluster TEXT NOT NULL DEFAULT '';
UPDATE environments SET loaded_cluster = cluster_name WHERE loaded_image <> '' AND loaded_cluster = '';
-- The image an environment's last layer takes the platform's tools from (domain.PlatformTools),
-- repository@digest, empty for one built on its job's image itself. base is then that image
-- without them, so a release that changes only the tools keeps the layers the file built.
ALTER TABLE environments ADD COLUMN IF NOT EXISTS tools TEXT NOT NULL DEFAULT '';
-- The repository whose environment file this is, of project project_id: its build reads it
-- (domain.EnvironmentGitSecret), and the key names it. Empty on rows from before, whose keys no
-- submit reaches again.
ALTER TABLE environments ADD COLUMN IF NOT EXISTS project_id     TEXT NOT NULL DEFAULT '';
ALTER TABLE environments ADD COLUMN IF NOT EXISTS git_repository TEXT NOT NULL DEFAULT '';
-- Each re-declaration of a running platform experiment's baseline to a stronger job that measured
-- the control: the experiment's history of its baseline, answered on its read and in the record of
-- its close. created_xid is the transaction that raised it, which platform_experiment.baseline
-- replays from.
CREATE TABLE IF NOT EXISTS baseline_raises (
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
CREATE INDEX IF NOT EXISTS baseline_raises_by_experiment ON baseline_raises (platform_experiment_id, id);

CREATE OR REPLACE FUNCTION hypothesisloop_baseline_raise_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('platform_experiment.baseline', NEW.platform_experiment_id,
        NEW.to_job_id, NEW.detail, NEW.platform_experiment_id, '');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS baseline_raises_notify ON baseline_raises;
CREATE TRIGGER baseline_raises_notify AFTER INSERT ON baseline_raises
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_baseline_raise_notify();

-- Each direct message from a platform experiment's coordinator or owner to one of its agents,
-- told to that agent as agent.message, with its text, live and replayed from created_xid. sender
-- is coordinator or owner, what the agent is told it is from; actor_kind and actor_id say whose
-- credential sent it.
CREATE TABLE IF NOT EXISTS agent_messages (
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
CREATE INDEX IF NOT EXISTS agent_messages_by_agent ON agent_messages (platform_experiment_id, agent_id, created_at);

CREATE OR REPLACE FUNCTION hypothesisloop_agent_message_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('agent.message', NEW.agent_id,
        NEW.sender, NEW.text, NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS agent_messages_notify ON agent_messages;
CREATE TRIGGER agent_messages_notify AFTER INSERT ON agent_messages
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_agent_message_notify();

-- Each agent that asked for the build of an environment (hl env build) while it was
-- building, told environment.status once that build ends, ready or failed, with what to do next:
-- a job waiting for it is told by its own status, and an agent that asked ahead had no job to be
-- told by. code_ref and image are what it last asked with. ended_xid is the transaction that
-- ended the build, NULL while it builds, which the event replays from with the state and detail
-- it was told. Asking again once it ended waits for the next build.
CREATE TABLE IF NOT EXISTS environment_waiters (
    environment_key        TEXT   NOT NULL REFERENCES environments (key) ON DELETE CASCADE,
    platform_experiment_id TEXT   NOT NULL REFERENCES platform_experiments (id) ON DELETE CASCADE,
    agent_id               TEXT   NOT NULL,
    code_ref               TEXT   NOT NULL,
    image                  TEXT   NOT NULL,
    state                  TEXT   NOT NULL DEFAULT '',
    detail                 TEXT   NOT NULL DEFAULT '',
    ended_xid              BIGINT,
    PRIMARY KEY (environment_key, platform_experiment_id, agent_id)
);
CREATE INDEX IF NOT EXISTS environment_waiters_ended ON environment_waiters (platform_experiment_id, ended_xid) WHERE ended_xid IS NOT NULL;
-- loaded_xid is the transaction that first loaded the build into a node of one cluster while it
-- builds, which told the agent environment.status loaded with loaded_detail: its jobs run there
-- from then on, and wait for the push by themselves elsewhere, so a build that then ends ready
-- tells that agent nothing more. A build that fails still tells it.
ALTER TABLE environment_waiters ADD COLUMN IF NOT EXISTS loaded_xid    BIGINT;
ALTER TABLE environment_waiters ADD COLUMN IF NOT EXISTS loaded_detail TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS environment_waiters_loaded ON environment_waiters (platform_experiment_id, loaded_xid) WHERE loaded_xid IS NOT NULL;

CREATE OR REPLACE FUNCTION hypothesisloop_environment_ended_notify() RETURNS trigger AS $$
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

DROP TRIGGER IF EXISTS environments_ended_notify ON environments;
CREATE TRIGGER environments_ended_notify AFTER UPDATE OF state ON environments
    FOR EACH ROW WHEN (OLD.state = 'building' AND NEW.state <> 'building')
    EXECUTE FUNCTION hypothesisloop_environment_ended_notify();

CREATE OR REPLACE FUNCTION hypothesisloop_environment_loaded_notify() RETURNS trigger AS $$
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

DROP TRIGGER IF EXISTS environments_loaded_notify ON environments;
CREATE TRIGGER environments_loaded_notify AFTER UPDATE OF loaded_image ON environments
    FOR EACH ROW WHEN (NEW.state = 'building' AND NEW.loaded_image <> '' AND OLD.loaded_image IS DISTINCT FROM NEW.loaded_image)
    EXECUTE FUNCTION hypothesisloop_environment_loaded_notify();

-- Checks (jobs that tested no hypothesis, to see code run) are gone: an agent runs its code on
-- its own machine, and every job tests a hypothesis.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'experiments' AND column_name = 'is_check') THEN
        DELETE FROM experiments WHERE is_check;
    END IF;
END $$;
ALTER TABLE experiments DROP COLUMN IF EXISTS is_check;

-- The closed run of the same project and repository a platform experiment continues, NULL for one
-- that starts fresh. Its agents read that run, and start from its baseline's commit.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS continues TEXT REFERENCES platform_experiments (id);

-- The commit researchers start from in place of the baseline's, a descendant of it, and which
-- earlier runs of the project and repository the agents read: every one, or none.
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS start_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE platform_experiments ADD COLUMN IF NOT EXISTS history TEXT NOT NULL DEFAULT 'prior_runs';
ALTER TABLE platform_experiments DROP CONSTRAINT IF EXISTS platform_experiments_history_check;
ALTER TABLE platform_experiments ADD CONSTRAINT platform_experiments_history_check CHECK (history IN ('prior_runs', 'none'));
