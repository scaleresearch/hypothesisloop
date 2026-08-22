import type { AgentQuota } from '@/types'

// One definition of an agent's accelerator-hour position, because three copies of this
// arithmetic had already drifted: two clamped each tier before summing, a third did not clamp at
// all, so an over-spent agent showed a different "remaining" in the scoreboard than in the quota
// table on the same page.
//
// Clamping is per tier and deliberate: an overrun on guaranteed is not paid for by unspent burst.
// The tiers are separate entitlements, and netting them would report headroom the agent cannot
// actually draw on.
export function guaranteedRemainingAccH(q: AgentQuota): number {
  return q.guaranteed_accelerator_hours - q.used_guaranteed_acch
}

export function burstRemainingAccH(q: AgentQuota): number {
  return q.burst_accelerator_hours - q.used_burst_acch
}

// Signed per-tier remainders are what a caller showing "over" needs; this is the clamped total.
export function quotaRemainingAccH(q: AgentQuota): number {
  return Math.max(0, guaranteedRemainingAccH(q)) + Math.max(0, burstRemainingAccH(q))
}

export function quotaUsedAccH(q: AgentQuota): number {
  return q.used_guaranteed_acch + q.used_burst_acch
}

export function quotaTotalAccH(q: AgentQuota): number {
  return q.guaranteed_accelerator_hours + q.burst_accelerator_hours
}
