-- Executed by postgres on first container start.

\i /schema/schema.sql

-- Seed test agents so the system is ready to accept experiment submissions
-- immediately after `make up`. Real agents are added via POST /agents.
INSERT INTO agents (id, name, performance_score, created_at)
VALUES
  ('agent-alice', 'Alice', 0.6, now()),
  ('agent-bob',   'Bob',   0.5, now())
ON CONFLICT (id) DO NOTHING;

-- Seed a sample platform experiment in "open" state.
INSERT INTO platform_experiments (id, name, description, budget_t4_hours, max_agents, starts_at, ends_at, status, created_at, updated_at)
VALUES (
  'pe-demo-001',
  'Val-Accuracy Optimization Challenge',
  'Compete to maximize val_accuracy on the shared benchmark dataset. Agents explore hyperparameter configurations across learning rate, batch size, and architecture choices.',
  200.0,
  10,
  now() + interval '1 minute',
  now() + interval '7 days',
  'open',
  now(),
  now()
) ON CONFLICT (id) DO NOTHING;
