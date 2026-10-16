-- Indexes for the reads every reconcile and scheduler pass makes, a launcher's version kept with
-- its last check-in, and the drop of timestamps nothing reads.

-- A cluster's reconcile reads its recent terminations; the scheduler's speculation reads the
-- clusters recently tried; the sweeps read the platform experiments still in progress.
CREATE INDEX idx_experiments_recently_undesired ON experiments(last_cluster, updated_at)
    WHERE eviction_reason IS NOT NULL;
CREATE INDEX idx_experiments_tried_clusters ON experiments(updated_at)
    WHERE tried_clusters <> '[]'::jsonb;
CREATE INDEX idx_platform_experiments_in_progress ON platform_experiments(status)
    WHERE status IN ('draft', 'open', 'running');

ALTER TABLE launchers ADD COLUMN agent_version TEXT NOT NULL DEFAULT '';

CREATE OR REPLACE FUNCTION hypothesisloop_platform_experiments_touch() RETURNS trigger AS $$
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

ALTER TABLE platform_experiments
    DROP COLUMN status_changed_at,
    DROP COLUMN description_changed_at;
ALTER TABLE org_disabled_clusters DROP COLUMN disabled_at;
