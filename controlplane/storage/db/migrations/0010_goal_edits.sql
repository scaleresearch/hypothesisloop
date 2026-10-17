-- A person changes the goal itself where a revision was asked beside it. The revisions of a draft
-- still being planned are folded into its goal; goal_version counts the goals it had, and a
-- planner's answer names the version it planned from.
ALTER TABLE platform_experiments ADD COLUMN goal_version INTEGER NOT NULL DEFAULT 1;
UPDATE platform_experiments pe SET goal = pe.goal || E'\n\n' || r.messages, goal_version = 1 + r.n
FROM (SELECT platform_experiment_id, string_agg(message, E'\n\n' ORDER BY id) messages, count(*) n
      FROM planning_requests GROUP BY platform_experiment_id) r
WHERE r.platform_experiment_id = pe.id AND pe.status = 'draft';
DROP TABLE planning_requests;

DELETE FROM changes WHERE kind = 'platform_experiment.revision';
ALTER TABLE changes DROP CONSTRAINT changes_kind;
ALTER TABLE changes ADD CONSTRAINT changes_kind CHECK (kind IN (
    'experiment.status', 'experiment.blocked', 'quota.changed', 'stage.boundary',
    'hypothesis.new', 'hypothesis.status', 'hypothesis.link', 'finding.new', 'comment.new',
    'agent.cut', 'agent.finished', 'agent.replaced', 'agent.idle', 'agent.instructions',
    'capacity.idle', 'agent.message',
    'platform_experiment.status', 'platform_experiment.description', 'platform_experiment.goal',
    'platform_experiment.proposal', 'platform_experiment.plan',
    'platform_experiment.baseline', 'platform_experiment.removed',
    'job.underperforming', 'job.held', 'job.duplicate', 'environment.status'
));
