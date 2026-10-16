import { apiFetch, apiFetchPage, apiSend, withQuery } from '@/lib/api/request'
import type { Page } from '@/lib/api/request'
import type { Experiment, ExperimentStats, MetricDataPoint } from '@/types/jobs'

// Field names are sent verbatim as query params, so they must match the API's own names.
interface ExperimentsParams {
  project_id?: string
  status?: string
  agent_id?: string
  platform_experiment_id?: string
  /** Substring match against hypothesis and theory. */
  q?: string
  limit?: number
  offset?: number
  /** "created_at" | "updated_at" | "status", optionally prefixed with "-" for descending. */
  sort?: string
}

export function fetchExperimentsPage(params: ExperimentsParams): Promise<Page<Experiment>> {
  return apiFetchPage<Experiment>(withQuery('/experiments', params))
}

export function fetchExperiments(params: ExperimentsParams): Promise<Experiment[]> {
  return fetchExperimentsPage(params).then(p => p.items)
}

export function fetchExperimentStats(
  params: Omit<ExperimentsParams, 'limit' | 'offset' | 'sort'> & { group_by?: 'agent' | 'hypothesis' },
): Promise<ExperimentStats> {
  return apiFetch<ExperimentStats>(withQuery('/experiments/stats', params))
}

// The same counts over every org's jobs. Staff only: anyone else gets their own orgs' jobs.
export function fetchAllExperimentStats(): Promise<ExperimentStats> {
  return apiFetch<ExperimentStats>(`/experiments/stats`)
}

export function fetchExperiment(id: string): Promise<Experiment> {
  return apiFetch<Experiment>(`/experiments/${id}`)
}

export function fetchExperimentMetrics(id: string): Promise<MetricDataPoint[]> {
  return apiFetch<MetricDataPoint[]>(`/experiments/${id}/metrics`)
}

// The job's most recently reported stdout/stderr tail. For a failed job this is usually the
// only place the error itself appears — the record carries a status and a phase_detail reason,
// but the traceback lives here.
export function fetchExperimentLogs(id: string, n = 200): Promise<string[]> {
  return apiFetch<string[]>(`/experiments/${id}/logs?n=${n}`)
}

export async function cancelExperiment(id: string): Promise<void> {
  await apiSend('POST', `/experiments/${id}/cancel`)
}
