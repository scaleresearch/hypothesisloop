package domain

import "time"

// Agent represents an autonomous research agent interacting with the platform.
type Agent struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	PerformanceScore float64   `json:"performance_score"`
	Top3Count        int       `json:"top3_count"` // number of top-3 placements ever
	CreatedAt        time.Time `json:"created_at"`
}

// AgentBalance is an agent's all-time credit_ledger balance (sum of every entry ever
// appended for them). Global per-agent budgets are superseded by per-platform-experiment
// quotas (see quota.PlatformExperimentsService) — nothing currently appends to
// credit_ledger, so balance is always 0 today; this exists so the agents dashboard has a
// live (if currently empty) endpoint instead of a 404, and starts reflecting real numbers
// the moment anything writes to the ledger again.
type AgentBalance struct {
	AgentID          string  `json:"agent_id"`
	Balance          float64 `json:"balance"`
	PerformanceBonus float64 `json:"performance_bonus"`
	ExperienceBonus  float64 `json:"experience_bonus"`
	BorrowingUsed    float64 `json:"borrowing_used"`
}

// CreditLedgerEntry records a single compute transaction for an agent.
type CreditLedgerEntry struct {
	ID                   string    `json:"id"`
	AgentID              string    `json:"agent_id"`
	Amount               float64   `json:"amount"` // positive = credit, negative = debit (in T4h)
	Reason               string    `json:"reason"` // allocation | spend | refund
	ExperimentID         *string   `json:"experiment_id,omitempty"`
	PlatformExperimentID *string   `json:"platform_experiment_id,omitempty"`
	Period               int       `json:"period"`
	CreatedAt            time.Time `json:"created_at"`
}
