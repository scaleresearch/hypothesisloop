-- OpenResearch Autonomous Research Platform — canonical schema
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

-- gpu_type is deliberately plain TEXT, not an ENUM: the GPU catalog is entirely
-- operator-defined via openresearch.yaml's gpu_types (see config.GPUTypeConfig) and any
-- vendor's model name is valid (NVIDIA H100, AMD MI300X, ...) — a closed Postgres enum here
-- would silently reject any GPU type the operator adds without a schema change, defeating the
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
    budget_t4_hours      DOUBLE PRECISION           NOT NULL,
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
    created_at           TIMESTAMPTZ                NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ                NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- hypotheses — the research-claim registry. Agents register (or retrieve, if an
-- equivalent one already exists) a hypothesis here before submitting an experiment that
-- tests it. normalized_text (lowercased, whitespace-collapsed) carries a UNIQUE index —
-- this is the real uniqueness check: registering the same claim twice returns the existing
-- row instead of a fake always-novel stub. See services/registry.RegisterHypothesis and
-- services/dedup (novelty scoring, which is advisory and separate from this hard constraint).
-- ---------------------------------------------------------------------------

CREATE TABLE hypotheses (
    id              TEXT        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    agent_id        TEXT        NOT NULL REFERENCES agents(id),
    text            TEXT        NOT NULL,
    normalized_text TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_hypotheses_normalized_text ON hypotheses(normalized_text);
CREATE INDEX idx_hypotheses_agent ON hypotheses(agent_id);

-- ---------------------------------------------------------------------------
-- experiments
-- ---------------------------------------------------------------------------

CREATE TABLE experiments (
    id                       TEXT              PRIMARY KEY,
    parent_id                TEXT              REFERENCES experiments(id),
    agent_id                 TEXT              NOT NULL REFERENCES agents(id),
    project_id               TEXT              NOT NULL,
    cluster_name             TEXT              NOT NULL DEFAULT 'default',
    platform_experiment_id   TEXT              REFERENCES platform_experiments(id),
    code_ref                 TEXT              NOT NULL,
    config_hash              TEXT              NOT NULL,
    data_ref                 TEXT              NOT NULL,
    job_spec                 JSONB             NOT NULL DEFAULT '{}'::jsonb,
    hypothesis_id            TEXT              NOT NULL REFERENCES hypotheses(id),
    hypothesis               TEXT              NOT NULL,
    objective                TEXT              NOT NULL,
    theory                   TEXT              NOT NULL DEFAULT '',
    summary                  TEXT              NOT NULL DEFAULT '',
    novelty_score            DOUBLE PRECISION  NOT NULL DEFAULT 0,
    gpu_type                 TEXT              NOT NULL,
    gpu_count                INTEGER           NOT NULL DEFAULT 1,
    capacity_tier            capacity_tier     NOT NULL DEFAULT 'guaranteed',
    status                   experiment_status NOT NULL DEFAULT 'DRAFT',
    priority_score           DOUBLE PRECISION  NOT NULL DEFAULT 0,
    estimated_duration_hours DOUBLE PRECISION  NOT NULL,
    estimated_cost_t4h       DOUBLE PRECISION  NOT NULL DEFAULT 0,
    estimated_cpu_core_hours    DOUBLE PRECISION NOT NULL DEFAULT 0,
    estimated_ram_gb_hours      DOUBLE PRECISION NOT NULL DEFAULT 0,
    estimated_storage_gb_hours  DOUBLE PRECISION NOT NULL DEFAULT 0,
    actual_duration_hours    DOUBLE PRECISION,
    actual_cost_t4h          DOUBLE PRECISION,
    actual_cpu_core_hours       DOUBLE PRECISION,
    actual_ram_gb_hours         DOUBLE PRECISION,
    actual_storage_gb_hours     DOUBLE PRECISION,
    artifacts                TEXT[]            NOT NULL DEFAULT '{}',
    queued_at                TIMESTAMPTZ,
    submitted_at             TIMESTAMPTZ,
    started_at               TIMESTAMPTZ,
    preempt_count            INTEGER           NOT NULL DEFAULT 0,
    eviction_reason          TEXT,
    -- not_admitted_reason: why a QUEUED job wasn't admitted on its most recent skipped tick
    -- (capacity_unavailable | outranked | summary_gate) — updated every tick, cleared on
    -- admission. Pre-admission counterpart to eviction_reason: same "flag why, don't make the
    -- agent infer it" pattern, for the QUEUED side instead of the post-RUNNING side.
    not_admitted_reason      TEXT,
    created_at               TIMESTAMPTZ       NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ       NOT NULL DEFAULT now()
);

CREATE INDEX idx_experiments_agent_id   ON experiments(agent_id);
CREATE INDEX idx_experiments_status     ON experiments(status);
CREATE INDEX idx_experiments_project    ON experiments(project_id);
CREATE INDEX idx_experiments_platform   ON experiments(platform_experiment_id);
CREATE INDEX idx_experiments_hypothesis ON experiments(hypothesis_id);

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

-- Allocation only (the operator-set capacity setting). Consumption (how much of it has been
-- used) is never stored here — it lives solely in the metrics DB (GreptimeDB), updated on every
-- debit/refund and read live wherever "available quota" needs to be computed. See
-- controlplane/shared/metricsdb.UsageTracker.
CREATE TABLE agent_quotas (
    id                     TEXT             PRIMARY KEY,
    agent_id               TEXT             NOT NULL REFERENCES agents(id),
    platform_experiment_id TEXT             NOT NULL REFERENCES platform_experiments(id),
    guaranteed_t4_hours    DOUBLE PRECISION NOT NULL,
    burst_t4_hours         DOUBLE PRECISION NOT NULL,
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

-- ---------------------------------------------------------------------------
-- cluster_job_reports — latest observed Job phase per experiment, pushed by
-- cluster-agents (never read live from a cluster by the control plane). There
-- is no separate command outbox: a cluster-agent derives what should exist
-- directly from experiments.status (SUBMITTED/ADMITTED/RUNNING = should have a
-- Job) and reconciles its local Jobs to match, the same way a kubelet
-- reconciles pods to a desired spec. sequence_number guards against
-- out-of-order/duplicate delivery: a report is only applied if its
-- sequence_number is greater than the stored one.
-- ---------------------------------------------------------------------------

-- job_uid: the k8s Job's UID, as first reported by cluster-agent. Ownership verification: once
-- set, a later report for the same experiment_id carrying a *different* job_uid indicates the
-- report doesn't correspond to the Job this control plane actually dispatched (name collision,
-- stray manually-created Job, a second cluster-agent misconfigured against the same cluster) —
-- flagged, not silently trusted. See UpsertJobReport.
CREATE TABLE cluster_job_reports (
    experiment_id    TEXT        PRIMARY KEY REFERENCES experiments(id),
    cluster_name     TEXT        NOT NULL,
    phase            TEXT        NOT NULL,
    admitted_gpu_type TEXT,
    job_uid          TEXT,
    sequence_number  BIGINT      NOT NULL DEFAULT 0,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_cluster_job_reports_cluster ON cluster_job_reports(cluster_name);

-- ---------------------------------------------------------------------------
-- cluster_heartbeats — last time each cluster-agent was seen, so the control
-- plane (and its UI) can show which registered clusters are actually connected
-- right now. Updated on every desired-state poll (cluster-agent calls this
-- every ~2s), so a cluster is "connected" iff last_seen_at is recent.
-- ---------------------------------------------------------------------------

-- cpu_available_cores/cpu_total_cores: live CPU capacity self-reported by cluster-agent on
-- every desired-state poll (allocatable minus actually-requested, computed against this
-- cluster's real node/pod state) — replaces the old static-config-only capacity model for
-- CPU-only jobs. See controlplane/services/scheduler/loop.go's admissionUnit.
CREATE TABLE cluster_heartbeats (
    cluster_name        TEXT             PRIMARY KEY,
    last_seen_at        TIMESTAMPTZ      NOT NULL DEFAULT now(),
    cpu_available_cores DOUBLE PRECISION,
    cpu_total_cores     DOUBLE PRECISION
);

COMMIT;
