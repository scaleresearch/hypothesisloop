import { DEFAULT_AGENT_TYPE, RESEARCHER_AGENT_TYPE } from '@/lib/agent-types'
import type { AgentPoolEntry, JoinedAgent } from '@/types/api'

/**
 * One entry of a team as typed: an agent type, how many of it and the model they run on (empty
 * for the one they inherit). The count is held as typed and parsed on submit, so it can be
 * half-written. Hyperparameters come from a proposal or the experiment and are sent back as they
 * are: the coordinator and planner set them, a person does not.
 */
export type TeamRow = { agent_type: string; count: string; model: string; hyperparameters?: Record<string, unknown> }

/** What a new experiment's team starts as: executors of the default type and one researcher. */
export function defaultTeam(executors: number): TeamRow[] {
  return [
    { agent_type: DEFAULT_AGENT_TYPE, count: String(executors), model: '' },
    { agent_type: RESEARCHER_AGENT_TYPE, count: '1', model: '' },
  ]
}

export function toTeamRows(entries: AgentPoolEntry[] | undefined): TeamRow[] {
  return (entries ?? []).map(e => ({
    agent_type: e.agent_type || DEFAULT_AGENT_TYPE,
    count: String(e.count),
    model: e.model ?? '',
    ...(e.hyperparameters && Object.keys(e.hyperparameters).length > 0 ? { hyperparameters: e.hyperparameters } : {}),
  }))
}

/** Rows a draft kept, which before held hyperparameters as JSON text: read as the object it names, else none. */
export function keptRows(rows: unknown): TeamRow[] {
  if (!Array.isArray(rows)) return []
  return rows.map(r => {
    const { hyperparameters, ...rest } = r as Omit<TeamRow, 'hyperparameters'> & { hyperparameters?: unknown }
    let h = hyperparameters
    if (typeof h === 'string') {
      try { h = h.trim() ? JSON.parse(h) : undefined } catch { h = undefined }
    }
    const object = typeof h === 'object' && h !== null && !Array.isArray(h) && Object.keys(h).length > 0
    return { ...rest, ...(object ? { hyperparameters: h as Record<string, unknown> } : {}) }
  })
}

/** What an entry starts from in words: its direction where that is text, else its hyperparameters as JSON. Empty for none. */
export function startsFrom(row: TeamRow): string {
  const h = row.hyperparameters
  if (!h || Object.keys(h).length === 0) return ''
  return typeof h.direction === 'string' ? h.direction : JSON.stringify(h)
}

/** The index of the earlier row of row i's agent type and model, or -1. The API takes one entry of each. */
export function sameEntry(rows: TeamRow[], i: number): number {
  const key = poolKey(rows[i].agent_type, rows[i].model.trim())
  return rows.findIndex((r, j) => j < i && poolKey(r.agent_type, r.model.trim()) === key)
}

/** How many executors the rows hold: every agent but a researcher, as the API counts them. */
export function executorCount(rows: TeamRow[]): number {
  return rows.filter(r => r.agent_type !== RESEARCHER_AGENT_TYPE).reduce((n, r) => n + (Number(r.count) || 0), 0)
}

/**
 * The rows as the agents entries the API takes, or the first problem: a count that is no whole
 * number of at least one, or, unless perAgent, a second entry of one type and model.
 */
export function toTeam(rows: TeamRow[], { perAgent = false } = {}): { agents: AgentPoolEntry[] } | { problem: string } {
  const agents: AgentPoolEntry[] = []
  for (let i = 0; i < rows.length; i++) {
    const r = rows[i]
    const count = Number(r.count)
    if (r.count.trim() === '' || !Number.isInteger(count) || count < 1) return { problem: `Team entry ${i + 1}: the count is a whole number of at least 1.` }
    const same = perAgent ? -1 : sameEntry(rows, i)
    if (same >= 0) return { problem: `Team entry ${i + 1}: entry ${same + 1} has this type and model. Raise its count instead.` }
    agents.push({
      agent_type: r.agent_type,
      count,
      ...(r.model.trim() ? { model: r.model.trim() } : {}),
      ...(r.hyperparameters && Object.keys(r.hyperparameters).length > 0 ? { hyperparameters: r.hyperparameters } : {}),
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
