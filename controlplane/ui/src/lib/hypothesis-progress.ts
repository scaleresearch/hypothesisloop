import { ExperimentStatus } from '@/types'
import type { ExperimentStats } from '@/lib/api'

const TERMINAL: string[] = [
  ExperimentStatus.COMPLETED,
  ExperimentStatus.FAILED,
  ExperimentStatus.EVICTED,
  ExperimentStatus.REJECTED,
]

export interface HypothesisProgressCounts {
  finished: number
  in_flight: number
  pending: number
}

// A hypothesis has no status of its own for "is it done running": HypothesisStatus is an agent's
// verdict on the claim. Run progress comes from the statuses of the jobs testing it, where running
// beats queued beats terminal. jobsByHypothesis is the stats endpoint's group_by=hypothesis, which
// has a group only for a hypothesis with jobs, so the rest of hypothesisCount is pending.
export function hypothesisProgressCounts(
  hypothesisCount: number,
  jobsByHypothesis: Record<string, ExperimentStats>,
): HypothesisProgressCounts {
  const groups = Object.values(jobsByHypothesis)
  const counts: HypothesisProgressCounts = { finished: 0, in_flight: 0, pending: hypothesisCount - groups.length }
  for (const { by_status } of groups) {
    if (by_status[ExperimentStatus.RUNNING]) counts.in_flight++
    else if (by_status[ExperimentStatus.QUEUED]) counts.pending++
    else if (Object.keys(by_status).every(s => TERMINAL.includes(s))) counts.finished++
    else counts.pending++
  }
  return counts
}
