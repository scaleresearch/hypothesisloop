import { apiSend } from '@/lib/api/request'
import type { AcceptRequest, PlatformExperiment } from '@/types/api'

export function createDraft(projectID: string, goal: string, code: { repository?: string; repository_account?: string; branch?: string } = {}): Promise<PlatformExperiment> {
  return apiSend<PlatformExperiment>('POST', '/platform-experiments/drafts', { project_id: projectID, goal, ...code })
}

/**
 * Accepts a draft's proposal. Accepted, it is an open experiment with its coordinator and
 * team, which the platform starts by itself once its baseline is measured and declared.
 */
export async function acceptDraft(id: string, acceptance: AcceptRequest): Promise<void> {
  await apiSend<PlatformExperiment>('POST', `/platform-experiments/${id}/accept`, acceptance)
}

/** Keeps the platform from accepting a proposal by itself, so that the person can edit it. */
export async function holdDraft(id: string): Promise<void> {
  await apiSend<PlatformExperiment>('POST', `/platform-experiments/${id}/hold`)
}

/** Abandons a draft: it is closed and its planner's and baseline agent's machines released. */
export async function abandonDraft(id: string): Promise<void> {
  await apiSend('POST', `/platform-experiments/${id}/close`)
}
