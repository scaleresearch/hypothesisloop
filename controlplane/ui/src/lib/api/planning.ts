import { apiSend } from '@/lib/api/request'
import type { Draft, ProposedAgent } from '@/types/planning'

/** What the person who accepts states themselves, whatever was proposed. */
interface Acceptance {
  name: string
  budget_accelerator_hours: number
  duration_hours: number
  baseline_minutes: number
  agents: ProposedAgent[]
  // One for each of the proposal's questions, in their order; absent when it has none.
  answers?: string[]
}

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

/** Keeps the platform from accepting a proposal by itself, so that the person can edit it. */
export async function holdDraft(id: string): Promise<void> {
  await apiSend<Draft>('POST', `/platform-experiments/${id}/hold`)
}

/** Abandons a draft: it is closed and its planner's and baseline agent's machines released. */
export async function abandonDraft(id: string): Promise<void> {
  await apiSend('POST', `/platform-experiments/${id}/close`)
}
