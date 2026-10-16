import type { Experiment } from '@/types/jobs'
import { TextLink } from '@/components/ui/link'
import { graphHref } from '@/lib/hypothesis-graph'
import { hypothesisPath } from '@/lib/routes'

type Tested = Pick<Experiment, 'hypothesis_id' | 'objective' | 'hypothesis' | 'platform_experiment_id'>

export const BASELINE_JOB = 'Baseline of the objective'

/** A baseline agent's job tests no hypothesis: it measures the objective of its platform experiment. */
export function measuresBaseline(job: Tested): boolean {
  return job.objective === true
}

/** What a job tests, in a row that opens the job: its hypothesis, or the objective for the baseline's. */
export function JobHypothesis({ job }: { job: Tested }) {
  return measuresBaseline(job)
    ? <TextLink href={graphHref(job.platform_experiment_id)} onClick={e => e.stopPropagation()}>{BASELINE_JOB}</TextLink>
    : <TextLink href={hypothesisPath(job.platform_experiment_id, job.hypothesis_id!)} onClick={e => e.stopPropagation()}>{job.hypothesis}</TextLink>
}
