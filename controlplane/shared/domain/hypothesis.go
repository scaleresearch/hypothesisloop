package domain

import (
	"strings"
	"time"
)

// Hypothesis is a registered research claim an agent intends to test, scoped to a single
// platform experiment — each platform experiment accumulates its own shared pool of ideas.
// Agents register (or retrieve, if an equivalent one already exists within the same platform
// experiment — see NormalizeHypothesisText) a Hypothesis before submitting a job against it;
// ExperimentMeta.HypothesisID is required and must reference a row here whose
// PlatformExperimentID matches the job's own PlatformExperimentID. This is the real
// uniqueness check: a DB-level UNIQUE index on (platform_experiment_id, normalized_text)
// (see schema.sql) rejects a second registration of the same claim within the same platform
// experiment, replacing the old always-novel dedup stub. The same claim registered under a
// different platform experiment is a distinct hypothesis — different research programs don't
// share an idea pool.
type Hypothesis struct {
	ID                   string    `json:"id"`
	AgentID              string    `json:"agent_id"`
	PlatformExperimentID string    `json:"platform_experiment_id"`
	Text                 string    `json:"text"`
	CreatedAt            time.Time `json:"created_at"`
}

// HypothesisFinding is the post-run write-up an agent files after one of its jobs testing
// this hypothesis reaches a terminal state (COMPLETED, FAILED, EVICTED, or REJECTED).
// Attached to the hypothesis (not just the job) so the accumulated evidence for a claim is
// visible in one place to every agent considering testing it again — see
// services/scheduler.WriteExperimentSummary and the scheduler's summary gate. One finding per job
// (ExperimentID), but a hypothesis accumulates one per job that tested it.
type HypothesisFinding struct {
	ID           string    `json:"id"`
	HypothesisID string    `json:"hypothesis_id"`
	ExperimentID string    `json:"experiment_id"`
	AgentID      string    `json:"agent_id"`
	Summary      string    `json:"summary"`
	CreatedAt    time.Time `json:"created_at"`
}

// HypothesisComment is a freeform, job-independent note on a hypothesis — amending, abandoning,
// or revising a claim, or cross-referencing another hypothesis's finding — recorded without
// requiring a terminal job. Contrast with HypothesisFinding, which is the measured write-up
// bound to one finished job: a comment records a thought, a finding records a result. The two
// must not overlap — recording the same conclusion as both re-pollutes the shared idea pool.
type HypothesisComment struct {
	ID           string    `json:"id"`
	HypothesisID string    `json:"hypothesis_id"`
	AgentID      string    `json:"agent_id"`
	Text         string    `json:"text"`
	CreatedAt    time.Time `json:"created_at"`
}

// NormalizeHypothesisText collapses a hypothesis statement to a canonical form (lowercased,
// whitespace-collapsed, trimmed) so that trivially-different phrasings of the same claim
// ("Model X improves accuracy" vs "model x improves accuracy.") collide on the same
// registered hypothesis instead of creating near-duplicate rows.
func NormalizeHypothesisText(text string) string {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	return strings.Join(fields, " ")
}
