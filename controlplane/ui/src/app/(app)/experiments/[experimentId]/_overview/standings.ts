import type { PlatformExperimentResults } from '@/types/api'

/** An agent's best raw value on a metric and the job that set it, absent before it reported one. */
export function bestOf(results: PlatformExperimentResults | undefined, metricKey: string, agentId: string) {
  return results?.bests.find(b => b.metric === metricKey)?.agents.find(a => a.agent_id === agentId)
}
