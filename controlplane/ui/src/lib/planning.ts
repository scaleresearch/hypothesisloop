import { apiSend } from '@/lib/api'
import type { Actor, AgentActivity, MetricDefinition, PlatformExperiment } from '@/types'

/** One researcher of a proposal or of an acceptance. */
export interface ProposedAgent {
  flavor?: string
  model?: string
  // direction is the way to the goal the planner gave this researcher, in a sentence or two.
  hyperparameters?: Record<string, unknown> & { direction?: string }
}

/** What a draft's planner proposed, as the API stored it. */
export interface Proposal {
  // The experiment as its file would state it. The description is the brief every agent reads.
  experiment: {
    name: string
    description: string
    metrics: MetricDefinition[]
    // Each left out is the platform's own setting.
    report_interval_seconds?: number
    stages?: PlatformExperiment['stages']
  }
  budget_accelerator_hours: number
  // The run's length, from its start once the baseline is declared.
  duration_hours: number
  // The minutes measuring the baseline needs from the accept.
  baseline_minutes: number
  agents: ProposedAgent[]
  // The type the jobs are sized for: an accelerator, or CPU cores (units.CPU_TYPE) for work that needs none.
  accelerator_type: string
  // The image its jobs name: a job image the platform offers, by name, or the whole address of
  // another. Absent on a proposal stored before proposals named one.
  image?: string
  // What one node of its jobs asks for, when the planner stated it.
  cpu?: string
  memory?: string
  storage?: string
  metric_reason: string
  measurement: string
  // Why the proposed budget, duration and researchers are the least that reach the goal. Absent
  // on a proposal made before the planner was asked for it.
  sizing_reason?: string
  out_of_scope?: string[]
  risks?: string[]
  questions?: string[]
}

export interface PlanningRequest {
  message: string
  actor: Actor
  at: string
}

/** A platform experiment made from a sentence: a draft, or what a draft became. */
export type Draft = PlatformExperiment & {
  goal?: string
  // When the platform closes a draft nobody acts on: the first while its planner has answered
  // nothing, the second once it has and the answer waits for a person.
  planning_ends_at?: string
  proposal_expires_at?: string
  // What the draft's planner is doing now, while its machine is up.
  activity?: AgentActivity
  // On a draft whose planner answered: until when its machine is kept, so that a revision asked
  // for before then is answered with nothing to boot.
  planner_ends_at?: string
  proposal?: Proposal
  // Set while a proposal stands. Absent while the planner works, on the first plan or on a
  // revision, and on a draft it declined.
  proposed_at?: string
  // When and why the planner proposed nothing. The reason stays while a revision is planned.
  declined_at?: string
  declined_reason?: string
  revisions?: PlanningRequest[]
}

/** What the person who accepts states themselves, whatever was proposed. */
export interface Acceptance {
  name: string
  budget_accelerator_hours: number
  duration_hours: number
  baseline_minutes: number
  agents: ProposedAgent[]
}

export const MAX_GOAL_LENGTH = 2000

export function createDraft(projectID: string, goal: string, repository: { repository?: string; repository_account?: string } = {}): Promise<Draft> {
  return apiSend<Draft>('POST', '/platform-experiments/drafts', { project_id: projectID, goal, ...repository })
}

export function reviseDraft(id: string, message: string): Promise<Draft> {
  return apiSend<Draft>('POST', `/platform-experiments/${id}/revise`, { message })
}

/**
 * Accepts a draft's proposal. Accepted, it is an open experiment with its coordinator and
 * researchers, which the platform starts by itself once its baseline is measured and declared.
 */
export async function acceptDraft(id: string, acceptance: Acceptance): Promise<void> {
  await apiSend<Draft>('POST', `/platform-experiments/${id}/accept`, acceptance)
}

/** Abandons a draft: it is closed and its planner's machine released. */
export async function abandonDraft(id: string): Promise<void> {
  await apiSend('POST', `/platform-experiments/${id}/close`)
}
