// Mirrors quota.AgentQuotaView. What remains is the largest estimate a job submitted now is
// admitted with, as admission counts it.
export interface AgentQuota {
  id: string
  agent_id: string
  platform_experiment_id: string
  share_accelerator_hours: number
  remaining_guaranteed_accelerator_hours: number
  remaining_burst_accelerator_hours: number
  guaranteed_accelerator_hours: number
  burst_accelerator_hours: number
  used_guaranteed_acch: number
  used_burst_acch: number
}
