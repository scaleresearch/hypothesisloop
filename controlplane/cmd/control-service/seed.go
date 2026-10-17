package main

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain/agenttypes"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// seedBuiltins writes the built-in agent types and the platform's rules this build holds, under
// its release (agenttypes.Release), right after the schema and under its lock.
func seedBuiltins(pool *db.Pool, logger *zap.Logger) {
	builtins, err := agenttypes.Builtins()
	if err != nil {
		logger.Fatal("control-service: built-in agent types", zap.Error(err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seeded, err := db.SeedBuiltins(ctx, pool, agenttypes.Release, builtins, agenttypes.PlatformRules)
	if err != nil {
		logger.Fatal("control-service: seed built-in agent types and rules", zap.Error(err))
	}
	for _, s := range seeded.Types {
		logger.Info("built-in agent type has a new version", zap.String("agent_type", s.ID), zap.Int("version", s.Propagation.Version),
			zap.Int("agents_told", s.Propagation.Told))
	}
	if seeded.RulesVersion > 0 {
		logger.Info("the platform's rules have a new version", zap.Int("version", seeded.RulesVersion), zap.Int("agents_told", seeded.RulesTold))
	}
	if len(seeded.Skipped) > 0 {
		logger.Warn("a later release seeded these, which this one leaves as they are", zap.Int("release", agenttypes.Release), zap.Strings("seeded", seeded.Skipped))
	}
}

// migrate brings the schema to this release's. control-service is the sole owner of the schema:
// metrics-service reads the same tables and never migrates them.
func migrate(pool *db.Pool, logger *zap.Logger) {
	migrateCtx, migrateCancel := context.WithTimeout(context.Background(), db.MigrationTimeouts.Statement)
	migrated, err := db.Migrate(migrateCtx, pool)
	migrateCancel()
	if err != nil {
		logger.Fatal("control-service: migrate database", zap.Error(err))
	}
	if migrated.Adopted {
		logger.Info("adopted a database from before migrations at the baseline migration, without running it")
	}
	for _, name := range migrated.Applied {
		logger.Info("applied database migration", zap.String("migration", name))
	}
	if len(migrated.Unknown) > 0 {
		logger.Warn("the database has migrations of a later release, which this one runs against until it is replaced",
			zap.Strings("migrations", migrated.Unknown))
	}
}
