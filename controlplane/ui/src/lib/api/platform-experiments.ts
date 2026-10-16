import { apiCall, apiFetch, apiFetchPage, apiSend, withQuery } from '@/lib/api/request'
import type { Page } from '@/lib/api/request'
import type { AgentMetricSeries, ExperimentFile, MetricDefinition, PlatformExperiment, PlatformExperimentResults, RepositoryCheck, ResearcherPoolEntry, Stage, StagesStatus } from '@/types/platform-experiments'

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

export interface CreatePlatformExperimentRequest {
  name: string
  description: string
  budget_accelerator_hours: number
  /** How many researchers may be at work at once. */
  max_agents: number
  /** Who the platform starts and keeps at work, at most max_agents in all. Optional. */
  researchers?: ResearcherPoolEntry[]
  /** Accelerators-in-flight (SUBMITTED+RUNNING) this experiment may hold at once. Omitted uses
   *  the platform default (quota.default_max_concurrent_accelerators). */
  max_concurrent_accelerators?: number
  metrics: MetricDefinition[]
  /** Omitted is the platform's default. */
  report_interval_seconds?: number
  /** Elimination ladder; omitted means the platform default. */
  stages?: Stage[]
  /** When its baseline begins to be measured. Omitted, the create. */
  starts_at?: string
  /** How long the run lasts from its start, once the baseline is declared. */
  duration_hours: number
  /** owner/name of a repository a connected GitHub account covers. Read at creation only. */
  repository?: string
  /** The image hosted agents and the coordinator run; omitted means the platform's. Read at creation only. */
  agent_image?: string
  /** true has the platform run the coordinator. It needs `repository`. Read at creation only. */
  coordinator_hosted?: boolean
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

/** The fields of a platform experiment that can change after it is created. */
export type PlatformExperimentPatch = Partial<Pick<CreatePlatformExperimentRequest,
  'name' | 'description' | 'budget_accelerator_hours' | 'max_agents' | 'researchers' | 'max_concurrent_accelerators' | 'metrics' |
  'report_interval_seconds' | 'starts_at' | 'duration_hours'>>

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
