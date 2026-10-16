-- A person removes a platform experiment: it is answered to nobody from then on, and every row it
-- holds stays, for what it spent and for the platform's own loops. Removed implies closed: a
-- removal closes one still going in the same write.
ALTER TABLE platform_experiments
    ADD COLUMN removed_at      TIMESTAMPTZ,
    ADD COLUMN removed_by_kind TEXT NOT NULL DEFAULT '',
    ADD COLUMN removed_by_id   TEXT NOT NULL DEFAULT '',
    ADD CONSTRAINT platform_experiments_removed_by_kind
        CHECK (removed_by_kind IN ('', 'user', 'key', 'agent', 'coordinator', 'staff', 'unverified')),
    ADD CONSTRAINT platform_experiments_removed_closed CHECK (removed_at IS NULL OR status = 'closed');

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
    -- The platform keeps places of the researchers empty, or no longer does.
    IF NEW.researchers_held IS NOT NULL AND NEW.researchers_held IS DISTINCT FROM OLD.researchers_held THEN
        PERFORM hypothesisloop_notify_event('platform_experiment.status', NEW.id, NEW.status::text,
            NEW.researchers_held, NEW.id, '');
    END IF;
    -- The brief is the question every agent is working on, and it is edited mid-run when the
    -- coordinator resolves or redirects something. The new text is deliberately NOT carried: a
    -- description is unbounded, and an event is a pointer — a subscriber re-reads it with the
    -- ordinary GET, exactly as it would have anyway. A new duration_hours is told the same way.
    IF NEW.description IS DISTINCT FROM OLD.description OR NEW.duration_hours IS DISTINCT FROM OLD.duration_hours THEN
        PERFORM hypothesisloop_notify_event('platform_experiment.description', NEW.id, '', '',
            NEW.id, '');
    END IF;
    -- Removed, it is answered to nobody and its tokens are refused: this is the last its agents hear.
    IF NEW.removed_at IS NOT NULL AND OLD.removed_at IS NULL THEN
        PERFORM hypothesisloop_notify_event('platform_experiment.removed', NEW.id, '', '', NEW.id, '');
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
