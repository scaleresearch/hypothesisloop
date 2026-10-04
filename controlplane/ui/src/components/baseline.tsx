'use client'

import useSWR from 'swr'
import { fetchExperimentMetrics } from '@/lib/api'
import type { MetricDataPoint, PlatformExperiment } from '@/types'
import { TextLink } from '@/components/ui/link'
import { Callout } from '@/components/ui/status-message'

/** The last raw value a job reported for each metric key. */
export function lastReported(points: MetricDataPoint[]): Map<string, number> {
  const last = new Map<string, number>()
  for (const point of points) {
    if (!point.metric_basis || point.metric_basis === 'raw') last.set(point.metric_name, point.metric_value)
  }
  return last
}

/**
 * What every researcher is ranked against: the job that measured the baseline with what it
 * reported, read from the job itself. Nothing while none is declared, which the phase strip says.
 */
export function Baseline({ pe }: { pe: PlatformExperiment }) {
  const job = pe.baseline_job_id
  const { data: points } = useSWR(job ? ['baseline', job] : null, () => fetchExperimentMetrics(job!))
  if (!job) return null
  const reported = points && lastReported(points)
  const values = (pe.metrics ?? []).filter(m => reported?.has(m.key)).map(m => `${m.key} ${reported!.get(m.key)}`).join(', ')
  return (
    <Callout tone="info">
      Baseline measured by <TextLink href={`/jobs/${job}`}>job {job}</TextLink>{values && `: ${values}`}.
    </Callout>
  )
}
