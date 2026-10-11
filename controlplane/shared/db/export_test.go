package db

import "testing"

// TestPool and ClosedRunWithAgents are the scratch database and the closed run of this package's
// tests, for a test of what reads them from outside it.
var (
	TestPool            = eventsTestDB
	ClosedRunWithAgents = func(t *testing.T, pool *Pool) ClosedRun { return closedRunWithAgents(t, pool) }
)
