import { Claim } from '@/components/ui/claim'
import { TextLink } from '@/components/ui/link'
import { graphHref } from '@/lib/hypothesis-graph'
import { hypothesisPath } from '@/lib/routes'
import type { Experiment } from '@/types/api'

type Tested = Pick<Experiment, 'hypothesis_id' | 'objective' | 'hypothesis' | 'platform_experiment_id'>

export const BASELINE_JOB = 'Baseline of the objective'

/** A baseline agent's job tests no hypothesis: it measures the objective of its platform experiment. */
export function measuresBaseline(job: Tested): boolean {
  return job.objective === true
}

/** What a job tests, in a row that opens the job: its hypothesis, or the objective for the baseline's. */
export function JobHypothesis({ job }: { job: Tested }) {
  return measuresBaseline(job)
    ? <TextLink href={graphHref(job.platform_experiment_id)} onClick={e => e.stopPropagation()} className="line-clamp-1">{BASELINE_JOB}</TextLink>
    : <TextLink href={hypothesisPath(job.platform_experiment_id, job.hypothesis_id!)} className="block" onClick={e => e.stopPropagation()}><Claim text={job.hypothesis} lines={1} /></TextLink>
}
