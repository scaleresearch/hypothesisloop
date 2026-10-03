import { ExperimentStatus } from '@/types'
import type { Experiment } from '@/types'
import type { ExperimentStats } from '@/lib/api'
import { relTime } from '@/lib/format'

const TERMINAL: string[] = [
  ExperimentStatus.COMPLETED,
  ExperimentStatus.FAILED,
  ExperimentStatus.EVICTED,
  ExperimentStatus.REJECTED,
  ExperimentStatus.CANCELLED,
]

// Job activity is separate from the verdict: a hypothesis whose jobs all ended is still open until
// its owner rules on it. jobsByHypothesis is the stats endpoint's group_by=hypothesis, which has a
// group only for a hypothesis with jobs, and one under the platform experiment's id for the
// baseline's jobs, which test no hypothesis.
export function hypothesisActivity(
  hypothesisCount: number,
  jobsByHypothesis: Record<string, ExperimentStats>,
  platformExperimentID: string,
): string {
  const groups = Object.entries(jobsByHypothesis).filter(([id]) => id !== platformExperimentID).map(([, stats]) => stats)
  const inFlight = groups.filter(g => Object.keys(g.by_status).some(s => !TERMINAL.includes(s))).length
  const untested = hypothesisCount - groups.length
  return [
    inFlight > 0 && `${inFlight} with jobs in flight`,
    untested > 0 && `${untested} without jobs`,
  ].filter(Boolean).join(' · ')
}

// A finding filed after the verdict is evidence the verdict did not weigh.
export function filedAfterVerdict(h: { status_changed_at: string | null }, findingAt: string): boolean {
  return h.status_changed_at != null && new Date(findingAt).getTime() > new Date(h.status_changed_at).getTime()
}

// The env settings that are not the same in every one of the jobs: what a hypothesis's runs varied.
export function variedEnvKeys(jobs: Experiment[]): string[] {
  return variedKeys(jobs.map(j => j.job?.env))
}

export function variedKeys(envs: Array<Record<string, string> | undefined>): string[] {
  const keys = Array.from(new Set(envs.flatMap(env => Object.keys(env ?? {})))).sort()
  return keys.filter(k => new Set(envs.map(env => env?.[k])).size > 1)
}

export function envSettings(job: Experiment, keys: string[]): string {
  return settings(job.job?.env, keys)
}

export function settings(env: Record<string, string> | undefined, keys: string[]): string {
  return keys.map(k => env?.[k] === undefined ? `${k} not set` : `${k}=${env[k]}`).join(' ')
}

// What a job is grouped with: its hypothesis, and for a baseline agent's job, which tests none,
// the other jobs on the objective.
function tested(job: Experiment): string {
  return job.hypothesis_id ?? ''
}

// Per hypothesis, the settings its jobs in the list do not share: what tells those rows apart.
export function variedEnvByHypothesis(jobs: Experiment[]): Map<string, string[]> {
  const groups = new Map<string, Experiment[]>()
  for (const j of jobs) groups.set(tested(j), [...(groups.get(tested(j)) ?? []), j])
  return new Map(Array.from(groups, ([id, group]) => [id, variedEnvKeys(group)]))
}

export function variedEnvSettings(job: Experiment, varied: Map<string, string[]>): string {
  return envSettings(job, varied.get(tested(job)) ?? [])
}
