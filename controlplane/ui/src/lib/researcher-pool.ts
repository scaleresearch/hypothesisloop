import { DEFAULT_AGENT_TYPE } from '@/lib/agent-types'
import type { JoinedAgent, ResearcherPoolEntry } from '@/types/api'

/** The researchers at work, oldest first: the agents the platform says are (`at_work`). */
export function researchersAtWork(roster: JoinedAgent[]): JoinedAgent[] {
  return roster
    .filter(a => a.at_work)
    .sort((a, b) => a.signed_up_at.localeCompare(b.signed_up_at) || a.agent_id.localeCompare(b.agent_id))
}

/** The researchers a max_agents of `max` cuts: the newest above it. */
export function cutByMax(atWork: JoinedAgent[], max: number): JoinedAgent[] {
  return atWork.slice(Math.max(0, max))
}

export function poolKey(agentType: string | undefined, model: string | undefined): string {
  return `${agentType || DEFAULT_AGENT_TYPE}\u0000${model ?? ''}`
}

/** How many researchers at work run each agent type and model. */
export function atWorkByKey(atWork: JoinedAgent[]): Map<string, number> {
  const out = new Map<string, number>()
  for (const a of atWork) out.set(poolKey(a.agent_type, a.model), (out.get(poolKey(a.agent_type, a.model)) ?? 0) + 1)
  return out
}

/** One entry of the pool as typed. */
export type PoolRow = { agent_type: string; model: string; count: string }

export function toRows(pool: ResearcherPoolEntry[] | undefined): PoolRow[] {
  return (pool ?? []).map(e => ({ agent_type: e.agent_type || DEFAULT_AGENT_TYPE, model: e.model ?? '', count: String(e.count) }))
}

export function toPool(rows: PoolRow[]): ResearcherPoolEntry[] {
  return rows.map(r => ({ agent_type: r.agent_type, count: Number(r.count), ...(r.model.trim() ? { model: r.model.trim() } : {}) }))
}
