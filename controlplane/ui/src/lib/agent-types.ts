import useSWR from 'swr'
import { fetchAgentTypes } from '@/lib/api/agent-types'
import { ApiError } from '@/lib/api/request'
import { keys } from '@/lib/keys'
import type { AgentRole, ListedAgentType } from '@/types/api'

/** The built-in researcher type an agent runs as when it names none. */
export const DEFAULT_AGENT_TYPE = 'generalist'

export const ROLES: { role: AgentRole; label: string; plural: string; builtin: string; description: string }[] = [
  { role: 'researcher', label: 'Researcher', plural: 'Researchers', builtin: DEFAULT_AGENT_TYPE, description: 'Competes in the run: registers hypotheses and tests them with jobs.' },
  { role: 'baseline', label: 'Baseline', plural: 'Baseline', builtin: 'baseline', description: 'Builds the harness and measures the control before the run starts.' },
  { role: 'planner', label: 'Planner', plural: 'Planners', builtin: 'planner', description: 'Turns a one-line goal into a draft experiment.' },
  { role: 'coordinator', label: 'Coordinator', plural: 'Coordinators', builtin: 'coordinator', description: 'Supervises a run and writes its summary.' },
]

/** The sentence every save and restore of a type says of the agents running it. */
export const APPLIES_LIVE = 'Changes apply in real time to running agents.'

export function roleOf(role: AgentRole) {
  const found = ROLES.find(r => r.role === role)
  if (!found) throw new Error(`unknown agent role ${role}`)
  return found
}

/** One line on what agents of the type do: the one the type states, a built-in's always. */
export function describeType(t: ListedAgentType): string {
  if (t.builtin || t.description) return t.description
  const by = t.created_by?.name ? `, made by ${t.created_by.name}` : ''
  return `Your org's own ${roleOf(t.role).label.toLowerCase()} type${by}.`
}

/** The name an agent type id reads as, or the id itself when the catalog does not hold it. */
export function typeName(id: string, catalog: ListedAgentType[] | undefined): string {
  return catalog?.find(t => t.id === id)?.name ?? id
}

/** Every agent type the org's agents may run as: the built-in ones and its own. */
export function useAgentTypes(orgID: string | undefined) {
  return useSWR(orgID ? keys.agentTypes(orgID) : null, () => fetchAgentTypes(orgID!))
}

/** The types a new researcher may be started as, built-in first. */
export function researcherTypes(catalog: ListedAgentType[] | undefined): ListedAgentType[] {
  return (catalog ?? []).filter(t => t.role === 'researcher' && !t.archived_at)
    .sort((a, b) => Number(b.builtin) - Number(a.builtin))
}

/** What each placeholder stands for, by its name. Which ones a prompt may hold the API lists on each type. */
export const PLACEHOLDER_DESCRIPTIONS: Record<string, string> = {
  rules: "The platform's rules every agent is bound by.",
  hl_commands: 'The commands of hl on the machine, with their flags.',
  workdir: 'The directory the agent works in.',
  platform_experiment_id: 'The id of the experiment the agent is in.',
  repository_url: 'Where the experiment keeps its code.',
  agent_id: "The agent's own id.",
  agent_type: 'The id of this agent type.',
  hyperparameters: 'The JSON object the agent was given to start from.',
}

const PLACEHOLDER = /\{\{([a-z_]+)\}\}/g

/** The placeholders a prompt holds, each once, in the order they first appear. */
export function placeholdersIn(prompt: string): string[] {
  return Array.from(new Set(Array.from(prompt.matchAll(PLACEHOLDER), m => m[1])))
}

/** Hyperparameters as typed: a JSON object, or the reason it is not one. */
export function parseHyperparameters(text: string): { value: Record<string, unknown> } | { problem: string } {
  if (text.trim() === '') return { value: {} }
  let value: unknown
  try {
    value = JSON.parse(text)
  } catch {
    return { problem: 'Not valid JSON.' }
  }
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return { problem: 'A JSON object, as {"lr": 0.001}.' }
  return { value: value as Record<string, unknown> }
}

const REFUSALS: Record<string, string> = {
  builtin_read_only: 'Built-in types are read-only. Duplicate it to change it.',
  archived: 'This type is archived and can no longer be edited.',
  version_conflict: 'Someone saved a newer version while you were editing.',
  agent_type_in_use: 'It is still in use.',
  agent_type_limit: 'The org has as many agent types as it may.',
  rate_limited: 'This type was changed as often as it may be in an hour.',
}

/** A refusal of a change to a type in words, its reason explained where it has one. */
export function said(e: unknown): string {
  if (e instanceof ApiError && e.reason && REFUSALS[e.reason]) return `${REFUSALS[e.reason]} ${e.message}`
  return e instanceof Error ? e.message : String(e)
}
