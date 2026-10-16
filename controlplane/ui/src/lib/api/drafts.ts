import { apiCall, apiFetch, apiSend } from '@/lib/api/request'
import type { ExperimentDraft } from '@/types/drafts'

type ExperimentDraftBody = Pick<ExperimentDraft, 'form' | 'step' | 'failure'>

export function fetchExperimentDrafts(projectID: string): Promise<ExperimentDraft[]> {
  return apiFetch<ExperimentDraft[]>(`/projects/${projectID}/experiment-drafts`)
}

export function fetchExperimentDraft(projectID: string, draftID: string): Promise<ExperimentDraft> {
  return apiFetch<ExperimentDraft>(`/projects/${projectID}/experiment-drafts/${draftID}`)
}

export function createExperimentDraft(projectID: string, body: ExperimentDraftBody): Promise<ExperimentDraft> {
  return apiSend<ExperimentDraft>('POST', `/projects/${projectID}/experiment-drafts`, body)
}

export function updateExperimentDraft(projectID: string, draftID: string, body: ExperimentDraftBody): Promise<ExperimentDraft> {
  return apiSend<ExperimentDraft>('PUT', `/projects/${projectID}/experiment-drafts/${draftID}`, body)
}

export async function deleteExperimentDraft(projectID: string, draftID: string): Promise<void> {
  await apiCall('DELETE', `/projects/${projectID}/experiment-drafts/${draftID}`)
}
