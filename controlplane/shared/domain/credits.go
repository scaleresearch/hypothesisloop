package domain

import "math"

// RoundHours is an allocation of hours as it is stored and shown: to the millionth of an hour,
// so that a share of a budget never carries the noise of the division that made it.
func RoundHours(hours float64) float64 {
	return math.Round(hours*1e6) / 1e6
}

// AllocateQuota computes the guaranteed and burst AccH quota for one agent without
// overcommitting the cluster's guaranteed capacity.
//
// Algorithm (two-pass, called once per agent with pre-computed totals):
//
//	base_share        = budgetAccH / signedUpCount
//	total_bonus_pool  = base_share × totalBonusFraction   (sum over all agents)
//	adjusted_base     = (budgetAccH - total_bonus_pool) / signedUpCount
//	guaranteed        = adjusted_base + base_share × myBonusFraction
//	burst             = guaranteed × BurstFraction
//
// The guaranteed values sum to budgetAccH: they are the agents' shares of it. Burst is how much
// of its share an agent may run at burst priority, on capacity nobody holds a guarantee for, and
// adds nothing to the share (see AgentQuota.Share).
// Callers must pre-compute myBonusFraction and totalBonusFraction across all agents.
func AllocateQuota(budgetAccH float64, signedUpCount int, myBonusFraction float64, totalBonusFraction float64, cfg QuotaConfig) (guaranteed, burst float64) {
	if signedUpCount <= 0 {
		return 0, 0
	}
	baseShare := budgetAccH / float64(signedUpCount)
	bonusPool := baseShare * totalBonusFraction
	adjustedBase := (budgetAccH - bonusPool) / float64(signedUpCount)
	guaranteed = RoundHours(adjustedBase + baseShare*myBonusFraction)
	return guaranteed, RoundHours(guaranteed * cfg.BurstFraction)
}
