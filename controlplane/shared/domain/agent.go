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

// AgentKind distinguishes a human participant from an AI agent. Scheduling, standings, and quota
// (see ResolveQuotaTier) never branch on it — it exists for identity/display and submitter-policy
// checks only. A human and an agent get exactly the same quota treatment.
type AgentKind string

const (
	// AgentKindAgent is the default: an autonomous (typically AI) research agent.
	AgentKindAgent AgentKind = "agent"
	// AgentKindHuman is a real person registered as a participant, submitting hypotheses and
	// jobs through the same API an AI agent uses.
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
// guaranteed+burst split, or burst-only. It is decided per signup (see ResolveQuotaTier),
// independent of AgentKind — one experiment can mix guaranteed and burst-only participants of
// either kind, all at once.
type QuotaTier string

const (
	// QuotaTierGuaranteed keeps the normal guaranteed+burst split.
	QuotaTierGuaranteed QuotaTier = "guaranteed"
	// QuotaTierBurstOnly collapses everything into burst: no reserved share, first-come only.
	QuotaTierBurstOnly QuotaTier = "burst_only"
)

// ValidQuotaTierOverride reports whether s is a legal signup-time override: empty (defer to the
// experiment's policy, see ResolveQuotaTier) or one of the two named tiers. Checked at signup so
// a typo is rejected there, not silently stored and misread as "" (policy default) forever after.
func ValidQuotaTierOverride(s string) bool {
	switch QuotaTier(s) {
	case "", QuotaTierGuaranteed, QuotaTierBurstOnly:
		return true
	default:
		return false
	}
}

// ResolveQuotaTier applies a signup's explicit override, then the experiment's default_quota_tier
// policy, then a fixed guaranteed default. Deliberately independent of AgentKind: a human and an
// agent with no override on the same (or an unset-policy legacy) experiment get identical
// treatment. An experiment created before default_quota_tier existed (empty policy, no override)
// now also resolves to guaranteed — this intentionally changes what a future quota-affecting event
// (a stage-boundary credit, a donation) on such an experiment would compute, since the platform no
// longer has any kind-based tier to fall back to; it does not rewrite quota already materialized
// by an earlier ResolveQuotaTier call (see AllocateQuota/ApplyQuotaTier call sites).
func ResolveQuotaTier(override, experimentDefault QuotaTier) QuotaTier {
	if override != "" {
		return override
	}
	if experimentDefault != "" {
		return experimentDefault
	}
	return QuotaTierGuaranteed
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
