import { ExperimentStatus } from '@/types'
import type { Experiment, Hypothesis } from '@/types'

export type HypothesisProgress = 'finished' | 'in_flight' | 'pending'

const TERMINAL: ExperimentStatus[] = [
  ExperimentStatus.COMPLETED,
  ExperimentStatus.FAILED,
  ExperimentStatus.EVICTED,
  ExperimentStatus.PROMOTED,
]

// A hypothesis has no status of its own for "is it done running" — HypothesisStatus (open /
// confirmed / inconclusive / refuted) is an agent's verdict on the claim, not job progress. What
// the run-progress view actually wants is derived from the jobs testing the hypothesis: running
// beats queued beats terminal, since a hypothesis with one running and one finished job is still
// in flight.
export function hypothesisProgress(jobs: Experiment[]): HypothesisProgress {
  if (jobs.some(j => j.status === ExperimentStatus.RUNNING)) return 'in_flight'
  if (jobs.some(j => j.status === ExperimentStatus.QUEUED)) return 'pending'
  if (jobs.length > 0 && jobs.every(j => TERMINAL.includes(j.status))) return 'finished'
  return 'pending'
}

export interface HypothesisProgressCounts {
  finished: number
  in_flight: number
  pending: number
}

// Buckets every hypothesis under a platform experiment by run progress, grouping the PE's jobs
// by hypothesis_id — the only place that link lives (GET /hypotheses returns hypothesis metadata
// only, no job data; GET /hypotheses/{id} has jobs but only for one hypothesis at a time).
export function hypothesisProgressCounts(hypotheses: Hypothesis[], jobs: Experiment[]): HypothesisProgressCounts {
  const jobsByHypothesis = new Map<string, Experiment[]>()
  for (const job of jobs) {
    const list = jobsByHypothesis.get(job.hypothesis_id) ?? []
    list.push(job)
    jobsByHypothesis.set(job.hypothesis_id, list)
  }
  const counts: HypothesisProgressCounts = { finished: 0, in_flight: 0, pending: 0 }
  for (const h of hypotheses) {
    counts[hypothesisProgress(jobsByHypothesis.get(h.id) ?? [])]++
  }
  return counts
}
