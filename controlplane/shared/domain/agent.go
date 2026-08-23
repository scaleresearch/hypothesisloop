package domain

import "time"

// Agent represents a registered platform participant — the identity that owns quota, submits
// hypotheses, and runs jobs. Despite the name, a row is not necessarily an AI: Kind says which it
// is. There is no auth, so registering one is a claim exactly like a hypothesis's Author is (see
// HypothesisSource) — anyone can pick an id and a kind. What matters is that once registered,
// a human participant is a first-class Agent row like any other: it can own quota, appear in
// standings, and be the AgentID referenced by hypotheses and jobs, letting a person submit ideas
// and jobs through the exact same endpoints an AI agent uses.
type Agent struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Kind             AgentKind `json:"kind"`
	PerformanceScore float64   `json:"performance_score"`
	Top3Count        int       `json:"top3_count"` // number of top-3 placements ever
	CreatedAt        time.Time `json:"created_at"`
}

// AgentKind distinguishes a human participant from an AI agent. Scheduling and standings never
// branch on it, but quota does: see ApplyParticipantQuotaTier.
type AgentKind string

const (
	// AgentKindAgent is the default: an autonomous (typically AI) research agent. Burst-tier
	// quota only — see ApplyParticipantQuotaTier.
	AgentKindAgent AgentKind = "agent"
	// AgentKindHuman is a real person registered as a participant, submitting hypotheses and
	// jobs through the same API an AI agent uses. Gets the normal guaranteed+burst split.
	AgentKindHuman AgentKind = "human"
)

// ValidAgentKind reports whether k is a recognized kind, or empty (callers default empty to
// AgentKindAgent). An unrecognized value must be rejected at registration, not stored: it would
// otherwise reach a Postgres row and every reader would have to defend against a third,
// unnamed kind forever after.
func ValidAgentKind(k AgentKind) bool {
	switch k {
	case "", AgentKindAgent, AgentKindHuman:
		return true
	default:
		return false
	}
}

// QuotaTier is which column of an experiment's budget a participant's share lands in: the normal
// guaranteed+burst split, or burst-only. It is decided per signup (see ResolveQuotaTier), not
// fixed by AgentKind — one experiment can mix guaranteed humans, burst-only agents, and agents
// explicitly granted guaranteed quota, all at once.
type QuotaTier string

const (
	// QuotaTierGuaranteed keeps the normal guaranteed+burst split.
	QuotaTierGuaranteed QuotaTier = "guaranteed"
	// QuotaTierBurstOnly collapses everything into burst: no reserved share, first-come only.
	QuotaTierBurstOnly QuotaTier = "burst_only"
)

// ValidQuotaTierOverride reports whether s is a legal signup-time override: empty (defer to the
// agent's kind, see ResolveQuotaTier) or one of the two named tiers. Checked at signup so a typo
// is rejected there, not silently stored and misread as "" (kind default) forever after.
func ValidQuotaTierOverride(s string) bool {
	switch QuotaTier(s) {
	case "", QuotaTierGuaranteed, QuotaTierBurstOnly:
		return true
	default:
		return false
	}
}

// ResolveQuotaTier applies a signup's explicit override, or — when none was given — the kind
// default: humans guaranteed+burst, everyone else (including an unrecognized kind, which must
// never silently gain priority capacity) burst-only.
func ResolveQuotaTier(kind AgentKind, override QuotaTier) QuotaTier {
	if override != "" {
		return override
	}
	if kind == AgentKindHuman {
		return QuotaTierGuaranteed
	}
	return QuotaTierBurstOnly
}

// ApplyQuotaTier is the platform's one enforcement point for where a participant's
// (guaranteed, burst) share lands: every place that grants or credits quota (initial allocation,
// stage-boundary redistribution, donations) must route its pair through this before writing it,
// or a burst-only participant regains guaranteed quota through whichever path skipped it. Total
// entitlement is preserved — nothing is added or dropped, only which column it lands in.
func ApplyQuotaTier(tier QuotaTier, guaranteed, burst float64) (float64, float64) {
	if tier == QuotaTierGuaranteed {
		return guaranteed, burst
	}
	return 0, guaranteed + burst
}
