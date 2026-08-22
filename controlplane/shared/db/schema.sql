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
    -- The owner column, and the only one anything keys ownership on: a job, quota and the
    -- standings all read it. NULL on a human-submitted row, which is why such a row can own
    -- none of those. Exactly one of agent_id/author is set (domain.ClassifyHypothesisOrigin).
    agent_id               TEXT              REFERENCES agents(id),
    -- domain.HypothesisSource. Existing rows backfill to 'agent' via this column default.
    source                 TEXT              NOT NULL DEFAULT 'agent',
    -- The name a human typed in the UI. There is no auth: it is a claim, not an identity,
    -- exactly as agent_id is. Empty on an agent row.
    author                 TEXT              NOT NULL DEFAULT '',
    platform_experiment_id TEXT              NOT NULL REFERENCES platform_experiments(id),
    text                   TEXT              NOT NULL,
    normalized_text        TEXT              NOT NULL,
    -- The owning agent's own verdict on this claim — see domain.HypothesisStatus. Every existing
    -- row backfills to 'open' via this column default (this schema has no migration history; a
    -- fresh apply and an in-place ADD COLUMN both get the same backfill from one DEFAULT clause).
    status                 hypothesis_status NOT NULL DEFAULT 'open',
    created_at             TIMESTAMPTZ       NOT NULL DEFAULT now()
);

-- No migration history: a fresh apply gets these from the column definitions above, an existing
-- database from the ALTERs, and both land on the same 'agent'/'' backfill from the one DEFAULT
-- clause each. Dropping NOT NULL on agent_id is what lets a human row exist at all, and is a
-- no-op on a database that already has it dropped.
ALTER TABLE hypotheses ALTER COLUMN agent_id DROP NOT NULL;
ALTER TABLE hypotheses ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'agent';
ALTER TABLE hypotheses ADD COLUMN IF NOT EXISTS author TEXT NOT NULL DEFAULT '';

-- The dedup index is deliberately not scoped by source: a human idea and an agent's restatement
-- of the same claim are the same claim, and collide exactly as two agent rows would.
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
    -- infra_requeue_count: how many of attempt_count's attempts were ended by the environment
    -- (an infrastructure-class eviction reason) and requeued for free. The agent's max_retries
    -- allowance is attempt_count - infra_requeue_count, so a job that keeps landing on broken
    -- hardware never spends the budget meant for its own bugs — while attempt_count still
    -- advances, which is what makes each requeue a distinct desired state the runtime rebuilds.
    infra_requeue_count      INTEGER           NOT NULL DEFAULT 0,
    created_at               TIMESTAMPTZ       NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ       NOT NULL DEFAULT now()
);

-- No migration history: a fresh apply gets this from the column definition above, an existing
-- database from the ALTER, and both land on the same 0 backfill from the one DEFAULT clause.
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS attempt_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS infra_requeue_count INTEGER NOT NULL DEFAULT 0;

-- artifacts held a file list no code ever wrote and no code ever read. Jobs may push metrics and
-- nothing else, so a job could never report its own files; the real bytes live in the object
-- store and GET /experiments/{id}/data lists them there. A copy here would only drift.
ALTER TABLE experiments DROP COLUMN IF EXISTS artifacts;

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
    -- NULL on a human comment; author carries the typed name instead. Exactly one of the two is
    -- set, on the same rule as hypotheses (domain.ClassifyHypothesisOrigin).
    agent_id       TEXT        REFERENCES agents(id),
    source         TEXT        NOT NULL DEFAULT 'agent',
    author         TEXT        NOT NULL DEFAULT '',
    text           TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Same no-migration-history pattern as hypotheses above: one DEFAULT clause backfills existing
-- rows to 'agent'/'' whether the table is created fresh or altered in place.
ALTER TABLE hypothesis_comments ALTER COLUMN agent_id DROP NOT NULL;
ALTER TABLE hypothesis_comments ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'agent';
ALTER TABLE hypothesis_comments ADD COLUMN IF NOT EXISTS author TEXT NOT NULL DEFAULT '';

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
    -- domain.SignupRole. Fixed at signup and never updated: a role change mid-run would
    -- retroactively rewrite who a completed cut applied to. Only ranking reads it.
    role                   TEXT        NOT NULL DEFAULT 'competitor',
    PRIMARY KEY (platform_experiment_id, agent_id)
);

ALTER TABLE experiment_signups ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'competitor';

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

-- updated_at exists so a reconnecting watcher can ask "which allocations changed since my
-- cursor" — an allocation is rewritten in place by stage moves and donations, so without it the
-- row carries no evidence that it ever moved. Maintained by a trigger below, not by any writer:
-- there is one clock for this column and no write path can forget it.
ALTER TABLE agent_quotas ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

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

-- ---------------------------------------------------------------------------
-- Change notification — the source of GET /watch's live event stream
-- ---------------------------------------------------------------------------
--
-- Every event is emitted by an AFTER trigger on the row that changed, so the NOTIFY is part of
-- the very transaction that wrote it: a rolled-back write emits nothing, and no event can ever
-- describe a state the database did not commit. That is also why this is a trigger rather than a
-- pg_notify() call added to each Go write path — a write path can be added tomorrow that forgets
-- to notify, a trigger cannot be bypassed.
--
-- The payload is a small typed record — kind, subject, new value, a qualifying detail, the two
-- scoping ids, and a cursor — never a copy of the row. A client wanting detail follows with a
-- normal GET, so no read path is duplicated here and nothing is persisted twice.
--
-- The cursor is the changed row's own timestamp in microseconds. Replay (see db.EventsStore)
-- re-derives events from those same timestamp columns, so a live event and its replayed twin
-- carry the identical cursor and a reconnecting client can pick up exactly where it stopped.

