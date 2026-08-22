-- HypothesisLoop Autonomous Research Platform — canonical schema
-- Single source of truth; no migration history.
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
CREATE TYPE experiment_status AS ENUM (
    'SUBMITTED',
    'QUEUED',
    'ADMITTED',
    'RUNNING',
    'COMPLETED',
    'FAILED',
    'EVICTED',
    'REJECTED'
);

-- accelerator_type is deliberately plain TEXT, not an ENUM: the Accelerator catalog is entirely
-- operator-defined via hypothesisloop.yaml's accelerator_types (see config.AcceleratorTypeConfig) and any
-- vendor's model name is valid (NVIDIA H100, AMD MI300X, ...) — a closed Postgres enum here
-- would silently reject any Accelerator type the operator adds without a schema change, defeating the
-- whole point of the config-driven catalog.

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

-- hypothesis_status is the owning agent's own verdict on its claim (see domain.HypothesisStatus)
-- — a closed enum is correct here, unlike accelerator_type above: these four values are a fixed
-- design decision, not an operator-extensible catalog.
CREATE TYPE hypothesis_status AS ENUM (
    'open',
    'confirmed',
    'refuted',
    'inconclusive'
);

-- ---------------------------------------------------------------------------
-- agents
-- ---------------------------------------------------------------------------

