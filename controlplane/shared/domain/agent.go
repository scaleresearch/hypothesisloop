package domain

import "time"

// Agent is a participant of platform experiments: the identity that owns quota, submits jobs and
// holds standings. The platform creates one when it is added to a platform experiment. Whether a
// person or an agent wrote a record is that record's actor, and no property of an agent.
type Agent struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	OrgID            string    `json:"org_id"`
	PerformanceScore float64   `json:"performance_score"`
	Top3Count        int       `json:"top3_count"` // number of top-3 placements ever
	CreatedAt        time.Time `json:"created_at"`
}

// AgentStats is the whole set of agents matching a filter, aggregated in PostgreSQL: a list read
// is one page, so a figure tallied from it describes the page.
type AgentStats struct {
	Total int `json:"total"`
	// Top3Eligible counts agents with at least one top-3 placement.
	Top3Eligible int `json:"top3_eligible"`
	// AvgPerformanceScore is 0 when Total is 0.
	AvgPerformanceScore float64 `json:"avg_performance_score"`
}

// QuotaTier is which column of an experiment's budget a participant's share lands in: the normal
// guaranteed+burst split, or burst-only. It is decided per signup (see ResolveQuotaTier):
// one experiment can mix guaranteed and burst-only participants.
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
// policy, then a fixed guaranteed default, which is also what an experiment created before
// default_quota_tier existed resolves to.
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
// stage-boundary redistribution) must route its pair through this before writing it,
// or a burst-only participant regains guaranteed quota through whichever path skipped it. A
// burst-only participant holds its share in the burst column, and the share is the guaranteed
// amount: burst hours are inside a share, never beside it.
func ApplyQuotaTier(tier QuotaTier, guaranteed, burst float64) (float64, float64) {
	if tier == QuotaTierGuaranteed {
		return guaranteed, burst
	}
	return 0, guaranteed
}
