-- Postgres holds no logic. Every change GET /watch delivers is a row of changes, appended by the
-- store in the transaction of the write it announces, and every column a write sets is set by it.
-- The triggers go first: what follows rewrites rows they would fire on.

DROP TRIGGER experiments_touch ON experiments;
DROP TRIGGER experiments_notify ON experiments;
DROP TRIGGER agent_quotas_touch ON agent_quotas;
DROP TRIGGER agent_quotas_notify ON agent_quotas;
DROP TRIGGER platform_experiments_touch ON platform_experiments;
DROP TRIGGER platform_experiments_notify ON platform_experiments;
DROP TRIGGER hypothesis_findings_notify ON hypothesis_findings;
DROP TRIGGER hypothesis_comments_notify ON hypothesis_comments;
DROP TRIGGER stage_advances_notify ON platform_experiment_stage_advances;
DROP TRIGGER pe_cuts_notify ON platform_experiment_cuts;
DROP TRIGGER signup_finished_notify ON experiment_signups;
DROP TRIGGER signup_replaced_notify ON experiment_signups;
DROP TRIGGER signup_idle_notify ON experiment_signups;
DROP TRIGGER platform_experiments_capacity_idle_notify ON platform_experiments;
DROP TRIGGER planning_requests_notify ON planning_requests;
DROP TRIGGER baseline_raises_notify ON baseline_raises;
DROP TRIGGER agent_messages_notify ON agent_messages;
DROP TRIGGER environments_ended_notify ON environments;
DROP TRIGGER environments_loaded_notify ON environments;
DROP TRIGGER agent_type_versions_immutable ON agent_type_versions;
DROP TRIGGER platform_rules_append_only ON platform_rules;
DROP TRIGGER agent_type_events_append_only ON agent_type_events;
DROP TRIGGER signup_instructions_notify ON experiment_signups;
DROP TRIGGER platform_experiments_coordinator_instructions_notify ON platform_experiments;

DROP FUNCTION hypothesisloop_experiments_touch();
DROP FUNCTION hypothesisloop_experiments_notify();
DROP FUNCTION hypothesisloop_agent_quotas_touch();
DROP FUNCTION hypothesisloop_agent_quotas_notify();
DROP FUNCTION hypothesisloop_platform_experiments_touch();
DROP FUNCTION hypothesisloop_platform_experiments_notify();
DROP FUNCTION hypothesisloop_findings_notify();
DROP FUNCTION hypothesisloop_comments_notify();
DROP FUNCTION hypothesisloop_stage_advance_notify();
DROP FUNCTION hypothesisloop_cuts_notify();
DROP FUNCTION hypothesisloop_agent_finished_notify();
DROP FUNCTION hypothesisloop_agent_replaced_notify();
DROP FUNCTION hypothesisloop_agent_idle_notify();
DROP FUNCTION hypothesisloop_capacity_idle_notify();
DROP FUNCTION hypothesisloop_planning_request_notify();
DROP FUNCTION hypothesisloop_baseline_raise_notify();
DROP FUNCTION hypothesisloop_agent_message_notify();
DROP FUNCTION hypothesisloop_environment_ended_notify();
DROP FUNCTION hypothesisloop_environment_loaded_notify();
DROP FUNCTION hypothesisloop_agent_type_versions_immutable();
DROP FUNCTION hypothesisloop_append_only();
DROP FUNCTION hypothesisloop_agent_instructions_notify();
DROP FUNCTION hypothesisloop_coordinator_instructions_notify();
DROP FUNCTION hypothesisloop_notify_event(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT);

ALTER TABLE experiments ADD COLUMN ended_at TIMESTAMPTZ;
UPDATE experiments e
SET ended_at = COALESCE((SELECT t.occurred_at FROM event_times t WHERE t.xid = e.status_xid), e.updated_at)
WHERE e.status IN ('COMPLETED', 'FAILED', 'EVICTED', 'REJECTED', 'CANCELLED');

ALTER TABLE environment_waiters ADD COLUMN loaded BOOLEAN NOT NULL DEFAULT false;
UPDATE environment_waiters SET loaded = loaded_xid IS NOT NULL;
DELETE FROM environment_waiters WHERE ended_xid IS NOT NULL;

DROP TABLE event_times;

ALTER TABLE platform_experiments
    DROP COLUMN coordinator_instructions_xid,
    DROP COLUMN coordinator_reason_xid,
    DROP COLUMN researchers_held_xid,
    DROP COLUMN capacity_xid,
    DROP COLUMN status_changed_xid,
    DROP COLUMN description_changed_xid;
ALTER TABLE planning_requests
    DROP COLUMN created_xid;
ALTER TABLE experiment_signups
    DROP COLUMN instructions_xid,
    DROP COLUMN replaces_xid,
    DROP COLUMN finished_xid,
    DROP COLUMN idle_xid;
ALTER TABLE agent_quotas
    DROP COLUMN updated_xid;
ALTER TABLE platform_experiment_cuts
    DROP COLUMN cut_xid;
ALTER TABLE platform_experiment_stage_advances
    DROP COLUMN advanced_xid;
ALTER TABLE agent_messages
    DROP COLUMN created_xid;
ALTER TABLE environment_waiters
    DROP COLUMN state,
    DROP COLUMN detail,
    DROP COLUMN ended_xid,
    DROP COLUMN loaded_xid,
    DROP COLUMN loaded_detail;
ALTER TABLE experiments
    DROP COLUMN updated_xid,
    DROP COLUMN status_xid,
    DROP COLUMN not_admitted_xid,
    DROP COLUMN queued_xid,
    DROP COLUMN submitted_xid,
    DROP COLUMN underperforming_xid,
    DROP COLUMN held_xid,
    DROP COLUMN same_hypothesis_xid;
ALTER TABLE baseline_raises
    DROP COLUMN created_xid;
ALTER TABLE hypothesis_findings
    DROP COLUMN created_xid;
ALTER TABLE hypothesis_comments
    DROP COLUMN created_xid;

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
        'platform_experiment.status', 'platform_experiment.description', 'platform_experiment.revision',
        'platform_experiment.baseline', 'platform_experiment.removed',
        'job.underperforming', 'job.held', 'job.duplicate', 'environment.status'
    ))
);
CREATE INDEX changes_by_experiment ON changes (platform_experiment_id, id);
CREATE INDEX changes_occurred ON changes (occurred_at);
