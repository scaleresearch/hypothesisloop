import type { JoinedAgent, ResearcherPoolEntry } from '@/types'
import { DEFAULT_AGENT_TYPE } from '@/lib/agent-types'

/**
 * The researchers at work, oldest first, as the platform counts them against max_agents: agents of
 * role competitor that have not finished, were not cut, were not replaced and still have a machine.
 * `cut` is the agents a stage boundary cut, which the roster does not say.
 */
export function researchersAtWork(roster: JoinedAgent[], cut: Set<string>): JoinedAgent[] {
  return roster
    .filter(a => a.role === 'competitor' && !a.finished_at && !a.replaced_by && !a.cut_reason && !cut.has(a.agent_id)
      && !(a.hosted && a.instance?.stopped))
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
  for (const a of atWork) out.set(poolKey(a.flavor, a.model), (out.get(poolKey(a.flavor, a.model)) ?? 0) + 1)
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

/**
 * What the API would refuse in a change of max_agents and the pool, `atWork` being the researchers
 * still at work once the lowered max cut its newest. `rows` is null for a change that leaves the pool alone.
 */
export function poolProblem(max: number, rows: PoolRow[] | null, atWork: JoinedAgent[], typeName: (id: string) => string): string | null {
  if (!Number.isInteger(max) || max < 1) return 'Max researchers is a whole number of at least 1.'
  if (!rows) return null
  if (rows.some(r => !Number.isInteger(Number(r.count)) || Number(r.count) < 1)) return 'Every entry needs a count of at least 1.'
  const keys = rows.map(r => poolKey(r.agent_type, r.model.trim()))
  if (new Set(keys).size !== keys.length) return 'Two entries name the same type and model: give one entry their count.'
  const total = rows.reduce((s, r) => s + Number(r.count), 0)
  if (total > max) return `The pool wants ${total} researchers, more than the max of ${max}.`
  const working = atWorkByKey(atWork)
  for (const r of rows) {
    const n = working.get(poolKey(r.agent_type, r.model.trim())) ?? 0
    if (Number(r.count) < n) {
      return `${n} ${typeName(r.agent_type)} researchers${r.model.trim() ? ` on ${r.model.trim()}` : ''} are at work: keep at least ${n}. Lower max researchers to cut the newest.`
    }
  }
  return null
}
