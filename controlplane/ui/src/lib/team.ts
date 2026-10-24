import { DEFAULT_AGENT_TYPE, RESEARCHER_AGENT_TYPE, parseHyperparameters } from '@/lib/agent-types'
import type { AgentPoolEntry, JoinedAgent } from '@/types/api'

/**
 * One entry of a team as typed: an agent type, how many of it, the model they run on (empty for
 * the one they inherit) and the hyperparameters each starts from, as JSON text (empty for none).
 * Numbers and JSON are held as typed and parsed on submit, so a field can be half-written.
 */
export type TeamRow = { agent_type: string; count: string; model: string; hyperparameters: string }

/** What a new experiment's team starts as: executors of the default type and one researcher. */
export function defaultTeam(executors: number): TeamRow[] {
  return [
    { agent_type: DEFAULT_AGENT_TYPE, count: String(executors), model: '', hyperparameters: '' },
    { agent_type: RESEARCHER_AGENT_TYPE, count: '1', model: '', hyperparameters: '' },
  ]
}

export function toTeamRows(entries: AgentPoolEntry[] | undefined): TeamRow[] {
  return (entries ?? []).map(e => ({
    agent_type: e.agent_type || DEFAULT_AGENT_TYPE,
    count: String(e.count),
    model: e.model ?? '',
    hyperparameters: e.hyperparameters && Object.keys(e.hyperparameters).length > 0 ? JSON.stringify(e.hyperparameters, null, 2) : '',
  }))
}

/** The rows as the agents entries the API takes, or the first row whose hyperparameters are no JSON object. */
export function toTeam(rows: TeamRow[]): { agents: AgentPoolEntry[] } | { problem: string } {
  const agents: AgentPoolEntry[] = []
  for (let i = 0; i < rows.length; i++) {
    const r = rows[i]
    const parsed = parseHyperparameters(r.hyperparameters)
    if ('problem' in parsed) return { problem: `Team entry ${i + 1}: hyperparameters: ${parsed.problem}` }
    agents.push({
      agent_type: r.agent_type,
      count: Number(r.count),
      ...(r.model.trim() ? { model: r.model.trim() } : {}),
      ...(Object.keys(parsed.value).length > 0 ? { hyperparameters: parsed.value } : {}),
    })
  }
  return { agents }
}

/** The executors at work, oldest first: the agents max_agents holds to that the platform says are at work. */
export function executorsAtWork(roster: JoinedAgent[]): JoinedAgent[] {
  return roster
    .filter(a => a.at_work && a.role === 'executor')
    .sort((a, b) => a.signed_up_at.localeCompare(b.signed_up_at) || a.agent_id.localeCompare(b.agent_id))
}

/** The executors a max_agents of `max` cuts: the newest above it. */
export function cutByMax(atWork: JoinedAgent[], max: number): JoinedAgent[] {
  return atWork.slice(Math.max(0, max))
}

export function poolKey(agentType: string | undefined, model: string | undefined): string {
  return `${agentType || DEFAULT_AGENT_TYPE}\u0000${model ?? ''}`
}

/** How many agents at work run each agent type and model. */
export function atWorkByKey(atWork: JoinedAgent[]): Map<string, number> {
  const out = new Map<string, number>()
  for (const a of atWork) out.set(poolKey(a.agent_type, a.model), (out.get(poolKey(a.agent_type, a.model)) ?? 0) + 1)
  return out
}