CREATE TABLE agents (
    id                TEXT             PRIMARY KEY,
    name              TEXT             NOT NULL,
    performance_score DOUBLE PRECISION NOT NULL DEFAULT 0.5,
    top3_count        INTEGER          NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ      NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- platform_experiments — operator-defined compute envelopes
-- ---------------------------------------------------------------------------

CREATE TABLE platform_experiments (
    id                   TEXT                       PRIMARY KEY,
    name                 TEXT                       NOT NULL,
    description          TEXT                       NOT NULL DEFAULT '',
    budget_accelerator_hours      DOUBLE PRECISION           NOT NULL,
    budget_cpu_core_hours    DOUBLE PRECISION       NOT NULL DEFAULT 0,
    budget_ram_gb_hours      DOUBLE PRECISION       NOT NULL DEFAULT 0,
    budget_storage_gb_hours  DOUBLE PRECISION       NOT NULL DEFAULT 0,
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
        budget_accelerator_hours >= 0 AND budget_cpu_core_hours >= 0 AND
        budget_ram_gb_hours >= 0 AND budget_storage_gb_hours >= 0
    ),
    CONSTRAINT platform_experiments_max_agents CHECK (max_agents > 0),
    CONSTRAINT platform_experiments_report_interval CHECK (report_interval_seconds > 0),
    -- Operator's narrative verdict on the finished run: what was learned, which result won and
    -- why, what to carry into the next run. Deliberately prose and nothing else — the standings
    -- themselves are never stored here, they are derived from the metrics store on read (see
    -- GET /platform-experiments/{id}/results), so there is one source of truth for a number.
    summary              TEXT                       NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ                NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ                NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- hypotheses — the research-claim registry, scoped to a single platform experiment. Each
-- platform experiment accumulates its own shared pool of ideas: agents register (or
-- retrieve, if an equivalent one already exists *within that platform experiment*) a
-- hypothesis here before submitting a job that tests it. normalized_text (lowercased,
-- whitespace-collapsed) carries a UNIQUE index scoped per platform_experiment_id — the real
-- uniqueness check: registering the same claim twice within the same platform experiment
-- returns the existing row instead of a fake always-novel stub. The same wording in two
-- different platform experiments is intentionally allowed to register separately — they are
-- different research programs with independent idea pools. See
-- services/registry.RegisterHypothesis and services/dedup (novelty scoring, which is
-- advisory and separate from this hard constraint).
-- ---------------------------------------------------------------------------

CREATE TABLE hypotheses (
    id                     TEXT              PRIMARY KEY DEFAULT gen_random_uuid()::text,
    agent_id               TEXT              NOT NULL REFERENCES agents(id),
    platform_experiment_id TEXT              NOT NULL REFERENCES platform_experiments(id),
    text                   TEXT              NOT NULL,
    normalized_text        TEXT              NOT NULL,
    -- The owning agent's own verdict on this claim — see domain.HypothesisStatus. Every existing
    -- row backfills to 'open' via this column default (this schema has no migration history; a
    -- fresh apply and an in-place ADD COLUMN both get the same backfill from one DEFAULT clause).
    status                 hypothesis_status NOT NULL DEFAULT 'open',
    created_at             TIMESTAMPTZ       NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_hypotheses_platform_normalized_text ON hypotheses(platform_experiment_id, normalized_text);
CREATE INDEX idx_hypotheses_agent    ON hypotheses(agent_id);
CREATE INDEX idx_hypotheses_platform ON hypotheses(platform_experiment_id);

-- ---------------------------------------------------------------------------
-- experiments
-- ---------------------------------------------------------------------------

CREATE TABLE experiments (
    id                       TEXT              PRIMARY KEY,
    parent_id                TEXT              REFERENCES experiments(id),
    agent_id                 TEXT              NOT NULL REFERENCES agents(id),
    project_id               TEXT              NOT NULL,
    cluster_name             TEXT              NOT NULL DEFAULT 'default',
    -- Every job must belong to exactly one platform experiment — there is no such thing as
    -- an unscoped job. Required so quota, the summary gate, and the hypothesis pool below
    -- all have an unambiguous platform experiment to key off.
    platform_experiment_id   TEXT              NOT NULL REFERENCES platform_experiments(id),
    code_ref                 TEXT              NOT NULL,
    config_hash              TEXT              NOT NULL,
    data_ref                 TEXT              NOT NULL,
    job_spec                 JSONB             NOT NULL DEFAULT '{}'::jsonb,
    hypothesis_id            TEXT              NOT NULL REFERENCES hypotheses(id),
    hypothesis               TEXT              NOT NULL,
    objective                TEXT              NOT NULL,
    theory                   TEXT              NOT NULL DEFAULT '',
    novelty_score            DOUBLE PRECISION  NOT NULL DEFAULT 0,
    accelerator_type                 TEXT              NOT NULL,
    accelerator_count                INTEGER           NOT NULL DEFAULT 1,
    capacity_tier            capacity_tier     NOT NULL DEFAULT 'guaranteed',
    status                   experiment_status NOT NULL,
    priority_score           DOUBLE PRECISION  NOT NULL DEFAULT 0,
    estimated_duration_hours DOUBLE PRECISION  NOT NULL,
    estimated_cost_acch       DOUBLE PRECISION  NOT NULL DEFAULT 0,
    estimated_cpu_core_hours    DOUBLE PRECISION NOT NULL DEFAULT 0,
    estimated_ram_gb_hours      DOUBLE PRECISION NOT NULL DEFAULT 0,
    estimated_storage_gb_hours  DOUBLE PRECISION NOT NULL DEFAULT 0,
    artifacts                TEXT[]            NOT NULL DEFAULT '{}',
    queued_at                TIMESTAMPTZ,
    submitted_at             TIMESTAMPTZ,
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
    created_at               TIMESTAMPTZ       NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ       NOT NULL DEFAULT now()
);

-- No migration history: a fresh apply gets this from the column definition above, an existing
-- database from the ALTER, and both land on the same 0 backfill from the one DEFAULT clause.
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS attempt_count INTEGER NOT NULL DEFAULT 0;

ALTER TABLE experiments ADD COLUMN IF NOT EXISTS not_admitted_reason TEXT;
UPDATE experiments
SET not_admitted_reason = CASE
    WHEN status = 'QUEUED' THEN COALESCE(not_admitted_reason, 'capacity_unavailable')
    ELSE NULL
END;
DO $$ BEGIN
    ALTER TABLE experiments ADD CONSTRAINT experiments_queue_reason_consistent
        CHECK ((status = 'QUEUED') = (not_admitted_reason IS NOT NULL));
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

CREATE INDEX idx_experiments_agent_id   ON experiments(agent_id);
CREATE INDEX idx_experiments_status     ON experiments(status);
-- Partial index backing the settlement reconciler's scan for terminal experiments whose final
-- usage hasn't been durably written yet — stays tiny since settled rows drop out of it.
CREATE INDEX idx_experiments_unsettled  ON experiments(updated_at)
    WHERE quota_settled_at IS NULL AND status IN ('COMPLETED', 'FAILED', 'EVICTED', 'REJECTED');
CREATE INDEX idx_experiments_project    ON experiments(project_id);
CREATE INDEX idx_experiments_platform   ON experiments(platform_experiment_id);
CREATE INDEX idx_experiments_hypothesis ON experiments(hypothesis_id);
-- Every cluster-agent polls its desired workload set continuously, and ClaimSubmitted re-reads it
-- while holding the cross-replica admission lock. Partial on the three desired statuses so the
-- index stays proportional to what is in flight rather than to everything ever run — without it
-- both sequential-scan the whole table as it grows, the second one inside the lock.
CREATE INDEX idx_experiments_desired ON experiments(cluster_name, status)
    WHERE status IN ('SUBMITTED', 'ADMITTED', 'RUNNING');
-- Quota and stage sweeps ask per (agent, platform experiment, status) — see
-- GetAgentRunningExperiments/GetAgentQueuedExperiments, called once per agent per reconcile tick.
CREATE INDEX idx_experiments_agent_platform_status ON experiments(agent_id, platform_experiment_id, status);

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

CREATE TABLE hypothesis_findings (
    id             TEXT        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    hypothesis_id  TEXT        NOT NULL REFERENCES hypotheses(id),
    experiment_id  TEXT        NOT NULL REFERENCES experiments(id) UNIQUE,
    agent_id       TEXT        NOT NULL REFERENCES agents(id),
    summary        TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_hypothesis_findings_hypothesis ON hypothesis_findings(hypothesis_id);

-- ---------------------------------------------------------------------------
-- hypothesis_comments — a freeform, job-independent note on a hypothesis (amend, abandon,
-- revise, cross-reference), as opposed to hypothesis_findings which is the measured result of
-- one terminal job. Lets an agent record "abandoning this, ruled out by X" without having to
-- burn a trial first. No idempotency key: an occasional duplicate under crash-restart is
-- low-harm noise, not a correctness bug — see plan.md.
-- ---------------------------------------------------------------------------

CREATE TABLE hypothesis_comments (
    id             TEXT        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    hypothesis_id  TEXT        NOT NULL REFERENCES hypotheses(id),
    agent_id       TEXT        NOT NULL REFERENCES agents(id),
    text           TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_hypothesis_comments_hypothesis ON hypothesis_comments(hypothesis_id);

-- ---------------------------------------------------------------------------
-- donation_requests
-- ---------------------------------------------------------------------------

CREATE TABLE donation_requests (
    id                     TEXT             PRIMARY KEY DEFAULT gen_random_uuid()::text,
    agent_id               TEXT             NOT NULL REFERENCES agents(id),
    platform_experiment_id TEXT             NOT NULL REFERENCES platform_experiments(id),
    credits_want           DOUBLE PRECISION NOT NULL,
    reason                 TEXT             NOT NULL,
    status                 TEXT             NOT NULL DEFAULT 'open',
    -- Free text here meant a typo produced a donation nobody could ever see again: every reader
    -- filters on one of these three values, so an unrecognised one is invisible, not invalid.
    CONSTRAINT donation_requests_status CHECK (status IN ('open', 'fulfilled', 'cancelled')),
    CONSTRAINT donation_requests_credits_positive CHECK (credits_want > 0),
    created_at             TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ      NOT NULL DEFAULT now()
);

CREATE INDEX idx_donation_requests_agent_id ON donation_requests(agent_id);
CREATE INDEX idx_donation_requests_platform ON donation_requests(platform_experiment_id);
CREATE INDEX idx_donation_requests_status   ON donation_requests(status);

-- ---------------------------------------------------------------------------
-- experiment_signups — agents enrolled in a platform experiment
-- ---------------------------------------------------------------------------

CREATE TABLE experiment_signups (
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    agent_id               TEXT        NOT NULL REFERENCES agents(id),
    signed_up_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (platform_experiment_id, agent_id)
);

CREATE INDEX idx_experiment_signups_platform ON experiment_signups(platform_experiment_id);
CREATE INDEX idx_experiment_signups_agent    ON experiment_signups(agent_id);

-- ---------------------------------------------------------------------------
-- agent_quotas — per-agent allocation per platform experiment
-- ---------------------------------------------------------------------------

-- Allocation only. Current desired usage is derived from experiment rows in PostgreSQL;
-- observed terminal consumption lives in the metrics store. No usage total is persisted here.
CREATE TABLE agent_quotas (
    id                     TEXT             PRIMARY KEY,
    agent_id               TEXT             NOT NULL REFERENCES agents(id),
    platform_experiment_id TEXT             NOT NULL REFERENCES platform_experiments(id),
    guaranteed_accelerator_hours    DOUBLE PRECISION NOT NULL,
    burst_accelerator_hours         DOUBLE PRECISION NOT NULL,
    guaranteed_cpu_core_hours    DOUBLE PRECISION NOT NULL DEFAULT 0,
    burst_cpu_core_hours         DOUBLE PRECISION NOT NULL DEFAULT 0,
    guaranteed_ram_gb_hours      DOUBLE PRECISION NOT NULL DEFAULT 0,
    burst_ram_gb_hours           DOUBLE PRECISION NOT NULL DEFAULT 0,
    guaranteed_storage_gb_hours  DOUBLE PRECISION NOT NULL DEFAULT 0,
    burst_storage_gb_hours       DOUBLE PRECISION NOT NULL DEFAULT 0,
    -- An allocation is a quantity of hours; a negative one is a bug, not a state. Without this,
    -- a stale-snapshot donation or a negative stage delta silently produced one, and the agent
    -- got rejections explaining it had "-3.2 hours remaining".
    CONSTRAINT agent_quotas_non_negative CHECK (
        guaranteed_accelerator_hours >= 0 AND burst_accelerator_hours >= 0 AND
        guaranteed_cpu_core_hours    >= 0 AND burst_cpu_core_hours    >= 0 AND
        guaranteed_ram_gb_hours      >= 0 AND burst_ram_gb_hours      >= 0 AND
        guaranteed_storage_gb_hours  >= 0 AND burst_storage_gb_hours  >= 0
    ),
    created_at             TIMESTAMPTZ      NOT NULL DEFAULT now(),
    UNIQUE (agent_id, platform_experiment_id)
);

CREATE INDEX idx_agent_quotas_platform ON agent_quotas(platform_experiment_id);
CREATE INDEX idx_agent_quotas_agent    ON agent_quotas(agent_id);

-- ---------------------------------------------------------------------------
-- experiment_top3 — top-3 agent placements per platform experiment
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
-- platform_experiment_cuts — agents cut at a stage boundary. Terminal: jobs stopped and
-- further submissions rejected 422 for the rest of the experiment.
-- ---------------------------------------------------------------------------

CREATE TABLE platform_experiment_cuts (
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    agent_id               TEXT        NOT NULL REFERENCES agents(id),
    stage_index            INTEGER     NOT NULL,
    cut_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (platform_experiment_id, agent_id)
);

CREATE INDEX idx_pe_cuts_platform ON platform_experiment_cuts(platform_experiment_id);

-- ---------------------------------------------------------------------------
-- platform_experiment_stage_advances — one row per boundary crossed, committed with that
-- boundary's cuts and quota moves (see AdvanceStage). Quota moves would double-apply on a naive
-- retry; this row is what makes a crash mid-advance resume rather than re-run.
-- ---------------------------------------------------------------------------

CREATE TABLE platform_experiment_stage_advances (
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    stage_index            INTEGER     NOT NULL,
    advanced_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (platform_experiment_id, stage_index)
);

COMMIT;
