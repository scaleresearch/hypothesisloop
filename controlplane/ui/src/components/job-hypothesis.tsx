import type { Experiment } from '@/types'
import { TextLink } from '@/components/ui/link'
import { graphHref } from '@/lib/hypothesis-graph'

type Tested = Pick<Experiment, 'hypothesis_id' | 'hypothesis' | 'platform_experiment_id'>

export const BASELINE_JOB = 'Baseline of the objective'

/** A baseline agent's job tests no hypothesis: its hypothesis_id is its platform experiment's id, which names the objective. */
export function measuresBaseline(job: Tested): boolean {
  return job.hypothesis_id === job.platform_experiment_id
}

/** What a job tests, in a row that opens the job: its hypothesis, or the objective for the baseline's. */
export function JobHypothesis({ job }: { job: Tested }) {
  return measuresBaseline(job)
    ? <TextLink href={graphHref(job.platform_experiment_id)} onClick={e => e.stopPropagation()}>{BASELINE_JOB}</TextLink>
    : <TextLink href={`/hypotheses/${job.hypothesis_id}`} onClick={e => e.stopPropagation()}>{job.hypothesis}</TextLink>
}
