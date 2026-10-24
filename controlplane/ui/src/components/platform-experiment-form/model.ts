import { defaultTeam } from '@/lib/team'
import type { TeamRow } from '@/lib/team'
import type { MetricDefinition, QuotaTier, Stage } from '@/types/api'
import type { EditableField } from '@/lib/editable'

// What a new experiment starts from. The stage ladder and the report interval are left out, so
// the platform applies its own (stages.default, scheduler.default_report_interval_seconds).
export const NEW_EXPERIMENT = { budget_accelerator_hours: 10, duration_hours: 48, executors: 2 }

/** The team a new experiment starts composed with. */
export const NEW_TEAM: TeamRow[] = defaultTeam(NEW_EXPERIMENT.executors)

// Numbers are held as typed, for the reason StageDraft gives, and parsed on submit.
export interface FormState {
  name: string
  description: string
  budget_accelerator_hours: string
  // The most executors at work at once, sent as max_agents.
  max_agents: string
  // The team the platform starts and keeps at work, sent as agents.
  agents: TeamRow[]
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
  // Empty is the platform's default.
  default_quota_tier: QuotaTier | ''
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

export const ONE_STAGE: StageDraft[] = [{ length_pct: '100', evict_pct: '0', max_job_hours: '' }]

export function toDrafts(stages: Stage[]): StageDraft[] {
  return stages.map(s => ({
    length_pct: String(s.length_pct),
    evict_pct: String(s.evict_pct),
    max_job_hours: s.max_job_hours ? String(s.max_job_hours) : '',
  }))
}

// An empty max_job_hours means "no limit" and parses to 0.
export function toStages(drafts: StageDraft[]): Stage[] {
  return drafts.map(s => ({
    length_pct: Number(s.length_pct),
    evict_pct: Number(s.evict_pct),
    max_job_hours: s.max_job_hours.trim() === '' ? 0 : Number(s.max_job_hours),
  }))
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
