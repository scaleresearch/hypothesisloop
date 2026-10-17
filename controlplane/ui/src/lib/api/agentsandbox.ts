import { apiFetch } from '@/lib/api/request'
import type { AgentSandbox } from '@/types/api'

// Whether the platform can host agents now, and why not while it cannot.
export function fetchAgentSandbox(): Promise<AgentSandbox> {
  return apiFetch<AgentSandbox>('/agentsandbox')
}
