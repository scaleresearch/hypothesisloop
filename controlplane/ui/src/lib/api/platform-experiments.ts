import { apiCall, apiFetch, apiFetchPage, apiSend, withQuery } from '@/lib/api/request'
import type { Page } from '@/lib/api/request'
import type { AgentMetricSeries, ExperimentFile, ExperimentSpec, MetricDefinition, PlatformExperiment, PlatformExperimentResults, RepositoryCheck, ResearcherPoolEntry, Stage, StagesStatus } from '@/types/platform-experiments'

// Metric history of every job in a platform experiment over its own run, one series per job. The
// window reaches from the run's start to its end, or to now while it runs: each series comes back as
// about 500 points whatever the window, so a wider one costs resolution. Quarter hours keep the
// sampling grid still from one poll to the next.
export function fetchPlatformExperimentTimeseries(
  pe: PlatformExperiment,
  metricName: string,
): Promise<{ series: AgentMetricSeries[] }> {
  const query = new URLSearchParams({ metric_name: metricName })
  if (pe.started_at) {
    const end = Math.min(Date.now(), new Date(pe.ends_at as string).getTime())
    const quarters = Math.ceil((end - new Date(pe.started_at).getTime()) / 900_000)
    query.set('lookback_hours', String(Math.max(1, quarters) / 4))
    query.set('end', new Date(pe.ends_at as string).toISOString())
  }
  return apiFetch<{ series: AgentMetricSeries[] }>(`/platform-experiments/${pe.id}/metrics-timeseries?${query}`)
}

export interface CreatePlatformExperimentRequest extends ExperimentSpec {
  /** owner/name of the repository it works on. */
  repository?: string
  /** The connected account a new repository is created on, without repository. */
  repository_account?: string
  /** A closed platform experiment of the project this one carries on. */
  continues?: string
  /** When its baseline begins to be measured. Omitted, the create. */
  starts_at?: string
}

/** The reason the API gives for a repository that brings no experiment file. */
export const NO_EXPERIMENT_FILE = 'no_experiment_file'

export function fetchExperimentFile(projectID: string, repository: string): Promise<ExperimentFile> {
  return apiFetch<ExperimentFile>(withQuery(`/projects/${projectID}/experiment-file`, { repository }))
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

/** The fields of a platform experiment that can change after it is created. The goal changes alone. */
export type PlatformExperimentPatch = Partial<Pick<CreatePlatformExperimentRequest,
  'name' | 'description' | 'budget_accelerator_hours' | 'max_agents' | 'researchers' | 'max_concurrent_accelerators' | 'metrics' |
  'report_interval_seconds' | 'starts_at' | 'duration_hours'> & { goal: string }>

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
