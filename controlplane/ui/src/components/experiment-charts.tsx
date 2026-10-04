'use client'

import { shortId } from '@/lib/format'
import { Fragment, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { TextLink } from '@/components/ui/link'
import useSWR from 'swr'
import {
  LineChart,
  Line,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  Legend,
  ResponsiveContainer,
  ReferenceLine,
} from 'recharts'
import {
  fetchExperimentsPage,
  fetchExperimentMetrics,
} from '@/lib/api'
import type { StagesStatus } from '@/lib/api'
import type { Experiment, MetricDataPoint, AgentMetricSeries } from '@/types'
import { AgentLabel, useAgentName } from '@/components/agent-label'
import { InsetCard } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Dot } from '@/components/ui/dot'
import { EmptyState } from '@/components/ui/empty-state'
import { Captioned } from '@/components/ui/captioned'
import { Inline, Stack } from '@/components/ui/stack'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { ExpandButton, Expandable, useExpanded } from '@/components/ui/expandable'
import { SearchableSelect } from '@/components/ui/searchable-select'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { Eyebrow, Pill, Text } from '@/components/ui/text'
import { semantic, seriesPalette, chart } from '@/lib/colors'
import { jobPath } from '@/lib/routes'

// Bounded-score metrics (roughly 0-1) get their own axis so they aren't flattened to a
// hairline by unbounded metrics like seconds_per_subject/cv_seconds sitting in the tens+.
// Kept as a simple name check rather than a generic auto-scaling heuristic.
const BOUNDED_METRIC_RE = /auroc|accuracy|precision|recall|f1|auc/i

function isBoundedMetric(name: string): boolean {
  return BOUNDED_METRIC_RE.test(name)
}

/** A chart that can fill the window, named by its label while it does. */
function ExpandableChart({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Expandable label={label} className="relative">
      <ChartTitle label={label} />
      <ExpandButton className="absolute right-0 top-0 z-10" />
      {children}
    </Expandable>
  )
}

function ChartTitle({ label }: { label: string }) {
  return useExpanded() ? <Eyebrow className="mb-2">{label}</Eyebrow> : null
}

/** Holds a chart at its height in the page, and grows it into the rest of the window while it fills it. */
function ChartBox({ height, children }: { height: number; children: ReactNode }) {
  const expanded = useExpanded()
  return <div className={expanded ? 'min-h-0 flex-fill' : undefined} style={expanded ? undefined : { height }}>{children}</div>
}

// Shared chart body for the per-job card and the job picker. `trackedMetrics`
// scopes the plot to the experiment's official tracked/ranking metrics rather than every
// ad-hoc metric_name a job happens to report.
function MetricLineChart({ points, trackedMetrics, height }: { points: MetricDataPoint[]; trackedMetrics: string[]; height: number }) {
  const trackedSet = new Set(trackedMetrics)
  const filtered = points.filter((p) => trackedSet.has(p.metric_name))
  const metricNames = Array.from(new Set(filtered.map((p) => p.metric_name))).sort()
  // One row per point; a point only sets its own metric's key, so unrelated metrics are left
  // undefined for that row (a gap in their line, not a fake zero). Plotted over training progress:
  // a job of a few seconds has no readable time axis.
  const chartData = [...filtered]
    .sort((a, b) => new Date(a.recorded_at as string).getTime() - new Date(b.recorded_at as string).getTime())
    .map((p) => ({
      frac: parseFloat((p.fraction_complete * 100).toFixed(1)),
      [p.metric_name]: p.metric_value,
    }))
  if (chartData.length === 0) {
    return <EmptyState>No data yet</EmptyState>
  }
  const hasBounded = metricNames.some(isBoundedMetric)
  const hasUnbounded = metricNames.some((n) => !isBoundedMetric(n))
  const axisOf = (name: string) => (isBoundedMetric(name) ? 'left' : 'right')
  return (
    <ChartBox height={height}>
    <ResponsiveContainer width="100%" height="100%">
      <LineChart data={chartData} margin={{ top: 4, right: 8, left: 0, bottom: 0 }}>
        <CartesianGrid strokeDasharray="3 3" stroke={chart.grid} />
        <XAxis dataKey="frac" type="number" domain={[0, 100]} tickFormatter={(v) => `${v}%`} tick={chart.tick} />
        {hasBounded && <YAxis yAxisId="left" domain={['auto', 'auto']} tick={chart.tick} width={40} />}
        {hasUnbounded && <YAxis yAxisId="right" orientation="right" domain={['auto', 'auto']} tick={chart.tick} width={40} />}
        <Tooltip formatter={(v: number) => v.toFixed(4)} labelFormatter={(l) => `${l}% of training`} contentStyle={chart.tooltip.contentStyle} labelStyle={chart.tooltip.labelStyle} />
        <Legend wrapperStyle={chart.legend} />
        {metricNames.map((name, i) => (
          <Line
            key={name}
            yAxisId={axisOf(name)}
            dataKey={name}
            name={hasBounded && hasUnbounded ? `${name} (${axisOf(name)} axis)` : name}
            stroke={seriesPalette[i % seriesPalette.length]}
            dot={{ r: 2 }}
            strokeWidth={1.5}
            type="monotone"
            connectNulls
            isAnimationActive={false}
          />
        ))}
      </LineChart>
    </ResponsiveContainer>
    </ChartBox>
  )
}

