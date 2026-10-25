import { apiSend } from '@/lib/api/request'
import type { PlatformExperiment } from '@/types/api'

export function createDraft(projectID: string, goal: string, code: { repository?: string; repository_account?: string; branch?: string } = {}): Promise<PlatformExperiment> {
  return apiSend<PlatformExperiment>('POST', '/platform-experiments/drafts', { project_id: projectID, goal, ...code })
}

/** Abandons a draft: it is closed and its machines released. */
export async function abandonDraft(id: string): Promise<void> {
  await apiSend('POST', `/platform-experiments/${id}/close`)
}
