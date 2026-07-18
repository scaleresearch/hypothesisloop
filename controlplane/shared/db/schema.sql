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
    -- attempt: bumped by the control plane each time a cluster-agent reports a Job gone
    -- while this experiment is still in a desired-running status (SUBMITTED/ADMITTED/
    -- RUNNING) — i.e. the Job disappeared without ever reaching a terminal phase, so
    -- whatever the cluster-agent creates next is a distinct execution attempt, not the
    -- original one. Carried in desired-state responses and stamped onto the recreated
    -- Job as a label so each attempt is individually identifiable.
    attempt                  INTEGER           NOT NULL DEFAULT 1,
    eviction_reason          TEXT,
    -- not_admitted_reason: why a QUEUED job wasn't admitted on its most recent skipped tick
    -- (capacity_unavailable | outranked | summary_gate) — updated every tick, cleared on
    -- admission. Pre-admission counterpart to eviction_reason: same "flag why, don't make the
    -- agent infer it" pattern, for the QUEUED side instead of the post-RUNNING side.
    not_admitted_reason      TEXT,
    -- quota_settled_at: set once this terminal experiment's final observed usage has been
    -- durably written to the metrics DB. NULL means settlement is outstanding (never attempted,
    -- or attempted and failed) — the durable signal a background reconciler scans for to retry
    -- writing it, surviving any crash/restart between the status transition and that write.
    quota_settled_at         TIMESTAMPTZ,
    created_at               TIMESTAMPTZ       NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ       NOT NULL DEFAULT now()
);

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

-- ---------------------------------------------------------------------------
-- pending_capacity_reservations — durable (Postgres, not in-process) claim on physical
-- capacity for an experiment between the moment tick()/the operator admit endpoint marks it
-- SUBMITTED and the moment its pod is actually confirmed to exist (job_watcher observes it
-- RUNNING). See SCHEDULING_GENERALIZATION_PLAN.md's "Durable pending-capacity reservations"
-- cross-cutting fix: cluster-agents' live CPU/GPU capacity numbers (cluster_heartbeats,
-- metricsdb) reflect real k8s state, so they do NOT yet include a job's request in the window
-- between MarkSubmitted and the cluster-agent actually creating the pod — a second scheduler
-- tick in that window could otherwise double-admit into the same capacity. tick() sums these
-- rows per cluster and subtracts them from live capacity on top of the existing
-- SUBMITTED/ADMITTED/RUNNING accounting, closing that gap without duplicating the resource
-- estimate anywhere else (experiments.job_spec / estimated_* columns remain the sole billing
-- source of truth; this table exists purely for the physical-fit race).
--
-- One row per experiment, upserted at admission (loop_preempt.go's submitJob) and deleted once
-- resolved: onRunning (pod confirmed to exist — capacity now already reflected live),
-- onStuckPending/onFinished (evicted/completed without a pod ever needing to be counted), or a
-- rolled-back submission (workload creation failed, job returned to QUEUED).
--
-- Tracks every dimension Experiment.Footprint() reports: CPU, one accelerator flavor, and (once
-- Class B step 2's capacity piggyback landed) RAM/ephemeral-storage in bytes.
CREATE TABLE pending_capacity_reservations (
    experiment_id       TEXT        PRIMARY KEY REFERENCES experiments(id) ON DELETE CASCADE,
    cluster_name        TEXT        NOT NULL,
    cpu_millicores       BIGINT      NOT NULL DEFAULT 0,
    accelerator_flavor   TEXT        NOT NULL DEFAULT '',
    accelerator_count    BIGINT      NOT NULL DEFAULT 0,
    ram_bytes            BIGINT      NOT NULL DEFAULT 0,
    storage_bytes        BIGINT      NOT NULL DEFAULT 0,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_pending_capacity_reservations_cluster ON pending_capacity_reservations(cluster_name);

COMMIT;
