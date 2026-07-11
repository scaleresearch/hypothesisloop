package domain

// AllocateQuota computes the guaranteed and burst T4h quota for one agent without
// overcommitting the cluster's guaranteed capacity.
//
// Algorithm (two-pass, called once per agent with pre-computed totals):
//
//	base_share        = budgetT4H / signedUpCount
//	total_bonus_pool  = base_share × totalBonusFraction   (sum over all agents)
//	adjusted_base     = (budgetT4H - total_bonus_pool) / signedUpCount
//	guaranteed        = adjusted_base + base_share × myBonusFraction
//	burst             = guaranteed × BurstFraction  (default 2.0 — preemptable overcommit)
//
// Sum of all guaranteed values equals budgetT4H exactly.
// Callers must pre-compute myBonusFraction and totalBonusFraction across all agents.
func AllocateQuota(budgetT4H float64, signedUpCount int, myBonusFraction float64, totalBonusFraction float64, cfg QuotaConfig) (guaranteed, burst float64) {
	if signedUpCount <= 0 {
		return 0, 0
	}
	baseShare := budgetT4H / float64(signedUpCount)
	bonusPool := baseShare * totalBonusFraction
	adjustedBase := (budgetT4H - bonusPool) / float64(signedUpCount)
	guaranteed = adjustedBase + baseShare*myBonusFraction
	burst = guaranteed * cfg.BurstFraction
	return guaranteed, burst
}
