import { apiSend, startPlatformExperiment } from '@/lib/api'
import type { MetricDefinition, PlatformExperiment } from '@/types'

/** One researcher of a proposal or of an acceptance. */
export interface ProposedAgent {
  flavor?: string
  model?: string
  hyperparameters?: Record<string, unknown>
}

/** What a draft's planner proposed, as the API stored it. */
export interface Proposal {
  // The experiment as its file would state it. The description is the brief every agent reads.
  experiment: {
    name: string
    description: string
    metrics: MetricDefinition[]
    report_interval_seconds: number
    stages?: PlatformExperiment['stages']
  }
  budget_accelerator_hours: number
  duration_hours: number
  agents: ProposedAgent[]
  baseline_job_id: string
  accelerator_type?: string
  metric_reason: string
  measurement: string
  out_of_scope?: string[]
  risks?: string[]
  questions?: string[]
}

export interface PlanningRequest {
  message: string
  by: { kind: string; id: string; name: string }
  at: string
}

/** A platform experiment made from a sentence: a draft, or what a draft became. */
export type Draft = PlatformExperiment & {
  goal?: string
  proposal?: Proposal
  // Absent while the planner works, on the first plan or on a revision.
  proposed_at?: string
  revisions?: PlanningRequest[]
}

/** What the person who accepts states themselves, whatever was proposed. */
export interface Acceptance {
  budget_accelerator_hours: number
  duration_hours: number
  agents: ProposedAgent[]
}

export const MAX_GOAL_LENGTH = 2000

export function createDraft(projectID: string, goal: string): Promise<Draft> {
  return apiSend<Draft>('POST', '/platform-experiments/drafts', { project_id: projectID, goal })
}

export function reviseDraft(id: string, message: string): Promise<Draft> {
  return apiSend<Draft>('POST', `/platform-experiments/${id}/revise`, { message })
}

/**
 * Accepts a draft's proposal and starts the experiment. Accepted, it is an open experiment with
 * its coordinator and researchers, which the platform also starts by itself within its sweep,
 * so a start that fails here is reported and loses nothing.
 */
export async function acceptAndStart(id: string, acceptance: Acceptance): Promise<{ startFailure?: string }> {
  await apiSend<Draft>('POST', `/platform-experiments/${id}/accept`, acceptance)
  try {
    await startPlatformExperiment(id)
  } catch (error) {
    return { startFailure: error instanceof Error ? error.message : String(error) }
  }
  return {}
}

/** What a draft is waiting for. */
export function draftState(draft: Draft): 'planning' | 'revising' | 'proposed' {
  if (draft.proposed_at) return 'proposed'
  return draft.proposal ? 'revising' : 'planning'
}

/** Abandons a draft: it is closed and its planner's machine released. */
export async function abandonDraft(id: string): Promise<void> {
  await apiSend('POST', `/platform-experiments/${id}/close`)
}
