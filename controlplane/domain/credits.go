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

// ExecutorQuota is an executor's allocation and what it used of it, as an executor that
// joins a running platform experiment is given its share from. Ended is an executor that
// finished or whose machine stopped with no restart left.
type ExecutorQuota struct {
	AgentID    string
	Guaranteed float64
	Burst      float64
	Used       float64
	Ended      bool
}

// Unspent is what is left of the executor's share past what it used.
func (q ExecutorQuota) Unspent() float64 {
	share := q.Guaranteed
	if share <= 0 {
		share = q.Burst
	}
	return max(0, share-q.Used)
}

// LateShare is the share of an executor that joins a running platform experiment whose other
// executors hold quotas: an even part, with the executors still at work, of what all of them
// left unspent. taken is what that share takes from each: first what the executors that ended
// left, in the order given, then the rest from the executors at work that hold more unspent
// than the share, in proportion to what they hold above it. None is left with less unspent than
// the newcomer is given, and none gives back what it used.
func LateShare(quotas []ExecutorQuota) (share float64, taken map[string]float64) {
	unspent, atWork := 0.0, 0
	for _, q := range quotas {
		unspent += q.Unspent()
		if !q.Ended {
			atWork++
		}
	}
	share = RoundHours(unspent / float64(atWork+1))
	taken = map[string]float64{}
	need := share
	for _, q := range quotas {
		if !q.Ended || need <= 0 {
			continue
		}
		if take := min(need, q.Unspent()); take > 0 {
			taken[q.AgentID] = take
			need -= take
		}
	}
	above := 0.0
	for _, q := range quotas {
		if !q.Ended {
			above += max(0, q.Unspent()-share)
		}
	}
	if need <= 0 || above <= 0 {
		return share, taken
	}
	for _, q := range quotas {
		if excess := q.Unspent() - share; !q.Ended && excess > 0 {
			taken[q.AgentID] = RoundHours(excess * min(1, need/above))
		}
	}
	return share, taken
}
