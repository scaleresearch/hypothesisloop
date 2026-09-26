import type { AgentQuota } from '@/types'

// One definition of an agent's accelerator-hour position. Its share of the experiment's budget
// is its guaranteed hours, or its burst hours when its tier gives it none guaranteed. Burst hours
// run on idle capacity and come out of the share, so what is left is the share less everything
// spent at either priority.
export function quotaShareAccH(q: AgentQuota): number {
  return q.guaranteed_accelerator_hours > 0 ? q.guaranteed_accelerator_hours : q.burst_accelerator_hours
}

export function quotaRemainingAccH(q: AgentQuota): number {
  return Math.max(0, quotaShareAccH(q) - q.used_guaranteed_acch - q.used_burst_acch)
}
