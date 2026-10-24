'use client'

import useSWR from 'swr'
import { fetchExperimentMetrics } from '@/lib/api/jobs'
import { TextLink } from '@/components/ui/link'
import { Callout } from '@/components/ui/status-message'
import { formatMetric, shortId } from '@/lib/format'
import { jobPath } from '@/lib/routes'
import { keys } from '@/lib/keys'
import type { MetricDataPoint, PlatformExperiment } from '@/types/api'

/** The last raw value a job reported for each metric key. */
function lastReported(points: MetricDataPoint[]): Map<string, number> {
  const last = new Map<string, number>()
  for (const point of points) {
    if (!point.metric_basis || point.metric_basis === 'raw') last.set(point.metric_name, point.metric_value)
  }
  return last
}

/**
 * What every executor is ranked against: the job that measured the baseline with what it
 * reported, read from the job itself. Nothing while none is declared, which the phase strip says.
 */
export function Baseline({ pe }: { pe: PlatformExperiment }) {
  const job = pe.baseline_job_id
  const { data: points, error } = useSWR(job ? keys.jobMetrics(job) : null, () => fetchExperimentMetrics(job!))
  if (!job) return null
  const reported = points && lastReported(points)
  const values = (pe.metrics ?? []).filter(m => reported?.has(m.key)).map(m => `${m.key} ${formatMetric(reported!.get(m.key)!)}`).join(', ')
  const raised = pe.baseline_raises?.at(-1)
  return (
    <Callout tone="info">
      Baseline measured by <TextLink href={jobPath(pe.id, job)} title={job}>{shortId(job)}</TextLink>
      {error ? `. What it reported could not be loaded: ${error.message}` : !points ? ': reading what it reported' : values && `: ${values}`}.
      {raised && ` Raised from ${shortId(raised.from)} by ${raised.actor.name || raised.actor.kind}: ${raised.reason}.`}
    </Callout>
  )
}
