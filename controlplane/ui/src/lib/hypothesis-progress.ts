import { ExperimentStatus } from '@/types'
import type { ExperimentStats } from '@/lib/api'

const TERMINAL: string[] = [
  ExperimentStatus.COMPLETED,
  ExperimentStatus.FAILED,
  ExperimentStatus.EVICTED,
  ExperimentStatus.REJECTED,
]

// Job activity is separate from the verdict: a hypothesis whose jobs all ended is still open until
// its owner rules on it. jobsByHypothesis is the stats endpoint's group_by=hypothesis, which has a
// group only for a hypothesis with jobs.
export function hypothesisActivity(
  hypothesisCount: number,
  jobsByHypothesis: Record<string, ExperimentStats>,
): string {
  const groups = Object.values(jobsByHypothesis)
  const inFlight = groups.filter(g => Object.keys(g.by_status).some(s => !TERMINAL.includes(s))).length
  const untested = hypothesisCount - groups.length
  return [
    inFlight > 0 && `${inFlight} with jobs in flight`,
    untested > 0 && `${untested} without jobs`,
  ].filter(Boolean).join(' · ')
}
