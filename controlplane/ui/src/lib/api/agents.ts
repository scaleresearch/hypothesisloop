import { apiCall, apiFetch, apiFetchPage, apiSend, withQuery } from '@/lib/api/request'
import type { Page } from '@/lib/api/request'
import type { AddAgentRequest, Agent, AgentMessage, AgentMessages, AgentStats, JoinedAgent, JoinedAgents } from '@/types/api'

// A list is asked for within a project. What is in a shared platform experiment of another org
// is asked for by platform_experiment_id alone: it lies in a project the caller's project_id
// never matches, and the API admits a list narrowed to a shared one.
interface AgentsParams {
  project_id?: string
  platform_experiment_id?: string
  /** Substring match against id and name. */
  q?: string
  limit?: number
  offset?: number
}

export function fetchAgentsPage(params: AgentsParams): Promise<Page<Agent>> {
  return apiFetchPage<Agent>(withQuery('/agents', params))
}

export function fetchAgentStats(params: Pick<AgentsParams, 'project_id' | 'platform_experiment_id'>): Promise<AgentStats> {
  return apiFetch<AgentStats>(withQuery('/agents/stats', params))
}

export function fetchPlatformExperimentAgents(platformExpID: string): Promise<JoinedAgent[]> {
  return apiFetch<JoinedAgents>(`/platform-experiments/${platformExpID}/agents`).then(r => r.agents)
}

// An agent the platform does not run comes back with its token.
export function addPlatformExperimentAgents(platformExpID: string, req: AddAgentRequest): Promise<JoinedAgent[]> {
  return apiSend<JoinedAgents>('POST', `/platform-experiments/${platformExpID}/agents`, req).then(r => r.agents)
}

// The token of an agent that already joined: joining again changes nothing and answers with the
// same token, which is derived from the agent's place in the experiment.
export async function fetchPlatformExperimentAgentToken(platformExpID: string, agentID: string): Promise<JoinedAgent> {
  const { agents } = await apiSend<JoinedAgents>('POST', `/platform-experiments/${platformExpID}/agents`, { agent_id: agentID })
  if (agents.length !== 1) throw new Error(`asked for the token of agent ${agentID} and was answered ${agents.length} agents`)
  return agents[0]
}

// Every message the coordinator and the owner sent to an agent of the experiment, oldest first.
export function fetchPlatformExperimentMessages(platformExpID: string): Promise<AgentMessage[]> {
  return apiFetch<AgentMessages>(`/platform-experiments/${platformExpID}/messages`).then(r => r.messages)
}

// The agent is woken with it at once. Answered 429 past the agent's hourly limit.
export async function sendPlatformExperimentMessage(platformExpID: string, agentID: string, text: string): Promise<AgentMessage[]> {
  const { messages } = await apiSend<AgentMessages>('POST', `/platform-experiments/${platformExpID}/messages`, { agent_id: agentID, text })
  return messages
}

// Answered 409 once the platform experiment has started.
export async function removePlatformExperimentAgent(platformExpID: string, agentID: string): Promise<void> {
  await apiCall('DELETE', `/platform-experiments/${platformExpID}/agents/${agentID}`)
}
