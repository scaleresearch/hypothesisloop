-- The launcher of hosted agents runs inside the control plane: there is one, nothing registers it
-- and it checks in with nobody, so no row says which launcher holds a machine.
DROP INDEX idx_experiment_signups_launcher;
ALTER TABLE experiment_signups DROP COLUMN launcher;
DROP INDEX idx_platform_experiments_coordinator_launcher;
ALTER TABLE platform_experiments DROP COLUMN coordinator_launcher;
DROP TABLE launchers;
