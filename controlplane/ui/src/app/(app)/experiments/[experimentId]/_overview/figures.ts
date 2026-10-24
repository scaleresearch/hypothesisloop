import type { Figure } from '@/components/ui/figures'
import { evictionLabel, faultBreakdown } from '@/lib/eviction'
import { formatAccH } from '@/lib/format'
import { graphHref } from '@/lib/hypothesis-graph'
import { hypothesisActivity } from '@/lib/hypothesis-progress'
import type { Phase } from '@/lib/phase'
import { experimentPath, withQuery } from '@/lib/routes'
import { inFlight } from '@/lib/units'
import { hypothesisStatusValues } from '@/types/api'
import type { ExperimentStats, HypothesisStats, PlatformExperiment } from '@/types/api'

/** The experiment in figures: what it spent and holds, its jobs, hypotheses and executors. */
export function overviewFigures({ pe, phase, plannedOnly, unit, stats, hypothesisStats, verdicts, executors }: {
  pe: PlatformExperiment
  phase: Phase
  plannedOnly: boolean
  unit: string
  stats: ExperimentStats | undefined
  hypothesisStats: ExperimentStats | undefined
  verdicts: HypothesisStats | undefined
  executors: number | undefined
}): Figure[] {
  // SUBMITTED and RUNNING, the set max_concurrent_accelerators is checked against.
  const held = inFlight(stats?.in_flight_accelerators ?? 0, stats?.in_flight_cores ?? 0, pe.max_concurrent_accelerators)
  const jobsByStatus = stats?.by_status ?? {}
  const evicted = jobsByStatus.EVICTED ?? 0
  const running = jobsByStatus.RUNNING ?? 0
  const hypothesisCount = verdicts?.total ?? 0
  const hypothesisSub = [
    hypothesisStatusValues.map(v => `${verdicts?.by_status[v] ?? 0} ${v}`).join(' · '),
    hypothesisActivity(hypothesisCount, hypothesisStats?.groups ?? {}, pe.id),
  ].filter(Boolean).join(' · ')

  // Evicted jobs, broken down by eviction_reason — a platform experiment can evict for very
  // different reasons (stuck on admission vs. silent vs. ran past a stage's job
  // length cap), so a single opaque count hides whether evictions are a symptom of something
  // wrong versus routine stage cuts. Counted server-side over the whole experiment, not over
  // the page of jobs, and already grouped by code (a reason may carry a ': detail'
  // explanation — see lib/eviction).
  const evictionBreakdown = Object.entries(stats?.evictions_by_reason ?? {})
    .sort((a, b) => b[1] - a[1])
    .map(([r, n]) => `${evictionLabel(r)} (${n})`)
    .join(' · ')
  // Whose fault those evictions were, folded from the same tally — an infrastructure count here
  // says the platform failed these jobs, not the agents running them.
  const evictionFaultBreakdown = faultBreakdown(stats?.evictions_by_class ?? {})

  const figures: Figure[] = []
  if (pe.started_at) {
    figures.push({ label: 'Budget spent', value: `${formatAccH(pe.consumed_accelerator_hours ?? 0)} of ${pe.budget_accelerator_hours} ${unit}` })
  } else if (!plannedOnly) {
    figures.push({ label: 'Budget', value: `${pe.budget_accelerator_hours} ${unit}` })
  }
  if (phase === 'running') {
    figures.push({
      label: held.label,
      value: held.value,
      tone: held.atCap ? 'warning' : undefined,
      title: pe.max_concurrent_accelerators == null ? 'No cap set' : undefined,
    })
  }
  if (!plannedOnly) figures.push({ label: 'Jobs', value: stats?.total ?? 0, href: experimentPath(pe.id, 'jobs') })
  if (running > 0) figures.push({ label: 'Running', value: running, tone: 'success', href: withQuery(experimentPath(pe.id, 'jobs'), { status: 'RUNNING' }) })
  if (evicted > 0) {
    figures.push({
      label: 'Evicted',
      value: evicted,
      tone: 'warning',
      title: [evictionFaultBreakdown, evictionBreakdown].filter(Boolean).join('. ') || undefined,
      href: withQuery(experimentPath(pe.id, 'jobs'), { status: 'EVICTED' }),
    })
  }
  if (!plannedOnly) figures.push({ label: 'Hypotheses', value: hypothesisCount, title: hypothesisSub, href: graphHref(pe.id) })
  if (phase === 'closed') {
    if (!plannedOnly && executors) figures.push({ label: 'Executors', value: executors, title: 'The executors that took part. Researchers, baseline agents and the coordinator are not counted.' })
  } else if (!plannedOnly && executors !== undefined) {
    figures.push({
      label: 'Executors',
      value: `${executors} of ${pe.max_agents}`,
      title: 'Executors at work, of the most that may be at work at once. Researchers, baseline agents and the coordinator are not counted.',
    })
  }
  return figures
}
