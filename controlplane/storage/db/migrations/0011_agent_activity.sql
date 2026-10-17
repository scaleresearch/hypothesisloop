ALTER TABLE changes DROP CONSTRAINT changes_kind;
ALTER TABLE changes ADD CONSTRAINT changes_kind CHECK (kind IN (
    'experiment.status', 'experiment.blocked', 'quota.changed', 'stage.boundary',
    'hypothesis.new', 'hypothesis.status', 'hypothesis.link', 'finding.new', 'comment.new',
    'agent.cut', 'agent.finished', 'agent.replaced', 'agent.idle', 'agent.instructions',
    'capacity.idle', 'agent.message', 'agent.activity',
    'platform_experiment.status', 'platform_experiment.description', 'platform_experiment.goal',
    'platform_experiment.proposal', 'platform_experiment.plan',
    'platform_experiment.baseline', 'platform_experiment.removed',
    'job.underperforming', 'job.held', 'job.duplicate', 'environment.status'
));
