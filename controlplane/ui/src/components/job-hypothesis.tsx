import type { Experiment } from '@/types'
import { TextLink } from '@/components/ui/link'
import { graphHref } from '@/lib/hypothesis-graph'
import { hypothesisPath } from '@/lib/routes'

type Tested = Pick<Experiment, 'hypothesis_id' | 'objective' | 'hypothesis' | 'platform_experiment_id' | 'check'>

export const BASELINE_JOB = 'Baseline of the objective'
export const CHECK_JOB = 'A check of the code in its environment'

/** A baseline agent's job tests no hypothesis: it measures the objective of its platform experiment. */
export function measuresBaseline(job: Tested): boolean {
  return job.objective === true
}

/** What a job tests, in a row that opens the job: its hypothesis, or the objective for the baseline's. */
export function JobHypothesis({ job }: { job: Tested }) {
  if (job.check) return <span className="text-muted">{CHECK_JOB}</span>
  return measuresBaseline(job)
    ? <TextLink href={graphHref(job.platform_experiment_id)} onClick={e => e.stopPropagation()}>{BASELINE_JOB}</TextLink>
    : <TextLink href={hypothesisPath(job.platform_experiment_id, job.hypothesis_id!)} onClick={e => e.stopPropagation()}>{job.hypothesis}</TextLink>
}
