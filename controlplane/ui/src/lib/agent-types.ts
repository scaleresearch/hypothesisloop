import useSWR from 'swr'
import { fetchAgentTypes } from '@/lib/api/agent-types'
import { ApiError } from '@/lib/api/request'
import { keys } from '@/lib/keys'
import type { AgentRole, AgentTypeSettings, ListedAgentType } from '@/types/api'

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

// ---------------------------------------------------------------------------
// Prompt placeholders, as domain.AgentPlaceholders and domain.ValidatePrompt hold them
// ---------------------------------------------------------------------------

interface Placeholder {
  name: string
  description: string
  /** Who fills it: the platform when it serves the prompt, or the harness on the machine. */
  filledBy: 'platform' | 'harness'
}

const PLACEHOLDERS: Placeholder[] = [
  { name: 'rules', description: "The platform's rules every agent is bound by.", filledBy: 'platform' },
  { name: 'hl_commands', description: 'The commands of hl on the machine, with their flags.', filledBy: 'harness' },
  { name: 'workdir', description: 'The directory the agent works in.', filledBy: 'harness' },
  { name: 'platform_experiment_id', description: 'The id of the experiment the agent is in.', filledBy: 'platform' },
  { name: 'repository_url', description: 'Where the experiment keeps its code.', filledBy: 'platform' },
  { name: 'agent_id', description: "The agent's own id.", filledBy: 'platform' },
  { name: 'agent_type', description: 'The id of this agent type.', filledBy: 'platform' },
  { name: 'hyperparameters', description: 'The JSON object the agent was given to start from.', filledBy: 'platform' },
]

export const REQUIRED_PLACEHOLDERS = ['rules', 'hl_commands']

/** The placeholders a prompt of role may hold. A coordinator is no agent of the run, so it has no agent id. */
export function placeholdersOf(role: AgentRole): Placeholder[] {
  const names = ['rules', 'hl_commands', 'workdir', 'platform_experiment_id', 'repository_url']
  if (role === 'researcher') names.push('agent_id', 'agent_type', 'hyperparameters')
  if (role === 'baseline' || role === 'planner') names.push('agent_id')
  return names.map(n => PLACEHOLDERS.find(p => p.name === n)!)
}

export const MAX_PROMPT_BYTES = 64 << 10
const MAX_NAME_LENGTH = 80
const MAX_DESCRIPTION_LENGTH = 200

const PLACEHOLDER = /\{\{([a-z_]+)\}\}/g
const STRAY = /\{\{|\}\}/

function bytes(s: string): number {
  return new TextEncoder().encode(s).length
}

/** The placeholders a prompt holds, each once, in the order they first appear. */
export function placeholdersIn(prompt: string): string[] {
  return Array.from(new Set(Array.from(prompt.matchAll(PLACEHOLDER), m => m[1])))
}

/** Everything the API would refuse in a prompt of role, each problem once, so all are shown at once. */
export function promptProblems(role: AgentRole, prompt: string): string[] {
  if (prompt.trim() === '') return ['The prompt is empty.']
  const problems: string[] = []
  const size = bytes(prompt)
  if (size > MAX_PROMPT_BYTES) problems.push(`The prompt is ${size} bytes, over the limit of ${MAX_PROMPT_BYTES}.`)
  const allowed = placeholdersOf(role).map(p => p.name)
  const found = placeholdersIn(prompt)
  for (const name of found.filter(n => !allowed.includes(n))) {
    problems.push(`{{${name}}} has no value in a ${role} prompt.`)
  }
  if (STRAY.test(prompt.replace(PLACEHOLDER, ''))) {
    problems.push('A {{ or }} that is no placeholder. A placeholder is {{name}} in lowercase letters and _.')
  }
  for (const name of REQUIRED_PLACEHOLDERS.filter(n => !found.includes(n))) {
    problems.push(`{{${name}}} is required.`)
  }
  return problems
}

export function nameProblem(name: string): string | null {
  const length = Array.from(name.trim()).length
  return length === 0 || length > MAX_NAME_LENGTH ? `A name is 1 to ${MAX_NAME_LENGTH} characters.` : null
}

export function descriptionProblem(description: string): string | null {
  return Array.from(description.trim()).length > MAX_DESCRIPTION_LENGTH ? `A description is at most ${MAX_DESCRIPTION_LENGTH} characters.` : null
}

// ---------------------------------------------------------------------------
// Settings, as domain.AgentTypeSettings.Validate holds them
// ---------------------------------------------------------------------------

export const EFFORTS = ['low', 'medium', 'high', 'xhigh', 'max'] as const
export const MAX_SUBAGENTS = 10
const MAX_HYPERPARAMETERS_BYTES = 4 << 10
const MODEL = /^[A-Za-z0-9][A-Za-z0-9._:[\]-]*$/
const MAX_MODEL_LENGTH = 128

export function modelProblem(model: string): string | null {
  if (model === '') return null
  return model.length > MAX_MODEL_LENGTH || !MODEL.test(model)
    ? `At most ${MAX_MODEL_LENGTH} letters, digits and . _ : - [ ]`
    : null
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
  const size = bytes(JSON.stringify(value))
  if (size > MAX_HYPERPARAMETERS_BYTES) return { problem: `${size} bytes as JSON, over the limit of ${MAX_HYPERPARAMETERS_BYTES}.` }
  return { value: value as Record<string, unknown> }
}

/** The problems of each field of settings, by field. */
export function settingsProblems(s: AgentTypeSettings): Partial<Record<keyof AgentTypeSettings, string>> {
  const out: Partial<Record<keyof AgentTypeSettings, string>> = {}
  const model = modelProblem(s.model ?? '')
  if (model) out.model = model
  if (!Number.isInteger(s.max_subagents) || s.max_subagents < 1 || s.max_subagents > MAX_SUBAGENTS) out.max_subagents = `1 to ${MAX_SUBAGENTS}.`
  if (!(EFFORTS as readonly string[]).includes(s.effort)) out.effort = `One of ${EFFORTS.join(', ')}.`
  return out
}

const REFUSALS: Record<string, string> = {
  builtin_read_only: 'Built-in types are read-only. Duplicate it to change it.',
  archived: 'This type is archived and can no longer be edited.',
  version_conflict: 'Someone saved a newer version while you were editing.',
  agent_type_in_use: 'It is still in use.',
  agent_type_limit: 'The org has as many agent types as it may.',
  edit_rate_limited: 'This type was changed as often as it may be in an hour.',
}

/** A refusal of a change to a type in words, its reason explained where it has one. */
export function said(e: unknown): string {
  if (e instanceof ApiError && e.reason && REFUSALS[e.reason]) return `${REFUSALS[e.reason]} ${e.message}`
  return e instanceof Error ? e.message : String(e)
}
