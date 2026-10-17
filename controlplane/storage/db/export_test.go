package db

import (
	"context"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// TestPool and ClosedRunWithAgents are the scratch database and the closed run of this package's
// tests, for a test of what reads them from outside it.
var (
	TestPool            = eventsTestDB
	ClosedRunWithAgents = func(t *testing.T, pool *Pool) ClosedRun { return closedRunWithAgents(t, pool) }
)

// insertExperiment writes exp in a write of its own.
func insertExperiment(ctx context.Context, pool *Pool, exp *domain.Experiment) error {
	return pool.write(ctx, func(w *Write) error { return createExperiment(ctx, w, exp) })
}
