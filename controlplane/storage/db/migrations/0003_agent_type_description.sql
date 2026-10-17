-- Keep the previous release working against this schema (README, Database).
ALTER TABLE agent_types ADD COLUMN description TEXT NOT NULL DEFAULT '';
