-- Postgres holds no logic. Every change GET /watch delivers is a row of changes, appended by the
-- store in the transaction of the write it announces.

ALTER TABLE experiments ADD COLUMN ended_at TIMESTAMPTZ;
UPDATE experiments e
SET ended_at = COALESCE((SELECT t.occurred_at FROM event_times t WHERE t.xid = e.status_xid), e.updated_at)
WHERE e.status IN ('COMPLETED', 'FAILED', 'EVICTED', 'REJECTED');

ALTER TABLE environment_waiters ADD COLUMN loaded BOOLEAN NOT NULL DEFAULT false;
UPDATE environment_waiters SET loaded = loaded_xid IS NOT NULL;
DELETE FROM environment_waiters WHERE ended_xid IS NOT NULL;

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
