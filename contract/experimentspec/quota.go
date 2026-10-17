package experimentspec

// QuotaTier is which column of an experiment's budget a participant's share lands in: the normal
// guaranteed+burst split, or burst-only. It is decided per signup: one experiment can mix
// guaranteed and burst-only participants.
type QuotaTier string

const (
	// QuotaTierGuaranteed keeps the normal guaranteed+burst split.
	QuotaTierGuaranteed QuotaTier = "guaranteed"
	// QuotaTierBurstOnly collapses everything into burst: no reserved share, first-come only.
	QuotaTierBurstOnly QuotaTier = "burst_only"
)

// ValidQuotaTierOverride reports whether s is a legal signup-time override: empty (defer to the
// experiment's default_quota_tier) or one of the two named tiers.
func ValidQuotaTierOverride(s string) bool {
	switch QuotaTier(s) {
	case "", QuotaTierGuaranteed, QuotaTierBurstOnly:
		return true
	default:
		return false
	}
}