CREATE OR REPLACE FUNCTION hypothesisloop_notify_event(
    kind TEXT, subject TEXT, new_value TEXT, detail TEXT,
    pe_id TEXT, owner_agent_id TEXT, at TIMESTAMPTZ
) RETURNS void AS $$
BEGIN
    PERFORM pg_notify('hypothesisloop_events', json_build_object(
        'kind', kind,
        'subject', subject,
        'value', new_value,
        'detail', COALESCE(detail, ''),
        'platform_experiment_id', pe_id,
        'agent_id', COALESCE(owner_agent_id, ''),
        'cursor', (EXTRACT(EPOCH FROM at) * 1000000)::bigint
    )::text);
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION hypothesisloop_experiments_notify() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' OR NEW.status IS DISTINCT FROM OLD.status THEN
        PERFORM hypothesisloop_notify_event('experiment.status', NEW.id, NEW.status::text,
            COALESCE(NEW.eviction_reason, NEW.not_admitted_reason, ''),
            NEW.platform_experiment_id, NEW.agent_id, NEW.updated_at);
    END IF;
    -- The queue reason is what an agent polls hardest for: it changes while the status does not,
    -- so it needs an event of its own or a waiting agent learns nothing until admission.
    IF NEW.not_admitted_reason IS NOT NULL
       AND (TG_OP = 'INSERT' OR NEW.not_admitted_reason IS DISTINCT FROM OLD.not_admitted_reason) THEN
        PERFORM hypothesisloop_notify_event('experiment.blocked', NEW.id, NEW.not_admitted_reason, '',
            NEW.platform_experiment_id, NEW.agent_id, NEW.updated_at);
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
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS agent_quotas_touch ON agent_quotas;
CREATE TRIGGER agent_quotas_touch BEFORE UPDATE ON agent_quotas
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_agent_quotas_touch();

CREATE OR REPLACE FUNCTION hypothesisloop_agent_quotas_notify() RETURNS trigger AS $$
BEGIN
    -- The value is the agent's total allocated accelerator hours: enough for a waiting agent to
    -- see a grant, a donation or a stage move land. What it is made of comes from GET /quota.
    PERFORM hypothesisloop_notify_event('quota.changed', NEW.agent_id,
        (NEW.guaranteed_accelerator_hours + NEW.burst_accelerator_hours)::text, '',
        NEW.platform_experiment_id, NEW.agent_id, NEW.updated_at);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS agent_quotas_notify ON agent_quotas;
CREATE TRIGGER agent_quotas_notify AFTER INSERT OR UPDATE ON agent_quotas
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_agent_quotas_notify();

CREATE OR REPLACE FUNCTION hypothesisloop_hypotheses_notify() RETURNS trigger AS $$
BEGIN
    -- The value is where the idea came from, agent or human. The text itself is not here: an
    -- event says what changed and never carries a copy of the row, so a reader that wants the
    -- claim fetches GET /hypotheses/{id} exactly as it would have anyway.
    PERFORM hypothesisloop_notify_event('hypothesis.new', NEW.id, NEW.source, '',
        NEW.platform_experiment_id, COALESCE(NEW.agent_id, ''), NEW.created_at);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS hypotheses_notify ON hypotheses;
CREATE TRIGGER hypotheses_notify AFTER INSERT ON hypotheses
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_hypotheses_notify();

-- Findings and comments both name the hypothesis they hang off as their subject, not their own
-- id: the hypothesis is what a reader would GET, and it is what the pool is organised around.
CREATE OR REPLACE FUNCTION hypothesisloop_findings_notify() RETURNS trigger AS $$
DECLARE pe_id TEXT;
BEGIN
    SELECT platform_experiment_id INTO pe_id FROM hypotheses WHERE id = NEW.hypothesis_id;
    PERFORM hypothesisloop_notify_event('finding.new', NEW.hypothesis_id, NEW.experiment_id, '',
        pe_id, NEW.agent_id, NEW.created_at);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS hypothesis_findings_notify ON hypothesis_findings;
CREATE TRIGGER hypothesis_findings_notify AFTER INSERT ON hypothesis_findings
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_findings_notify();

CREATE OR REPLACE FUNCTION hypothesisloop_comments_notify() RETURNS trigger AS $$
DECLARE pe_id TEXT;
BEGIN
    SELECT platform_experiment_id INTO pe_id FROM hypotheses WHERE id = NEW.hypothesis_id;
    PERFORM hypothesisloop_notify_event('comment.new', NEW.hypothesis_id, NEW.source, '',
        pe_id, COALESCE(NEW.agent_id, ''), NEW.created_at);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS hypothesis_comments_notify ON hypothesis_comments;
CREATE TRIGGER hypothesis_comments_notify AFTER INSERT ON hypothesis_comments
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_comments_notify();

CREATE OR REPLACE FUNCTION hypothesisloop_stage_advance_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('stage.boundary', NEW.platform_experiment_id,
        NEW.stage_index::text, 'advanced', NEW.platform_experiment_id, '', NEW.advanced_at);
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
        NEW.stage_index::text, 'cut', NEW.platform_experiment_id, NEW.agent_id, NEW.cut_at);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS pe_cuts_notify ON platform_experiment_cuts;
CREATE TRIGGER pe_cuts_notify AFTER INSERT ON platform_experiment_cuts
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_cuts_notify();

COMMIT;
