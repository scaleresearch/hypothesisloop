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
func seedBuiltins(store *db.Store, logger *zap.Logger) {
	builtins, err := agenttypes.Builtins()
	if err != nil {
		logger.Fatal("control-service: built-in agent types", zap.Error(err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seeded, err := store.SeedBuiltins(ctx, agenttypes.Release, builtins, agenttypes.PlatformRules)
	if err != nil {
		logger.Fatal("control-service: seed built-in agent types and rules", zap.Error(err))
	}
	for _, s := range seeded.Types {
		logger.Info("built-in agent type has a new version", zap.String("agent_type", s.ID), zap.Int("version", s.Propagation.Version),
			zap.Int("agents_told", s.Propagation.Told))
	}
	if len(seeded.Archived) > 0 {
		logger.Info("archived the built-in agent types this release no longer has", zap.Int("release", agenttypes.Release), zap.Strings("archived", seeded.Archived))
	}
	if seeded.RulesVersion > 0 {
		logger.Info("the platform's rules have a new version", zap.Int("version", seeded.RulesVersion), zap.Int("agents_told", seeded.RulesTold))
	}
	if len(seeded.Skipped) > 0 {
		logger.Warn("a later release seeded these, which this one leaves as they are", zap.Int("release", agenttypes.Release), zap.Strings("seeded", seeded.Skipped))
	}
}

// ensureSchema builds the schema on an empty database and refuses any other that is not exactly
// it. control-service is the sole owner of the schema: metrics-service reads the same tables and
// never builds them.
func ensureSchema(pool *db.Pool, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	built, err := db.EnsureSchema(ctx, pool)
	cancel()
	if err != nil {
		logger.Fatal("control-service: database schema", zap.Error(err))
	}
	if built {
		logger.Info("built the database schema on an empty database")
	}
}
