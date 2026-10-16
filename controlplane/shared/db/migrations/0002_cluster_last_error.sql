-- last_error is what the control plane last refused a reconcile of the cluster's agent with, and
-- last_error_at when. The next one it accepts clears both. Its owner reads them beside the last
-- contact and the agent's version, which are the metrics store's.
-- IF NOT EXISTS: databases started by the schema.sql of 456f346d already have both columns.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS last_error_at TIMESTAMPTZ;
