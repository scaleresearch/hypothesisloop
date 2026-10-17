import { DEFAULT_AGENT_TYPE } from '@/lib/agent-types'
import type { MetricDefinition, ResearcherPoolEntry, Stage } from '@/types/api'
import type { EditableField } from '@/lib/editable'

// What a new experiment starts from. The stage ladder and the report interval are left out, so
// the platform applies its own (stages.default, scheduler.default_report_interval_seconds).
export const NEW_EXPERIMENT = { budget_accelerator_hours: 10, duration_hours: 48, researchers: 2 }

// Numbers are held as typed, for the reason StageDraft gives, and parsed on submit.
export interface FormState {
  name: string
  description: string
  budget_accelerator_hours: string
  // The most researchers at work at once, sent as max_agents.
  max_agents: string
  // Who the platform starts and keeps at work. null is max_agents generalists on the default model.
  starting_agents: StartingAgentDraft[] | null
  // Empty is the platform's default (quota.default_max_concurrent_accelerators).
  max_concurrent_accelerators: string
  // Empty is no start time: the platform starts it once its baseline is declared.
  starts_at: string
  duration_hours: string
  metrics: MetricDefinition[]
  // Empty is the platform's default.
  report_interval_seconds: string
  // null is the platform's default ladder.
  stages: StageDraft[] | null
  repository: string
  // The branch a named repository is worked on at.
  branch: string
  agent_image: string
  // Empty is the platform's coordinator default.
  coordinator_model: string
  // Empty is the platform's default: guaranteed or burst_only.
  default_quota_tier: string
}

export type SetField = <K extends keyof FormState>(field: K, value: FormState[K]) => void

/** What every step is handed: the form, its setter, and whether it edits an experiment that may already run. */
export interface StepProps {
  form: FormState
  set: SetField
  isEdit: boolean
  /** Whether an edit leaves field as it is: the API does not list it in the experiment's editable. */
  locked: (field: EditableField) => boolean
}

// StageDraft holds what's typed, not what's parsed. A number-typed field cannot represent a
// half-edited value: clearing it yields "", Number("") is 0, and the input snaps back to 0 before
// the next digit is typed. Strings let a field be empty while editing; the ladder is parsed once,
// on submit.
export type StageDraft = { length_pct: string; evict_pct: string; max_job_hours: string }

// One row of the starting agents, held as typed, for the reason StageDraft gives.
// Hyperparameters come only from a repository's experiment file, and are sent as it states them.
export type StartingAgentDraft = { agent_type: string; count: string; model: string; hyperparameters?: Record<string, unknown> }

export const ONE_STARTING_AGENT: StartingAgentDraft = { agent_type: DEFAULT_AGENT_TYPE, count: '1', model: '' }

/** The researchers a new experiment is sent with: the rows given, or max_agents generalists. */
export function toResearchers(rows: StartingAgentDraft[] | null, maxAgents: number): ResearcherPoolEntry[] {
  if (!rows) return [{ agent_type: DEFAULT_AGENT_TYPE, count: maxAgents }]
  return rows.map(r => ({
    agent_type: r.agent_type,
    count: Number(r.count),
    ...(r.model.trim() ? { model: r.model.trim() } : {}),
    ...(r.hyperparameters ? { hyperparameters: r.hyperparameters } : {}),
  }))
}

/** The rows of the researchers a spec states. */
export function toStartingAgents(researchers: ResearcherPoolEntry[]): StartingAgentDraft[] {
  return researchers.map(r => ({
    agent_type: r.agent_type || DEFAULT_AGENT_TYPE,
    count: String(r.count),
    model: r.model ?? '',
    ...(r.hyperparameters ? { hyperparameters: r.hyperparameters } : {}),
  }))
}