function JobMetricMini({ jobId, trackedMetrics }: { jobId: string; trackedMetrics: string[] }) {
  // 48h-scale experiments move slowly enough that 10s polling here just repeats the same
  // full per-job history over and over; 30s keeps mini-charts fresh without redundant load
  // when dozens of these mount at once (one per running/expanded job).
  const { data: metrics } = useSWR<MetricDataPoint[]>(
    `metrics-${jobId}`,
    () => fetchExperimentMetrics(jobId),
    { refreshInterval: 30_000 },
  )
  const points = (metrics ?? []).filter((p) => p.recorded_at)
  return (
    <ExpandableChart label={`Metrics of job ${jobId}`}>
      <MetricLineChart points={points} trackedMetrics={trackedMetrics} height={400} />
    </ExpandableChart>
  )
}

// Lets the user inspect any job belonging to this platform experiment (not just currently
// running ones) and pick which of that job's actually-reported metrics to chart, rather than
// being limited to the experiment's tracked/ranking subset like the per-agent panel above.
export function LiveMetricsPicker({ jobs }: { jobs: Experiment[] }) {
  const agentName = useAgentName()
  // Most-recently-active first: updated_at moves as a running job reports progress.
  const sorted = useMemo(
    () => [...jobs].sort((a, b) => {
      const tA = new Date(a.updated_at).getTime()
      const tB = new Date(b.updated_at).getTime()
      return tB - tA
    }),
    [jobs],
  )
  const [jobId, setJobId] = useState<string | null>(sorted[0]?.id ?? null)
  const selectedJob = sorted.find(j => j.id === jobId) ?? sorted[0]
  const effectiveJobId = selectedJob?.id

  const { data: metrics } = useSWR<MetricDataPoint[]>(
    effectiveJobId ? `metrics-${effectiveJobId}` : null,
    () => fetchExperimentMetrics(effectiveJobId as string),
    { refreshInterval: 30_000 },
  )
  const points = (metrics ?? []).filter((p) => p.recorded_at)
  const availableMetrics = useMemo(
    () => Array.from(new Set(points.map((p) => p.metric_name))).sort(),
    [points],
  )

  // Selected metric set defaults to all metrics the job actually reports, but is keyed per-job
  // so switching jobs doesn't carry over a selection the new job may not even report.
  const [selectedByJob, setSelectedByJob] = useState<Map<string, Set<string>>>(new Map())
  const selected = selectedByJob.get(effectiveJobId ?? '') ?? new Set(availableMetrics)
  const setSelected = (next: Set<string>) => {
    if (!effectiveJobId) return
    setSelectedByJob(prev => new Map(prev).set(effectiveJobId, next))
  }

  if (!selectedJob) return null

  return (
    <Stack>
      <Inline gap="md" align="start">
        <Captioned caption="Job">
          <SearchableSelect
            options={sorted.map(j => ({ value: j.id, label: `${shortId(j.id)} · ${j.status} · ${agentName(j.agent_id)}` }))}
            value={selectedJob.id}
            onChange={v => { if (v) setJobId(v) }}
            placeholder="Search jobs…"
            className="min-w-80"
            hideAllOption
          />
        </Captioned>
        <Captioned caption="Metrics" className="min-w-52 flex-1">
          {availableMetrics.length === 0 ? (
            <Text as="span" variant="hint">No metrics reported yet</Text>
          ) : (
            <Inline gap="xs">
              {availableMetrics.map(name => {
                const isOn = selected.has(name)
                return (
                  <Checkbox
                    key={name}
                    pill
                    checked={isOn}
                    onChange={() => {
                      const next = new Set(selected)
                      isOn ? next.delete(name) : next.add(name)
                      setSelected(next)
                    }}
                    label={name}
                  />
                )
              })}
            </Inline>
          )}
        </Captioned>
      </Inline>
      <ExpandableChart label={`Metrics of job ${selectedJob.id}`}>
        <MetricLineChart points={points} trackedMetrics={Array.from(selected)} height={420} />
      </ExpandableChart>
    </Stack>
  )
}

