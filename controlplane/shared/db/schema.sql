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

CREATE TYPE experiment_status AS ENUM (
    'DRAFT',
    'SUBMITTED',
    'QUEUED',
    'ADMITTED',
    'RUNNING',
    'COMPLETED',
    'FAILED',
    'EVICTED',
    'PROMOTED',
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
    phase                INTEGER                    NOT NULL DEFAULT 1,
    phase2_triggered_at  TIMESTAMPTZ,
    -- Set exactly once, inside the same transaction as phase-2's one-time quota redistribution
    -- (zero held agents, add their remaining budget to active agents) — see
    -- db.PlatformExperimentsStore.RedistributePhase2Quota. Job-stopping for held agents is
    -- naturally idempotent and safe to retry every reconcile tick, but redistribution moves
    -- quota between agents and would double-add on a naive retry — this column (committed
    -- atomically with the redistribution writes) is what lets a crash between TriggerPhase2 and
    -- full application resume without redoing (and corrupting) that step.
    phase2_redistributed_at TIMESTAMPTZ,
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
    id                     TEXT        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    agent_id               TEXT        NOT NULL REFERENCES agents(id),
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    text                   TEXT        NOT NULL,
    normalized_text        TEXT        NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
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
    status                   experiment_status NOT NULL DEFAULT 'DRAFT',
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
    created_at               TIMESTAMPTZ       NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ       NOT NULL DEFAULT now()
);

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
-- credit_ledger
-- ---------------------------------------------------------------------------

CREATE TABLE credit_ledger (
    id                     TEXT             PRIMARY KEY,
    agent_id               TEXT             NOT NULL REFERENCES agents(id),
    amount                 DOUBLE PRECISION NOT NULL,
    reason                 TEXT             NOT NULL,   -- issuance | spend | refund | borrow
    experiment_id          TEXT             REFERENCES experiments(id),
    platform_experiment_id TEXT             REFERENCES platform_experiments(id),
    period                 INTEGER          NOT NULL,
    created_at             TIMESTAMPTZ      NOT NULL DEFAULT now()
);

CREATE INDEX idx_credit_ledger_agent_id ON credit_ledger(agent_id);
CREATE INDEX idx_credit_ledger_period   ON credit_ledger(period);

-- ---------------------------------------------------------------------------
-- donation_requests
-- ---------------------------------------------------------------------------

CREATE TABLE donation_requests (
    id                     TEXT             PRIMARY KEY DEFAULT gen_random_uuid()::text,
    agent_id               TEXT             NOT NULL REFERENCES agents(id),
    platform_experiment_id TEXT             NOT NULL REFERENCES platform_experiments(id),
    credits_want           DOUBLE PRECISION NOT NULL,
    reason                 TEXT             NOT NULL,
    status                 TEXT             NOT NULL DEFAULT 'open',  -- open | fulfilled | cancelled
    created_at             TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ      NOT NULL DEFAULT now()
);

CREATE INDEX idx_donation_requests_agent_id ON donation_requests(agent_id);
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
-- experiment_phase2_holds — agents held for Phase 2 admission
-- ---------------------------------------------------------------------------

CREATE TABLE experiment_phase2_holds (
    platform_experiment_id TEXT        NOT NULL REFERENCES platform_experiments(id),
    agent_id               TEXT        NOT NULL REFERENCES agents(id),
    held_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (platform_experiment_id, agent_id)
);

CREATE INDEX idx_phase2_holds_platform ON experiment_phase2_holds(platform_experiment_id);

COMMIT;