/** Mirrors what the API holds starting agents to, so the form says it before the API does. */
export function startingAgentsError(rows: StartingAgentDraft[] | null, maxAgents: number): string | null {
  if (!rows) return null
  if (rows.some(r => !Number.isInteger(Number(r.count)) || Number(r.count) < 1)) return 'Every starting agent row needs a count of at least 1.'
  const keys = rows.map(r => `${r.agent_type}/${r.model.trim()}`)
  if (new Set(keys).size !== keys.length) return 'Two starting agent rows name the same type and model: give one row their count.'
  const total = rows.reduce((sum, r) => sum + Number(r.count), 0)
  if (total > maxAgents) return `${total} starting agents, more than the ${maxAgents} researchers allowed at once.`
  return null
}

export const ONE_STAGE: StageDraft[] = [{ length_pct: '100', evict_pct: '0', max_job_hours: '' }]

export const MAX_STAGES = 8

export function toDrafts(stages: Stage[]): StageDraft[] {
  return stages.map(s => ({
    length_pct: String(s.length_pct),
    evict_pct: String(s.evict_pct),
    max_job_hours: s.max_job_hours ? String(s.max_job_hours) : '',
  }))
}

// An empty max_job_hours means "no limit" and parses to 0; an empty length/evict is a genuine
// gap that stagesError reports.
export function toStages(drafts: StageDraft[]): Stage[] {
  return drafts.map(s => ({
    length_pct: Number(s.length_pct),
    evict_pct: Number(s.evict_pct),
    max_job_hours: s.max_job_hours.trim() === '' ? 0 : Number(s.max_job_hours),
  }))
}

// stagesError mirrors domain.ValidateStages so the form rejects a bad ladder before the API does.
export function stagesError(stages: StageDraft[]): string | null {
  if (stages.length === 0) return 'Add at least one stage.'
  if (stages.length > MAX_STAGES) return 'At most 8 stages.'
  if (stages.some(s => s.length_pct.trim() === '' || s.evict_pct.trim() === '')) return 'Every stage needs a length % and an evict %.'
  if (stages.some(s => !Number.isFinite(Number(s.length_pct)) || !Number.isFinite(Number(s.evict_pct)))) return 'Stage percentages must be numbers.'
  const total = stages.reduce((s, x) => s + Number(x.length_pct), 0)
  if (stages.some(s => Number(s.length_pct) <= 0)) return 'Every stage must be longer than 0%.'
  if (stages.some(s => Number(s.evict_pct) < 0 || Number(s.evict_pct) >= 100)) return 'Evict % must be between 0 and 99.'
  if (stages.some(s => s.max_job_hours.trim() !== '' && (!Number.isFinite(Number(s.max_job_hours)) || Number(s.max_job_hours) < 0))) return 'Max job hours must be a positive number, or empty for no limit.'
  if (total < 99.999 || total > 100.001) return `Stage lengths must add up to 100% (now ${round2(total)}%).`
  if (Number(stages[stages.length - 1].evict_pct) !== 0) return 'The final stage evicts 0%: nothing follows it.'
  return null
}

// Length totals are summed from free-typed decimals; 33.33+33.33+33.34 must read as 100, not
// 99.99999999999999.
export function round2(n: number): number {
  return Math.round(n * 100) / 100
}

export const STEPS = [
  { id: '1', label: '1. What' },
  { id: '2', label: '2. Measure' },
  { id: '3', label: '3. Budget and rules' },
] as const

export type StepID = typeof STEPS[number]['id']

/** One thing the API refused a create for. A refusal of several checks lists each in problems. */
export interface Problem {
  reason?: string
  message: string
  field?: string
}

/** The step a problem of the create concerns, by the API's reason: the repository, the metrics, or the rest, on Budget and rules. */
export function refusedStep(reason: string | undefined): StepID {
  if (reason && /repositor|github|installation|experiment_file|unreadable_file|invalid_branch|branch_not_found/.test(reason)) return '1'
  if (reason && /metric/.test(reason)) return '2'
  return '3'
}

export const LOCKED = 'Locked once running.'
