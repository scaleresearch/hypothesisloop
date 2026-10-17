import useSWR from 'swr'
import { fetchFindings, fetchHypothesisStats } from '@/lib/api/hypotheses'
import { fetchExperimentsPage, fetchExperimentStats } from '@/lib/api/jobs'
import { fetchPlatformExperiment, fetchPlatformExperimentResults, fetchPlatformExperimentTimeseries, fetchStages } from '@/lib/api/platform-experiments'
import { fetchPlatformExperimentQuotas } from '@/lib/api/quotas'
import { keys } from '@/lib/keys'
import { POLL } from '@/lib/poll'
import type { AgentMetricSeries, AgentQuotaView, PlatformExperiment, StagesStatus } from '@/types/api'

// How many of the newest jobs the page reads, for the jobs table and the metrics picker.
const RECENT_JOBS = 20

/** Every read of an experiment's page, and the names of those that failed. */
export function useOverview(id: string) {
  const experiment = useSWR<PlatformExperiment>(
    keys.experiment(id),
    () => fetchPlatformExperiment(id),
  )
  const pe = experiment.data

  const { data: quotas, error: quotasError } = useSWR<AgentQuotaView[]>(
    keys.quotas(id),
    () => fetchPlatformExperimentQuotas(id),
    { refreshInterval: POLL.standing },
  )

  // Each list below is asked for inside the platform experiment's own project, so it waits for
  // the experiment itself. The newest jobs only, for the jobs table and the live metrics picker.
  const { data: experimentsPage, error: experimentsError } = useSWR(
    pe ? keys.experimentJobs(id) : null,
    () => fetchExperimentsPage({ project_id: pe!.project_id, platform_experiment_id: id, sort: '-created_at', limit: RECENT_JOBS }),
  )

  // Asked for by status: a job that outlived the newest page of jobs is still running.
  const { data: runningPage, error: runningError } = useSWR(
    pe ? keys.runningJobs(id) : null,
    () => fetchExperimentsPage({ project_id: pe!.project_id, platform_experiment_id: id, status: 'RUNNING', sort: '-created_at' }),
  )

  // Whole-experiment job counts, grouped server-side. The job list above is one bounded page,
  // so tallying it would report the page's shape under a label that reads as the experiment's.
  const { data: stats, error: statsError } = useSWR(
    pe ? keys.jobStatsByAgent(id) : null,
    () => fetchExperimentStats({ project_id: pe!.project_id, platform_experiment_id: id, group_by: 'agent' }),
  )
  const { data: hypothesisStats, error: hypothesisStatsError } = useSWR(
    pe ? keys.jobStatsByHypothesis(id) : null,
    () => fetchExperimentStats({ project_id: pe!.project_id, platform_experiment_id: id, group_by: 'hypothesis' }),
  )

  const { data: stages, error: stagesError } = useSWR<StagesStatus>(
    keys.stages(id),
    () => fetchStages(id),
  )

  const { data: verdicts, error: hypothesesError } = useSWR(
    keys.hypothesisStats(id),
    () => fetchHypothesisStats(id),
  )

  const { data: findings, error: findingsError } = useSWR(
    keys.findings(id),
    () => fetchFindings(id),
  )

  const { data: results, error: resultsError } = useSWR(
    keys.results(id),
    () => fetchPlatformExperimentResults(id),
    { refreshInterval: POLL.standing },
  )

  // The history of every declared metric, for the chart. The heaviest read on the page, so it
  // polls slower than the rest.
  const metricKeys = (pe?.metrics ?? []).map(m => m.key)
  const { data: metricSeries, error: metricSeriesError } = useSWR<{ key: string; series: AgentMetricSeries[] }[]>(
    pe && metricKeys.length > 0 ? keys.metricSeries(id, metricKeys.join(','), pe.started_at, pe.ends_at) : null,
    () => Promise.all(metricKeys.map(k =>
      fetchPlatformExperimentTimeseries(pe!, k).then(r => ({ key: k, series: r.series })),
    )),
    { refreshInterval: POLL.slow },
  )

  // Every number on the page is derived by joining these reads together, so a failed one must
  // not pass through as an empty array: an unreachable quota service would render as "no agents
  // signed up", which is a different answer, not a missing one.
  const failed = [
    quotasError && 'quotas',
    (experimentsError || runningError) && 'jobs',
    stagesError && 'stage status',
    (hypothesesError || findingsError) && 'hypotheses',
    metricSeriesError && 'metric history',
    resultsError && 'results',
    (statsError || hypothesisStatsError) && 'job counts',
  ].filter(Boolean) as string[]

  return {
    experiment,
    quotas,
    experiments: experimentsPage?.items,
    running: runningPage?.items,
    stats,
    hypothesisStats,
    stages,
    verdicts,
    findings,
    results,
    metricSeries,
    failed,
  }
}