// Colors are assigned once per full (unfiltered) agent roster, keyed by a sorted id list, so a
// given agent keeps the same color even as the user toggles which agents/metrics are visible.
export function useAgentColorMap(agentIds: string[]): Map<string, string> {
  const roster = [...agentIds].sort().join(',')
  return useMemo(
    () => new Map(roster.split(',').map((id, i) => [id, seriesPalette[i % seriesPalette.length]])),
    [roster],
  )
}

export function AgentSelector({
  agentIds, selected, onChange, colorFor,
}: {
  agentIds: string[]
  selected: Set<string>
  onChange: (next: Set<string>) => void
  colorFor: (agentId: string) => string
}) {
  const allSelected = agentIds.length > 0 && agentIds.every(id => selected.has(id))
  return (
    <Stack gap="sm">
      <Inline nowrap className="justify-between">
        <Eyebrow>Agents</Eyebrow>
        <Button type="button" variant="link" size="sm" onClick={() => onChange(allSelected ? new Set() : new Set(agentIds))}>
          {allSelected ? 'Clear' : 'Select all'}
        </Button>
      </Inline>
      <Inline gap="xs">
        {agentIds.map((id) => {
          const isOn = selected.has(id)
          return (
            <Checkbox
              key={id}
              pill
              color={colorFor(id)}
              checked={isOn}
              onChange={() => {
                const next = new Set(selected)
                isOn ? next.delete(id) : next.add(id)
                onChange(next)
              }}
              label={<><Dot color={colorFor(id)} /><AgentLabel id={id} /></>}
            />
          )
        })}
      </Inline>
    </Stack>
  )
}

export function MetricSelector({
  metrics, selected, onChange,
}: {
  metrics: { key: string; direction: string }[]
  selected: Set<string>
  onChange: (next: Set<string>) => void
}) {
  const allSelected = metrics.length > 0 && metrics.every(m => selected.has(m.key))
  return (
    <Stack gap="sm">
      <Inline nowrap className="justify-between">
        <Eyebrow>Metrics</Eyebrow>
        <Button type="button" variant="link" size="sm" onClick={() => onChange(allSelected ? new Set() : new Set(metrics.map(m => m.key)))}>
          {allSelected ? 'Clear' : 'Select all'}
        </Button>
      </Inline>
      <Inline gap="xs">
        {metrics.map((m, i) => {
          const isOn = selected.has(m.key)
          return (
            <Checkbox
              key={m.key}
              pill
              checked={isOn}
              onChange={() => {
                const next = new Set(selected)
                isOn ? next.delete(m.key) : next.add(m.key)
                onChange(next)
              }}
              label={<>
                {i === 0 && <span className="font-medium">★</span>}
                {m.key}
                <span>{m.direction === 'maximize' ? '↑' : '↓'}</span>
              </>}
            />
          )
        })}
      </Inline>
    </Stack>
  )
}

