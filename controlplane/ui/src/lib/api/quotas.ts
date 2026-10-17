import { apiFetch } from '@/lib/api/request'
import type { AgentQuotaView } from '@/types/api'

export function fetchPlatformExperimentQuotas(platformExpID: string): Promise<AgentQuotaView[]> {
  return apiFetch<AgentQuotaView[]>(`/platform-experiments/${platformExpID}/quotas`)
}

// Answered 404 for an agent with no quota there: quota is granted at the start, to the agents signed up.
export function fetchAgentQuota(platformExpID: string, agentID: string): Promise<AgentQuotaView> {
  return apiFetch<AgentQuotaView>(`/platform-experiments/${platformExpID}/quotas/${agentID}`)
}
