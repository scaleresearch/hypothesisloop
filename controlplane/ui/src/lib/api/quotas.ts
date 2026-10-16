import { apiFetch } from '@/lib/api/request'
import type { AgentQuota } from '@/types/quotas'

export function fetchPlatformExperimentQuotas(platformExpID: string): Promise<AgentQuota[]> {
  return apiFetch<AgentQuota[]>(`/platform-experiments/${platformExpID}/quotas`)
}

// Answered 404 for an agent with no quota there: quota is granted at the start, to the agents signed up.
export function fetchAgentQuota(platformExpID: string, agentID: string): Promise<AgentQuota> {
  return apiFetch<AgentQuota>(`/platform-experiments/${platformExpID}/quotas/${agentID}`)
}