export function CompetingAgentsChart({
  series: allSeries, metricName, direction, selectedAgents, colorFor, boundaries,
}: {
  series: AgentMetricSeries[] | undefined
  metricName: string
  direction: 'maximize' | 'minimize'
  selectedAgents: Set<string>
  colorFor: (agentId: string) => string
  /** Every stage boundary already crossed, as {stage_index, advanced_at}. */
  boundaries?: StagesStatus['advances']
}) {
  const agentName = useAgentName()
  // An agent runs many jobs back to back over the course of a platform experiment (baseline,
  // then a string of hypothesis variants), each its own series with its own local warmup ramp
  // and plateau. Plotting those raw per-job values spliced together as one "per agent" line
  // makes the chart swing wildly between one job's converged plateau (e.g. 405) and the next
  // job's — which might be a regression test or a fresh variant landing lower (e.g. 188..436..
  // 405..420) — even though nothing about the agent's best-known performance actually dropped.
  // What the chart should show is each agent's best-known value *so far* (a running max/min,
  // matching how these metrics are defined server-side), computed here by merging every job's
  // points for that agent, sorting by time, and taking a running best — then forward-filling
  // that onto the shared timestamp axis below so the line only moves when a new personal best
  // is actually set.
  //
  // This merge/sort/pivot touches every point across every job (tens of thousands on a large
  // platform experiment), so it's useMemo'd on the series/selection identity rather than
  // redone on every render — e.g. hovering the scoreboard or toggling an unrelated agent
  // checkbox shouldn't re-walk the full history of every other chart on the page.
  const { runningBestByAgent, rows, nonRawAgentIds } = useMemo(() => {
    // Never merge a non-"raw"-basis series into an agent's running best: this is a "best so
    // far" comparison chart, so a rescaled/derived value can win it exactly the way it won a
    // ranking before this fix. Excluded agents are surfaced via nonRawAgentIds instead.
    const series = (allSeries ?? []).filter(
      s => s.points.length > 0 && selectedAgents.has(s.agent_id || s.experiment_id) && (!s.metric_basis || s.metric_basis === 'raw'),
    )
    const nonRawAgentIds = new Set(
      (allSeries ?? [])
        .filter(s => s.metric_basis && s.metric_basis !== 'raw' && selectedAgents.has(s.agent_id || s.experiment_id))
        .map(s => s.agent_id || s.experiment_id),
    )
    const bestDir = direction === 'maximize' ? Math.max : Math.min
    const runningBestByAgent = new Map<string, { t: number; best: number }[]>()
    for (const agentId of Array.from(new Set(series.map(s => s.agent_id || s.experiment_id)))) {
      const points = series
        .filter(s => (s.agent_id || s.experiment_id) === agentId)
        .flatMap(s => s.points)
        .map(p => ({ t: new Date(p.timestamp).getTime(), value: p.value }))
        .sort((a, b) => a.t - b.t)
      let best: number | undefined
      const trace = points.map(p => {
        best = best === undefined ? p.value : bestDir(best, p.value)
        return { t: p.t, best }
      })
      runningBestByAgent.set(agentId, trace)
    }

    // Pivot onto one row per timestamp (union across all agents), forward-filling each agent's
    // most recent running-best value so the line stays flat between updates instead of dropping
    // to null (which would otherwise fragment it every time only one agent reports).
    const timestamps = Array.from(new Set(series.flatMap(s => s.points.map(p => new Date(p.timestamp).getTime())))).sort((a, b) => a - b)
    const cursor = new Map<string, number>() // agentId -> index into its running-best trace
    const rows = timestamps.map((t) => {
      const row: Record<string, number | string> = { t }
      for (const [agentId, trace] of Array.from(runningBestByAgent.entries())) {
        let i = cursor.get(agentId) ?? 0
        while (i < trace.length && trace[i].t <= t) i++
        cursor.set(agentId, i)
        if (i > 0) row[agentId] = trace[i - 1].best
      }
      return row
    })
    return { runningBestByAgent, rows, nonRawAgentIds }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [allSeries, direction, Array.from(selectedAgents).sort().join(',')])

  if (allSeries === undefined) return <Loading />
  if (runningBestByAgent.size === 0) {
    return <EmptyState>No metric data to show for &quot;{metricName}&quot; with the current agent selection.</EmptyState>
  }

  return (
    <ExpandableChart label={metricName}>
      {nonRawAgentIds.size > 0 && (
        <Pill
          tone="danger"
          className="mb-2"
          title="These agents also reported this metric on a non-&quot;raw&quot; basis (a rescaled/transformed value); that data is excluded from this chart and never compared to raw-basis agents."
        >
          Non-raw basis data excluded for: {Array.from(nonRawAgentIds).sort().map((agentID, i) => <Fragment key={agentID}>{i > 0 && ', '}<AgentLabel id={agentID} /></Fragment>)}
        </Pill>
      )}
    <ChartBox height={300}>
    <ResponsiveContainer width="100%" height="100%">
      <LineChart data={rows} margin={{ top: 8, right: 24, left: 0, bottom: 0 }}>
        <CartesianGrid strokeDasharray="3 3" stroke={chart.grid} />
        <XAxis dataKey="t" type="number" domain={['dataMin', 'dataMax']} tickFormatter={(v) => new Date(v).toLocaleTimeString()} tick={chart.tick} />
        <YAxis domain={['auto', 'auto']} tick={chart.tick} />
        <Tooltip formatter={(v: number) => v.toFixed(4)} labelFormatter={(l) => new Date(l).toLocaleTimeString()} contentStyle={chart.tooltip.contentStyle} labelStyle={chart.tooltip.labelStyle} />
        {(boundaries ?? []).map(b => (
          <ReferenceLine
            key={b.stage_index}
            x={new Date(b.advanced_at).getTime()}
            stroke={semantic.warning}
            strokeDasharray="4 4"
            label={{ value: `End of stage ${b.stage_index}`, position: 'insideTopLeft', fill: semantic.warning, ...chart.label }}
          />
        ))}
        {/* One <Line> per unique agent, plotting its running-best trace computed above. The only
            nulls left in a row are the leading ones before an agent has reported its first point
            at all, so connectNulls is safe here (and desired, so the line doesn't fragment on
            timestamps where only other agents reported). */}
        {Array.from(runningBestByAgent.keys()).map((agentId) => (
          <Line
            key={agentId}
            dataKey={agentId}
            name={agentName(agentId)}
            stroke={colorFor(agentId)}
            dot={false}
            strokeWidth={2}
            type="stepAfter"
            connectNulls
          />
        ))}
      </LineChart>
    </ResponsiveContainer>
    </ChartBox>
    </ExpandableChart>
  )
}

export function AgentJobsPanel({ projectId, platformExperimentId, agentId, trackedMetrics }: { projectId: string; platformExperimentId: string; agentId: string; trackedMetrics: string[] }) {
  const { data, error } = useSWR(
    ['pe-agent-jobs', platformExperimentId, agentId],
    () => fetchExperimentsPage({ project_id: projectId, platform_experiment_id: platformExperimentId, agent_id: agentId, sort: '-created_at' }),
    { refreshInterval: 15_000 },
  )
  if (error) return <ErrorMessage>Could not load jobs from <AgentLabel id={agentId} />.</ErrorMessage>
  if (!data) return <Loading />
  const jobs = data.items
  if (jobs.length === 0) {
    return <EmptyState>No jobs from <AgentLabel id={agentId} /> yet.</EmptyState>
  }
  return (
    <div className="grid grid-cols-panels gap-3 pb-1 pt-2.5">
      {jobs.map(job => (
        <InsetCard key={job.id} gap="xs">
          <Inline nowrap className="justify-between text-2xs">
            <TextLink href={jobPath(platformExperimentId, job.id)} className="font-mono font-medium">{shortId(job.id)}</TextLink>
            <Badge status={job.status} />
          </Inline>
          <JobMetricMini jobId={job.id} trackedMetrics={trackedMetrics} />
        </InsetCard>
      ))}
    </div>
  )
}
