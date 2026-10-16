-- Agents run the current version of their agent type, and the type's model and hyperparameters
-- unless they name their own. Built-in versions and the platform's rules carry the release that
-- seeded them, every change of an agent type is kept in agent_type_events, and a running platform
-- experiment says which researchers it does not replace. It breaks the previous release, which
-- reads the pinned versions: deploy it with the release that reads it.

-- A signup and a coordinator name their type only: they run its current version.
ALTER TABLE experiment_signups
    DROP CONSTRAINT experiment_signups_agent_type,
    DROP COLUMN agent_type_version,
    ADD CONSTRAINT experiment_signups_agent_type FOREIGN KEY (agent_type) REFERENCES agent_types(id);
ALTER TABLE platform_experiments
    DROP CONSTRAINT platform_experiments_coordinator_agent_type,
    DROP COLUMN coordinator_agent_type_version,
    ADD CONSTRAINT platform_experiments_coordinator_agent_type FOREIGN KEY (coordinator_agent_type) REFERENCES agent_types(id);

-- NULL model or hyperparameters: the type's, read when the agent is served. A value the type's
-- current version already gives followed it by value until now, and follows it from here on.
ALTER TABLE experiment_signups
    ALTER COLUMN model DROP NOT NULL,
    ALTER COLUMN model DROP DEFAULT,
    ALTER COLUMN hyperparameters DROP NOT NULL,
    ALTER COLUMN hyperparameters DROP DEFAULT;
UPDATE experiment_signups s SET model = NULL
FROM agent_types t JOIN agent_type_versions v ON v.type_id = t.id AND v.version = t.current_version
WHERE t.id = s.agent_type AND (s.model = '' OR s.model = v.settings->>'model');
UPDATE experiment_signups s SET hyperparameters = NULL
FROM agent_types t JOIN agent_type_versions v ON v.type_id = t.id AND v.version = t.current_version
WHERE t.id = s.agent_type AND s.hyperparameters = COALESCE(v.settings->'hyperparameters', '{}'::jsonb);
ALTER TABLE platform_experiments
    ALTER COLUMN coordinator_model DROP NOT NULL,
    ALTER COLUMN coordinator_model DROP DEFAULT;
UPDATE platform_experiments pe SET coordinator_model = NULL
FROM agent_types t JOIN agent_type_versions v ON v.type_id = t.id AND v.version = t.current_version
WHERE t.id = pe.coordinator_agent_type AND (pe.coordinator_model = '' OR pe.coordinator_model = v.settings->>'model');

-- The release of the binary that seeded a built-in version (agenttypes.Release), NULL for an org's.
ALTER TABLE agent_type_versions ADD COLUMN release INTEGER;

CREATE FUNCTION hypothesisloop_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

-- The rules every agent's prompt holds ({{rules}}), seeded at every start like the built-in agent
-- types (db.SeedBuiltins). The newest version is the one served.
CREATE TABLE platform_rules (
    version    INTEGER     PRIMARY KEY,
    rules      TEXT        NOT NULL,
    digest     TEXT        NOT NULL,
    release    INTEGER     NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT platform_rules_positive CHECK (version > 0)
);
CREATE TRIGGER platform_rules_append_only BEFORE UPDATE OR DELETE ON platform_rules
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_append_only();

-- Who changed an agent type, when and how, and the version it was at after.
CREATE TABLE agent_type_events (
    id         BIGSERIAL   PRIMARY KEY,
    type_id    TEXT        NOT NULL REFERENCES agent_types(id),
    action     TEXT        NOT NULL,
    version    INTEGER     NOT NULL,
    detail     TEXT        NOT NULL DEFAULT '',
    actor_kind TEXT        NOT NULL,
    actor_id   TEXT        NOT NULL DEFAULT '',
    at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT agent_type_events_action CHECK (action IN ('create', 'edit', 'rename', 'describe', 'archive', 'restore')),
    CONSTRAINT agent_type_events_actor_kind
        CHECK (actor_kind IN ('user', 'key', 'agent', 'coordinator', 'staff', 'unverified'))
);
CREATE INDEX agent_type_events_by_type ON agent_type_events (type_id, id);
CREATE TRIGGER agent_type_events_append_only BEFORE UPDATE OR DELETE ON agent_type_events
    FOR EACH ROW EXECUTE FUNCTION hypothesisloop_append_only();
INSERT INTO agent_type_events (type_id, action, version, actor_kind, actor_id, at)
SELECT v.type_id, CASE WHEN v.version = 1 THEN 'create' ELSE 'edit' END, v.version, v.created_by_kind, v.created_by_id, v.created_at
FROM agent_type_versions v ORDER BY v.created_at, v.type_id, v.version;
INSERT INTO agent_type_events (type_id, action, version, actor_kind, at)
SELECT t.id, 'archive', t.current_version, 'unverified', t.archived_at FROM agent_types t WHERE t.archived_at IS NOT NULL;

-- Why the platform keeps places of the researchers of a running platform experiment empty, NULL
-- while it keeps none: told as platform_experiment.status with it as the detail.
ALTER TABLE platform_experiments
    ADD COLUMN researchers_held     TEXT,
    ADD COLUMN researchers_held_xid BIGINT;

CREATE OR REPLACE FUNCTION hypothesisloop_platform_experiments_touch() RETURNS trigger AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        NEW.status_changed_at := now();
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
        NEW.description_changed_at := now();
        NEW.description_changed_xid := pg_current_xact_id()::text::bigint;
    END IF;
    IF NEW.summary IS DISTINCT FROM OLD.summary THEN
        NEW.summary_at := now();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

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
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- The value is the type's current version: a later change of the type or the rules writes the
-- xid again, so the one event replay keeps of an agent says what its live twin said.
CREATE OR REPLACE FUNCTION hypothesisloop_agent_instructions_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('agent.instructions', NEW.agent_id,
        (SELECT t.current_version::text FROM agent_types t WHERE t.id = NEW.agent_type), NEW.agent_type, NEW.platform_experiment_id, NEW.agent_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION hypothesisloop_coordinator_instructions_notify() RETURNS trigger AS $$
BEGIN
    PERFORM hypothesisloop_notify_event('agent.instructions', 'coordinator',
        (SELECT t.current_version::text FROM agent_types t WHERE t.id = NEW.coordinator_agent_type), NEW.coordinator_agent_type, NEW.id, 'coordinator');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
