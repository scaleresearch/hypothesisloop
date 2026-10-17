package domain

import (
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
)

// Agent is a participant of platform experiments: the identity that owns quota, submits jobs and
// holds standings. The platform creates one when it is added to a platform experiment. Whether a
// person or an agent wrote a record is that record's actor, and no property of an agent.
type Agent struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	OrgID     string    `json:"org_id"`
	Top3Count int       `json:"top3_count"` // number of top-3 placements ever
	CreatedAt time.Time `json:"created_at"`
}

// AgentStats is the whole set of agents matching a filter, aggregated in PostgreSQL: a list read
// is one page, so a figure tallied from it describes the page.
type AgentStats struct {
	Total int `json:"total"`
	// Top3Eligible counts agents with at least one top-3 placement.
	Top3Eligible int `json:"top3_eligible"`
}

// QuotaTier is the contract's own (experimentspec), decided per signup (see ResolveQuotaTier).
type QuotaTier = experimentspec.QuotaTier

const (
	QuotaTierGuaranteed = experimentspec.QuotaTierGuaranteed
	QuotaTierBurstOnly  = experimentspec.QuotaTierBurstOnly
)

// QuotaTiers is every quota tier.
var QuotaTiers = experimentspec.QuotaTiers

// ResolveQuotaTier is the tier a signup is stored with: its explicit override, else the
// experiment's default_quota_tier.
func ResolveQuotaTier(override, experimentDefault QuotaTier) QuotaTier {
	if override != "" {
		return override
	}
	return experimentDefault
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
