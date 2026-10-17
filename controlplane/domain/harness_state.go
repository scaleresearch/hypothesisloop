package domain

import "time"

// HarnessState is what the harness of a sandboxed agent, or of a sandboxed coordinator (Role
// CoordinatorHealthRole, no AgentID), last said of itself on its hl-state line: the subagents it
// has running, whether its turn waits for platform updates and since when, the digest of the
// instructions it runs, and the model provider's rate limit it last read. At is when the platform last read such a line.
type HarnessState struct {
	PlatformExperimentID string
	AgentID              string
	Role                 string
	Subagents            int
	TurnWaiting          bool
	TurnSince            time.Time
	// InstructionsDigest is the first InstructionsDigestLength hex characters of the digest of
	// the instructions the harness runs, "" for one that has not said, and AgentTypeVersion the
	// version of its agent type they are, 0 for one that has not said. AgentType is the type the
	// agent runs, as the platform knows it.
	InstructionsDigest string
	AgentType          string
	AgentTypeVersion   int
	Model              RateLimit
	At                 time.Time
}

// InstructionsDigestLength is how many characters of an instructions digest a harness reports.
const InstructionsDigestLength = 12

// RateLimit is the model provider's rate limit as a harness last read it from its model CLI. The
// zero value is a harness that has read none.
type RateLimit struct {
	// Rejected: the provider refuses the account's calls until ResetsAt.
	Rejected    bool
	Utilization *float64
	ResetsAt    *time.Time
}
