import { createExperimentDraft } from '@/lib/api/drafts'
import { ApiError } from '@/lib/api/request'
import type { ExperimentDraft } from '@/types/drafts'
import type { StatusReason } from '@/types/platform-experiments'

export const MAX_GOAL_LENGTH = 2000

// The reasons of a draft being planned that move on by themselves. Any other holds planning up.
export const PLANNING_MOVES = new Set(['planning', 'revising', 'repository_creating'])

/** Why hosting is unavailable, when the API refused a create for it (503 with a status reason as its body). */
export function hostingRefusal(error: unknown): StatusReason | null {
  if (!(error instanceof ApiError) || error.status !== 503 || typeof error.body?.summary !== 'string') return null
  return error.body as unknown as StatusReason
}

/** Keeps a goal as a saved draft of the full form, its brief, for a person who sets the experiment up themselves. */
export function formDraftFromGoal(projectID: string, goal: string, repository: string): Promise<ExperimentDraft> {
  return createExperimentDraft(projectID, { form: { description: goal, repository }, step: '1', failure: '' })
}
