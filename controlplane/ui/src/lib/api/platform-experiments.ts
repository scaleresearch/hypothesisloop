import { apiCall, apiFetch, apiFetchPage, apiSend, withQuery } from '@/lib/api/request'
import type { Page } from '@/lib/api/request'
import type { CreatePlatformExperimentRequest, ExperimentFile, MetricHistory, PlatformExperiment, PlatformExperimentPatch, PlatformExperimentResults, RepositoryCheck, StagesStatus } from '@/types/api'

// Metric history of every job in a platform experiment over its own run, one series per job. The
// window reaches from the run's start to its end, or to now while it runs: each series comes back as
// about 500 points whatever the window, so a wider one costs resolution. Quarter hours keep the
// sampling grid still from one poll to the next.
export function fetchPlatformExperimentTimeseries(
  pe: PlatformExperiment,
  metricName: string,
): Promise<MetricHistory> {
  const query = new URLSearchParams({ metric_name: metricName })
  if (pe.started_at) {
    const end = Math.min(Date.now(), new Date(pe.ends_at as string).getTime())
    const quarters = Math.ceil((end - new Date(pe.started_at).getTime()) / 900_000)
    query.set('lookback_hours', String(Math.max(1, quarters) / 4))
    query.set('end', new Date(pe.ends_at as string).toISOString())
  }
  return apiFetch<MetricHistory>(`/platform-experiments/${pe.id}/metrics-timeseries?${query}`)
}

/** The reason the API gives for a repository that brings no experiment file. */
export const NO_EXPERIMENT_FILE = 'no_experiment_file'

/** The reason the API gives for an experiment file left as the experiment template holds it, its placeholders not yet filled in. */
export const EXPERIMENT_FILE_UNFILLED = 'experiment_file_unfilled'

/** The reason the API gives for an experiment file that places the experiment in another project, repository or branch. */
export const EXPERIMENT_FILE_ELSEWHERE = 'experiment_file_elsewhere'

/** The reason the API gives for an experiment file that cannot be used, its message naming the file, where it was read and why. */
export const INVALID_EXPERIMENT_FILE = 'invalid_experiment_file'

export function fetchExperimentFile(projectID: string, repository: string, branch: string): Promise<ExperimentFile> {
  return apiFetch<ExperimentFile>(withQuery(`/projects/${projectID}/experiment-file`, { repository, branch }))
}

// The create's own repository checks, creating nothing: what it refuses, the create refuses.
export function checkRepository(projectID: string, fields: { repository?: string; repository_account?: string }): Promise<RepositoryCheck> {
  return apiFetch<RepositoryCheck>(withQuery(`/projects/${projectID}/repository-check`, fields))
}

export function createPlatformExperiment(projectID: string, req: CreatePlatformExperimentRequest): Promise<PlatformExperiment> {
  return apiSend<PlatformExperiment>('POST', '/platform-experiments', { ...req, project_id: projectID })
}

interface PlatformExperimentsParams {
  /** The project's own. The ones staff shared with every account are listed with them, in every project. */
  project_id?: string
  /** true keeps only the shared ones, false leaves them out. */
  shared?: boolean
  status?: string
  /** Search over name/description. */
  q?: string
  limit?: number
  offset?: number
  /** "created_at" | "starts_at" | "name" | "status", optionally prefixed with "-" for descending. */
  sort?: string
}

export function fetchPlatformExperimentsPage(params: PlatformExperimentsParams): Promise<Page<PlatformExperiment>> {
  return apiFetchPage<PlatformExperiment>(withQuery('/platform-experiments', params))
}

export function fetchPlatformExperiment(id: string): Promise<PlatformExperiment> {
  return apiFetch<PlatformExperiment>(`/platform-experiments/${id}`)
}

export function fetchPlatformExperimentResults(id: string): Promise<PlatformExperimentResults> {
  return apiFetch<PlatformExperimentResults>(`/platform-experiments/${id}/results`)
}

/** Changes the fields the patch names and no others. */
export function updatePlatformExperiment(id: string, patch: PlatformExperimentPatch): Promise<PlatformExperiment> {
  return apiSend<PlatformExperiment>('PATCH', `/platform-experiments/${id}`, patch)
}

/** Closes an open or running platform experiment: its agents wind down and its running jobs stop. */
export function closePlatformExperiment(id: string): Promise<PlatformExperiment> {
  return apiSend<PlatformExperiment>('POST', `/platform-experiments/${id}/close`)
}

export async function removePlatformExperiment(id: string): Promise<void> {
  await apiCall('DELETE', `/platform-experiments/${id}`)
}

export function fetchStages(platformExpID: string): Promise<StagesStatus> {
  return apiFetch<StagesStatus>(`/platform-experiments/${platformExpID}/stages`)
}
