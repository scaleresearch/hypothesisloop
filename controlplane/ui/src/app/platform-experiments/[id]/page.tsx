'use client'

import { Fragment, useMemo, useState } from 'react'
import { useRouter } from 'next/navigation'
import Link from 'next/link'
import useSWR from 'swr'
import {
  LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer, ReferenceLine,
} from 'recharts'
import {
  fetchPlatformExperiment,
  fetchPlatformExperimentQuotas,
  fetchExperimentsPage,
  fetchExperimentStats,
  fetchDonationsPage,
  MAX_LIST_PAGE_SIZE,
  fetchStages,
  fetchExperimentMetrics,
  fetchPlatformExperimentTimeseries,
  fetchHypothesesPage,
} from '@/lib/api'
import type { StagesStatus } from '@/lib/api'
import { quotaRemainingAccH } from '@/lib/quota'
import type { PlatformExperiment, AgentQuota, Experiment, MetricDataPoint, AgentMetricSeries, MetricDefinition, Hypothesis } from '@/types'
import type { DonationRequest } from '@/lib/api'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Badge } from '@/components/ui/badge'
import { MetricBar } from '@/components/ui/metric-bar'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { StatTile } from '@/components/ui/stat-tile'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { semantic, agentPalette } from '@/lib/colors'
import { formatAccH, formatDate, isZeroDate } from '@/lib/format'
import { evictionLabel } from '@/lib/eviction'
import { hypothesisProgressCounts } from '@/lib/hypothesis-progress'
import { AddIdeaForm } from '@/components/human-idea'

const CHART_GRID = 'rgba(255,255,255,0.08)'
const CHART_TICK = { fontSize: 10, fill: 'var(--muted-fg)' }
const CHART_TOOLTIP_STYLE: React.CSSProperties = {
  fontSize: 11, background: 'var(--surface-raised)', border: '1px solid var(--border)',
  borderRadius: 6, color: 'var(--foreground)',
}

function JobMetricMini({ jobId, metricName }: { jobId: string; metricName?: string }) {
  // 48h-scale experiments move slowly enough that 10s polling here just repeats the same
  // full per-job history over and over; 30s keeps mini-charts fresh without redundant load
  // when dozens of these mount at once (one per running/expanded job).
  const { data: metrics } = useSWR<MetricDataPoint[]>(
    `metrics-${jobId}`,
    () => fetchExperimentMetrics(jobId),
    { refreshInterval: 30_000 },
  )
  const chartData = (metrics ?? [])
    .filter((p) => p.recorded_at && (!metricName || p.metric_name === metricName))
    .map((p) => ({
      t: new Date(p.recorded_at as string).getTime(),
      value: p.metric_value,
    }))
    .sort((a, b) => a.t - b.t)
  if (chartData.length === 0) {
    return <EmptyState>No data yet</EmptyState>
  }
  return (
    <ResponsiveContainer width="100%" height={110}>
      <LineChart data={chartData} margin={{ top: 4, right: 8, left: -20, bottom: 0 }}>
        <CartesianGrid strokeDasharray="3 3" stroke={CHART_GRID} />
        <XAxis dataKey="t" type="number" domain={['dataMin', 'dataMax']} tickFormatter={(v) => new Date(v).toLocaleTimeString()} tick={CHART_TICK} />
        <YAxis tick={CHART_TICK} />
        <Tooltip formatter={(v: number) => v.toFixed(4)} labelFormatter={(l) => new Date(l).toLocaleTimeString()} contentStyle={CHART_TOOLTIP_STYLE} />
        <Line dataKey="value" name={metricName ?? 'metric'} stroke={semantic.accent} dot={false} strokeWidth={1.5} type="monotone" />
      </LineChart>
    </ResponsiveContainer>
  )
}

// Distinct colors per agent so competing lines are visually separable on one chart.
const AGENT_COLORS = agentPalette

// Colors are assigned once per full (unfiltered) agent roster, keyed by a sorted id list, so a
// given agent keeps the same color even as the user toggles which agents/metrics are visible.
function useAgentColorMap(agentIds: string[]): Map<string, string> {
  return useMemo(() => {
    const sorted = [...agentIds].sort()
    return new Map(sorted.map((id, i) => [id, AGENT_COLORS[i % AGENT_COLORS.length]]))
  }, [agentIds.join(',')])
}

function AgentSelector({
  agentIds, selected, onChange, colorFor,
}: {
  agentIds: string[]
  selected: Set<string>
  onChange: (next: Set<string>) => void
  colorFor: (agentId: string) => string
}) {
  const allSelected = agentIds.length > 0 && agentIds.every(id => selected.has(id))
  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
        <div className="uppercase-label">Agents</div>
        <button type="button" onClick={() => onChange(allSelected ? new Set() : new Set(agentIds))} className="wa-btn-link" style={{ fontSize: 11 }}>
          {allSelected ? 'Clear' : 'Select all'}
        </button>
      </div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
        {agentIds.map((id) => {
          const isOn = selected.has(id)
          return (
            <label
              key={id}
              className="mono"
              style={{
                display: 'flex', alignItems: 'center', gap: 5, fontSize: 11, cursor: 'pointer',
                padding: '3px 8px', borderRadius: 999, border: `1px solid ${isOn ? colorFor(id) : 'var(--border)'}`,
                background: isOn ? `${colorFor(id)}1a` : 'var(--surface-2)', color: isOn ? 'var(--foreground)' : 'var(--muted-fg)',
              }}
            >
              <input
                type="checkbox"
                checked={isOn}
                onChange={() => {
                  const next = new Set(selected)
                  isOn ? next.delete(id) : next.add(id)
                  onChange(next)
                }}
                style={{ margin: 0 }}
              />
              <span style={{ width: 8, height: 8, borderRadius: '50%', background: colorFor(id), flexShrink: 0 }} />
              {id}
            </label>
          )
        })}
      </div>
    </div>
  )
}

function MetricSelector({
  metrics, selected, onChange,
}: {
  metrics: { key: string; direction: string }[]
  selected: Set<string>
  onChange: (next: Set<string>) => void
}) {
  const allSelected = metrics.length > 0 && metrics.every(m => selected.has(m.key))
  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
        <div className="uppercase-label">Metrics</div>
        <button type="button" onClick={() => onChange(allSelected ? new Set() : new Set(metrics.map(m => m.key)))} className="wa-btn-link" style={{ fontSize: 11 }}>
          {allSelected ? 'Clear' : 'Select all'}
        </button>
      </div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
        {metrics.map((m, i) => {
          const isOn = selected.has(m.key)
          return (
            <label
              key={m.key}
              className="mono"
              style={{
                display: 'flex', alignItems: 'center', gap: 5, fontSize: 11, cursor: 'pointer',
                padding: '3px 8px', borderRadius: 999, border: `1px solid ${isOn ? 'var(--accent)' : 'var(--border)'}`,
                background: isOn ? 'rgba(124,108,240,0.14)' : 'var(--surface-2)', color: isOn ? 'var(--foreground)' : 'var(--muted-fg)',
              }}
            >
              <input
                type="checkbox"
                checked={isOn}
                onChange={() => {
                  const next = new Set(selected)
                  isOn ? next.delete(m.key) : next.add(m.key)
                  onChange(next)
                }}
                style={{ margin: 0 }}
              />
              {i === 0 && <span style={{ fontWeight: 700 }}>★</span>}
              {m.key}
              <span style={{ opacity: 0.7 }}>{m.direction === 'maximize' ? '↑' : '↓'}</span>
            </label>
          )
        })}
      </div>
    </div>
  )
}

function CompetingAgentsChart({
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
    <>
      {nonRawAgentIds.size > 0 && (
        <div
          className="mono"
          title="These agents also reported this metric on a non-&quot;raw&quot; basis (a rescaled/transformed value); that data is excluded from this chart and never compared to raw-basis agents."
          style={{
            marginBottom: 8, fontSize: 11, padding: '3px 8px', borderRadius: 4, display: 'inline-block',
            background: 'rgba(255,153,153,.12)', border: `1px solid ${semantic.danger}`, color: semantic.danger,
          }}
        >
          Non-raw basis data excluded for: {Array.from(nonRawAgentIds).sort().join(', ')}
        </div>
      )}
    <ResponsiveContainer width="100%" height={300}>
      <LineChart data={rows} margin={{ top: 8, right: 24, left: 0, bottom: 0 }}>
        <CartesianGrid strokeDasharray="3 3" stroke={CHART_GRID} />
        <XAxis dataKey="t" type="number" domain={['dataMin', 'dataMax']} tickFormatter={(v) => new Date(v).toLocaleTimeString()} tick={CHART_TICK} />
        <YAxis tick={CHART_TICK} />
        <Tooltip formatter={(v: number) => v.toFixed(4)} labelFormatter={(l) => new Date(l).toLocaleTimeString()} contentStyle={CHART_TOOLTIP_STYLE} />
        {(boundaries ?? []).map(b => (
          <ReferenceLine
            key={b.stage_index}
            x={new Date(b.advanced_at).getTime()}
            stroke={semantic.warning}
            strokeDasharray="4 4"
            label={{ value: `End of stage ${b.stage_index}`, position: 'insideTopLeft', fill: semantic.warning, fontSize: 10 }}
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
            name={agentId}
            stroke={colorFor(agentId)}
            dot={false}
            strokeWidth={2}
            type="monotone"
            connectNulls
          />
        ))}
      </LineChart>
    </ResponsiveContainer>
    </>
  )
}

function AgentJobsPanel({ agentId, jobs, metricName }: { agentId: string; jobs: Experiment[]; metricName?: string }) {
  if (jobs.length === 0) {
    return <EmptyState>No jobs from {agentId} yet.</EmptyState>
  }
  return (
    <div style={{ padding: '10px 0 4px', display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(260px, 1fr))', gap: 10 }}>
      {jobs.map(job => (
        <div key={job.id} className="wa-mini-card">
          <div style={{ fontSize: 11, marginBottom: 4, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
            <Link href={`/jobs/${job.id}`} className="mono text-link" style={{ fontWeight: 600 }}>{job.id.slice(0, 8)}…</Link>
            <StatusBadge status={job.status} />
          </div>
          <JobMetricMini jobId={job.id} metricName={metricName} />
        </div>
      ))}
    </div>
  )
}

function StatusBadge({ status }: { status: string }) {
  return <Badge status={status}>{status}</Badge>
}

// A agent's best-known value per metric key, e.g. { val_accuracy: 0.94, latency_ms: 120 }.
type MetricBests = Record<string, number | null>

// Does `a` Pareto-dominate `b`? True iff a is at least as good as b on every metric (direction-
// aware) and strictly better on at least one. An agent with no data on a metric is treated as
// having the worst possible value on it, so agents with zero results never dominate anyone.
function dominates(a: MetricBests, b: MetricBests, metrics: MetricDefinition[]): boolean {
  let strictlyBetterOnOne = false
  for (const m of metrics) {
    const worst = m.direction === 'maximize' ? -Infinity : Infinity
    const av = a[m.key] ?? worst
    const bv = b[m.key] ?? worst
    if (m.direction === 'maximize') {
      if (av < bv) return false
      if (av > bv) strictlyBetterOnOne = true
    } else {
      if (av > bv) return false
      if (av < bv) strictlyBetterOnOne = true
    }
  }
  return strictlyBetterOnOne
}

// Compute the scoreboard: one row per known agent, its best value per objective metric, whether
// it's on the Pareto front ("winner"), and a total order for display.
//
// Ranking logic (deliberately simple, no arbitrary weights):
//  - Winners = the Pareto front: agents no other agent dominates on every metric at once.
//    With a single metric this is exactly "highest (or lowest) value wins".
//  - Everyone else is ranked below, ordered by how many agents dominate them (fewer is better),
//    tie-broken alphabetically by agent id for a stable, well-defined order.
function buildScoreboard(
  experiments: Experiment[],
  quotas: AgentQuota[],
  metrics: MetricDefinition[],
  metricBestsByAgent: Map<string, MetricBests>,
  nonRawAgentIdsByMetric: Map<string, Set<string>> = new Map(),
): Array<{ agentId: string; metricBests: MetricBests; jobCount: number; completedCount: number; isWinner: boolean; nonRawMetrics: string[] }> {
  const counts = new Map<string, { jobCount: number; completedCount: number }>()
  for (const exp of experiments) {
    const entry = counts.get(exp.agent_id) ?? { jobCount: 0, completedCount: 0 }
    entry.jobCount++
    if (exp.status === 'COMPLETED') entry.completedCount++
    counts.set(exp.agent_id, entry)
  }

  const agentIds = new Set<string>([
    ...experiments.map(e => e.agent_id),
    ...quotas.map(q => q.agent_id),
  ])

  const rows = Array.from(agentIds).map((agentId) => {
    const metricBests = metricBestsByAgent.get(agentId) ?? {}
    const c = counts.get(agentId) ?? { jobCount: 0, completedCount: 0 }
    const hasAnyData = metrics.some(m => metricBests[m.key] != null)
    return { agentId, metricBests, ...c, hasAnyData }
  })

  const dominatedByCount = new Map<string, number>()
  for (const row of rows) {
    let count = 0
    if (row.hasAnyData) {
      for (const other of rows) {
        if (other.agentId === row.agentId || !other.hasAnyData) continue
        if (dominates(other.metricBests, row.metricBests, metrics)) count++
      }
    }
    dominatedByCount.set(row.agentId, count)
  }

  const result = rows.map(row => ({
    agentId: row.agentId,
    metricBests: row.metricBests,
    jobCount: row.jobCount,
    completedCount: row.completedCount,
    isWinner: metrics.length > 0 && row.hasAnyData && dominatedByCount.get(row.agentId) === 0,
    nonRawMetrics: metrics.filter(m => nonRawAgentIdsByMetric.get(m.key)?.has(row.agentId)).map(m => m.key),
  }))

  result.sort((a, b) => {
    if (a.isWinner !== b.isWinner) return a.isWinner ? -1 : 1
    const da = dominatedByCount.get(a.agentId) ?? 0
    const db = dominatedByCount.get(b.agentId) ?? 0
    if (da !== db) return da - db
    return a.agentId.localeCompare(b.agentId)
  })
  return result
}

// Best value per agent per metric, derived from each metric's full timeseries (same source the
// "Competing Agents over time" chart uses) rather than the single-valued job.final_metric_value,
// since a platform experiment can define several objectives at once.
//
// Only "raw"-basis series count toward `best`: a series reported on a rescaled/derived basis is
// never blended into a min/max against a raw-basis competitor — that mismatch is exactly what
// let a rescaled loss look like a real win before. An agent whose only series for this metric is
// non-"raw" is returned in `nonRawAgentIds` so the scoreboard can flag it instead of the agent
// simply vanishing from the ranking with no explanation.
function bestPerAgentFromSeries(
  series: AgentMetricSeries[],
  direction: 'maximize' | 'minimize',
): { best: Map<string, number>; nonRawAgentIds: Set<string> } {
  const best = new Map<string, number>()
  const nonRawAgentIds = new Set<string>()
  for (const s of series) {
    const agentId = s.agent_id || s.experiment_id
    if (s.metric_basis && s.metric_basis !== 'raw') {
      nonRawAgentIds.add(agentId)
      continue
    }
    for (const p of s.points) {
      const cur = best.get(agentId)
      if (cur === undefined) {
        best.set(agentId, p.value)
      } else {
        best.set(agentId, direction === 'maximize' ? Math.max(cur, p.value) : Math.min(cur, p.value))
      }
    }
  }
  return { best, nonRawAgentIds }
}

export default function PlatformExperimentDetailPage({ params }: { params: { id: string } }) {
  const { id } = params
  const router = useRouter()
  const [selectedAgentIds, setSelectedAgentIds] = useState<Set<string> | null>(null)
  const [selectedMetricKeys, setSelectedMetricKeys] = useState<Set<string> | null>(null)
  const [expandedAgentId, setExpandedAgentId] = useState<string | null>(null)

  const { data: pe, error: peError, isLoading: peLoading } = useSWR<PlatformExperiment>(
    ['pe', id],
    () => fetchPlatformExperiment(id),
    { refreshInterval: 15_000 },
  )

  const { data: quotas, error: quotasError } = useSWR<AgentQuota[]>(
    ['pe-quotas', id],
    () => fetchPlatformExperimentQuotas(id),
    { refreshInterval: 15_000 },
  )

  // One explicitly bounded page: every list endpoint caps a read, so asking without a limit
  // would only hide where the cut fell.
  const { data: experimentsPage, error: experimentsError } = useSWR(
    ['pe-experiments', id],
    () => fetchExperimentsPage({ platform_experiment_id: id, limit: MAX_LIST_PAGE_SIZE }),
    { refreshInterval: 15_000 },
  )
  const experiments = experimentsPage?.items

  // Whole-experiment job counts, grouped server-side. The job list above is one bounded page,
  // so tallying it would report the page's shape under a label that reads as the experiment's.
  const { data: stats, error: statsError } = useSWR(
    ['pe-stats', id],
    () => fetchExperimentStats({ platform_experiment_id: id }),
    { refreshInterval: 15_000 },
  )

  const { data: donations } = useSWR(
    'donations-open',
    () => fetchDonationsPage({ status: 'open', limit: MAX_LIST_PAGE_SIZE }),
    { refreshInterval: 15_000 },
  )

  const { data: stagesStatus, error: stagesError } = useSWR<StagesStatus>(
    ['pe-stages', id],
    () => fetchStages(id),
    { refreshInterval: 15_000 },
  )

  const { data: hypothesesPage, error: hypothesesError, mutate: mutateHypotheses } = useSWR(
    ['pe-hypotheses', id],
    () => fetchHypothesesPage({ platform_experiment_id: id, limit: MAX_LIST_PAGE_SIZE }),
    { refreshInterval: 15_000 },
  )
  const hypotheses = hypothesesPage?.items

  // Fetch the full timeseries for every objective metric so the scoreboard can compute each
  // agent's best value per metric (needed for Pareto winner detection below). This is the
  // heaviest fetch on the page — one full per-job metric history per objective metric, across
  // every one of the platform experiment's jobs — so it gets a slower poll than the small,
  // cheap fetches above: a 48h-scale run doesn't meaningfully change within any 10s window,
  // and re-fetching/re-sorting the full history that often is pure waste.
  const peMetricKeys = (pe?.metrics ?? []).map(m => m.key)
  const { data: allMetricSeries, error: metricSeriesError } = useSWR<{ key: string; series: AgentMetricSeries[] }[]>(
    pe && peMetricKeys.length > 0 ? ['pe-all-metrics', id, peMetricKeys.join(',')] : null,
    () => Promise.all(peMetricKeys.map(k =>
      fetchPlatformExperimentTimeseries(id, k).then(r => ({ key: k, series: r.series })),
    )),
    { refreshInterval: 30_000 },
  )

  // Every agent known to this platform experiment, whether via quota signup or a submitted job.
  // Computed unconditionally (before the loading/not-found early returns below) so hook order
  // stays stable across renders — useAgentColorMap can't follow a conditional return.
  const knownAgentIds = Array.from(new Set([
    ...(quotas ?? []).map(q => q.agent_id),
    ...(experiments ?? []).map(e => e.agent_id),
  ])).sort()
  const colorMap = useAgentColorMap(knownAgentIds)
  const colorFor = (agentId: string) => colorMap.get(agentId) ?? semantic.neutral

  // Every number below is derived by joining these reads together, so a failed one must not
  // pass through as an empty array: an unreachable quota service would render as "no agents
  // signed up", which is a different answer, not a missing one.
  const failed = [
    peError && 'the experiment',
    quotasError && 'quotas',
    experimentsError && 'jobs',
    stagesError && 'stage status',
    hypothesesError && 'hypotheses',
    metricSeriesError && 'metric history',
    statsError && 'job counts',
  ].filter(Boolean) as string[]

  if (peLoading) return <Loading />
  if (failed.length > 0) return <ErrorMessage>Could not load {failed.join(', ')} for this platform experiment.</ErrorMessage>
  if (!pe) return <ErrorMessage>Experiment not found.</ErrorMessage>

  const primaryMetric = pe.metrics?.[0]
  const primaryDir = primaryMetric?.direction ?? 'maximize'
  const metrics = pe.metrics ?? []

  // Per-agent best value for each metric, built from the metrics' full timeseries. Agents whose
  // only reported value for a metric was on a non-"raw" basis are tracked separately so the
  // scoreboard can flag them rather than silently excluding them with no explanation.
  const metricBestsByAgent = new Map<string, MetricBests>()
  const nonRawAgentIdsByMetric = new Map<string, Set<string>>()
  for (const m of metrics) {
    const entry = (allMetricSeries ?? []).find(e => e.key === m.key)
    const { best: bestMap, nonRawAgentIds } = bestPerAgentFromSeries(entry?.series ?? [], m.direction)
    bestMap.forEach((value, agentId) => {
      const rec = metricBestsByAgent.get(agentId) ?? {}
      rec[m.key] = value
      metricBestsByAgent.set(agentId, rec)
    })
    nonRawAgentIdsByMetric.set(m.key, nonRawAgentIds)
  }

  const scoreboard = buildScoreboard(experiments ?? [], quotas ?? [], metrics, metricBestsByAgent, nonRawAgentIdsByMetric)
  const totalUsed = (quotas ?? []).reduce((s, q) => s + q.used_guaranteed_acch + q.used_burst_acch, 0)
  const hypothesisProgress = hypothesisProgressCounts(hypotheses ?? [], experiments ?? [])

  // Running experiments
  const jobsByStatus = stats?.by_status ?? {}
  const running = (experiments ?? []).filter(e => e.status === 'RUNNING')
  const recent = (experiments ?? []).slice(0, 20)

  // Evicted jobs, broken down by eviction_reason — a platform experiment can evict for very
  // different reasons (stuck on admission vs. explicitly cancelled vs. ran past a stage's job
  // length cap), so a single opaque count hides whether evictions are a symptom of something
  // wrong versus routine stage cuts. Counted server-side over the whole experiment, not over
  // the page of jobs above it, and already grouped by code (a reason may carry a ': detail'
  // explanation — see lib/eviction).
  const evictionBreakdown = Object.entries(stats?.evictions_by_reason ?? {})
    .sort((a, b) => b[1] - a[1])
    .map(([r, n]) => `${evictionLabel(r)} (${n})`)
    .join(' · ')

  // The metric that actually determines standings/cuts — role: 'ranking' when the platform
  // experiment reports several metrics, defaulting to the first (primary) one for older data
  // that predates the role field. Used below to show each job's own "final" value, since with
  // multiple reported metrics there's no single obvious one to show without this.
  const rankingMetric = metrics.find(m => m.role === 'ranking') ?? primaryMetric

  // Per-job final metric value, taken from the ranking metric's own timeseries (already fetched
  // above for the "Competing Agents over time" chart) rather than any single job.final_metric_value
  // field — the backend's Experiment record has no such field; the only place a job's metric
  // history lives is the metrics timeseries, keyed by experiment_id.
  const rankingSeries = rankingMetric
    ? (allMetricSeries ?? []).find(e => e.key === rankingMetric.key)?.series ?? []
    : []
  const finalMetricByJobId = new Map<string, number>()
  for (const s of rankingSeries) {
    const pts = [...s.points].sort((a, b) => new Date(a.timestamp).getTime() - new Date(b.timestamp).getTime())
    if (pts.length > 0) finalMetricByJobId.set(s.experiment_id, pts[pts.length - 1].value)
  }

  // Default both selectors to "everything" until the user narrows them down.
  const activeAgentIds = selectedAgentIds ?? new Set(knownAgentIds)
  const activeMetricKeys = selectedMetricKeys ?? new Set(metrics.map(m => m.key))
  const jobsByAgent = (experiments ?? []).reduce<Map<string, Experiment[]>>((map, exp) => {
    const list = map.get(exp.agent_id) ?? []
    list.push(exp)
    map.set(exp.agent_id, list)
    return map
  }, new Map())

  // Donations targeted at agents in this experiment
  const relevantAgents = new Set((quotas ?? []).map(q => q.agent_id))
  const relevantDonations = (donations?.items ?? []).filter(d => relevantAgents.has(d.agent_id))

  return (
    <div>
      {/* Header */}
      <div className="wa-title" style={{ display: 'flex', alignItems: 'flex-end', justifyContent: 'space-between' }}>
        <div>
          <div
            className="text-dim"
            style={{ marginBottom: 6, cursor: 'pointer' }}
            onClick={() => router.push('/platform-experiments')}
          >
            ← Platform Experiments
          </div>
          <h1 style={{ marginBottom: 6 }}>{pe.name}</h1>
          <div style={{ display: 'flex', gap: 10, alignItems: 'center' }}>
            <StatusBadge status={pe.status} />
            {!isZeroDate(pe.starts_at) && (
              <span className="text-dim">
                {formatDate(pe.starts_at)} – {formatDate(pe.ends_at)}
              </span>
            )}
            <Link href={`/hypotheses?pe=${pe.id}`} className="text-link" style={{ fontSize: 12 }}>
              {(hypotheses?.length ?? 0)} Hypotheses →
            </Link>
          </div>
        </div>
      </div>

      {/* Description + metrics */}
      {(pe.description || (pe.metrics && pe.metrics.length > 0)) && (
        <Pod>
          <PodHeader>About this Experiment</PodHeader>
          <PodContent>
            {pe.description && (
              <CollapsibleDescription text={pe.description} style={{ marginBottom: pe.metrics?.length ? 12 : 0 }} />
            )}
            {pe.metrics && pe.metrics.length > 0 && (
              <div>
                <div className="uppercase-label" style={{ marginBottom: 6 }}>Optimization Metrics</div>
                <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                  {pe.metrics.map((m, i) => (
                    <div key={m.key} className="mono" style={{
                      padding: '4px 12px', borderRadius: 999, fontSize: 12,
                      background: m.direction === 'maximize' ? 'rgba(74, 222, 128, 0.12)' : 'rgba(251, 191, 36, 0.12)',
                      border: `1px solid ${m.direction === 'maximize' ? 'rgba(74, 222, 128, 0.3)' : 'rgba(251, 191, 36, 0.3)'}`,
                      color: m.direction === 'maximize' ? semantic.success : semantic.warning,
                    }}>
                      {i === 0 && <span style={{ fontWeight: 700, marginRight: 4 }}>PRIMARY</span>}
                      {m.key} {m.direction === 'maximize' ? '↑ maximize' : '↓ minimize'}
                    </div>
                  ))}
                </div>
              </div>
            )}
          </PodContent>
        </Pod>
      )}

      {/* Competing agents over time */}
      {metrics.length > 0 && (
        <Pod>
          <PodHeader>Competing Agents over time</PodHeader>
          <PodContent>
            <div style={{ display: 'grid', gridTemplateColumns: knownAgentIds.length > 0 ? '1fr 1fr' : '1fr', gap: 24, marginBottom: 16 }}>
              <MetricSelector metrics={metrics} selected={activeMetricKeys} onChange={setSelectedMetricKeys} />
              {knownAgentIds.length > 0 && (
                <AgentSelector agentIds={knownAgentIds} selected={activeAgentIds} onChange={setSelectedAgentIds} colorFor={colorFor} />
              )}
            </div>
            {activeMetricKeys.size === 0 ? (
              <EmptyState>Select at least one metric to plot.</EmptyState>
            ) : (
              <div style={{ display: 'grid', gap: 16 }}>
                {metrics.filter(m => activeMetricKeys.has(m.key)).map(m => (
                  <div key={m.key}>
                    <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 4 }}>
                      {m.key} <span className="text-dim" style={{ fontWeight: 400 }}>({m.direction})</span>
                    </div>
                    <CompetingAgentsChart
                      series={(allMetricSeries ?? []).find(e => e.key === m.key)?.series}
                      metricName={m.key}
                      direction={m.direction}
                      selectedAgents={activeAgentIds}
                      colorFor={colorFor}
                      boundaries={stagesStatus?.advances}
                    />
                  </div>
                ))}
              </div>
            )}
          </PodContent>
        </Pod>
      )}

      {/* Stage ladder — boundaries are published, per-agent rank deliberately is not
          (see docs/stages.md). */}
      {stagesStatus && stagesStatus.stages.length > 0 && (
        <Pod style={{ borderLeft: `3px solid ${stagesStatus.cut_agents.length > 0 ? semantic.warning : 'var(--accent)'}` }}>
          <PodContent>
            <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
              <div className="mono" style={{
                fontSize: 12, fontWeight: 700, padding: '4px 10px', borderRadius: 999,
                background: 'rgba(124, 108, 240, 0.16)', color: semantic.accent,
              }}>
                STAGE {stagesStatus.current_stage} OF {stagesStatus.stages.length}
              </div>
              <span className="text-dim">
                {Math.round(stagesStatus.progress * 100)}% through the experiment
                {stagesStatus.current_stage < stagesStatus.stages.length && (
                  <> — next boundary at {Math.round(stagesStatus.next_boundary_progress * 100)}%,
                    cutting {stagesStatus.stages[stagesStatus.current_stage - 1].evict_pct}% of survivors</>
                )}
                {stagesStatus.current_stage >= stagesStatus.stages.length && <> — final stage, no cut remaining</>}
              </span>
            </div>

            {/* The ladder itself: one segment per stage, sized by its share of the experiment. */}
            <div style={{ display: 'flex', gap: 3, marginTop: 12 }}>
              {stagesStatus.stages.map((stage, i) => {
                const stageNo = i + 1
                const done = stageNo < stagesStatus.current_stage
                const current = stageNo === stagesStatus.current_stage
                return (
                  <div key={stageNo} style={{ flex: stage.length_pct, minWidth: 0 }} title={stage.max_job_hours ? `No single job may run longer than ${stage.max_job_hours}h during this stage` : 'No job length limit during this stage'}>
                    <div style={{
                      height: 8, borderRadius: 2,
                      background: current ? semantic.accent : done ? 'rgba(124,108,240,0.55)' : 'var(--border)',
                      boxShadow: current ? '0 0 8px rgba(124,108,240,0.6)' : undefined,
                    }} />
                    <div className="mono text-dim" style={{ fontSize: 10, marginTop: 4, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                      {stage.length_pct}%{stage.evict_pct > 0 && ` · cut ${stage.evict_pct}%`}{stage.max_job_hours ? ` · ≤${stage.max_job_hours}h/job` : ''}
                    </div>
                  </div>
                )
              })}
            </div>

            <div style={{ marginTop: 12, display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
              <div>
                <div className="uppercase-label" style={{ color: semantic.success, marginBottom: 6 }}>
                  Active Agents ({stagesStatus.active_agents.length})
                </div>
                {stagesStatus.active_agents.length === 0
                  ? <span className="text-dim">none</span>
                  : stagesStatus.active_agents.map(a => (
                    <div key={a} className="mono" style={{ fontSize: 12, padding: '2px 8px', margin: '2px 4px 2px 0', background: 'rgba(74,222,128,0.12)', color: semantic.success, borderRadius: 999, display: 'inline-block' }}>{a}</div>
                  ))}
              </div>
              <div>
                <div className="uppercase-label" style={{ color: semantic.danger, marginBottom: 6 }}>
                  Cut Agents ({stagesStatus.cut_agents.length})
                </div>
                {stagesStatus.cut_agents.length === 0
                  ? <span className="text-dim">none</span>
                  : stagesStatus.cut_agents.map(c => (
                    <div key={c.agent_id} className="mono" style={{ fontSize: 12, padding: '2px 8px', margin: '2px 4px 2px 0', background: 'rgba(242,89,107,0.12)', color: semantic.danger, borderRadius: 999, display: 'inline-block' }}>
                      {c.agent_id} <span style={{ opacity: 0.7 }}>· stage {c.stage_index}</span>
                    </div>
                  ))}
              </div>
            </div>
          </PodContent>
        </Pod>
      )}

      {/* Stats row */}
      <Pod>
        <PodContent>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(7, 1fr)', gap: 16 }}>
            <StatTile label="Budget" value={`${pe.budget_accelerator_hours} AccH`} />
            <StatTile label="Agents" value={`${pe.signup_count} / ${pe.max_agents}`} />
            <StatTile label="Budget Used" value={`${formatAccH(totalUsed)} AccH`} />
            <StatTile label="Jobs" value={(stats?.total ?? 0).toString()} href={`/jobs?platform_experiment_id=${pe.id}`} />
            <StatTile
              label="Running"
              value={(jobsByStatus.RUNNING ?? 0).toString()}
              color={(jobsByStatus.RUNNING ?? 0) > 0 ? semantic.success : undefined}
              href={`/jobs?platform_experiment_id=${pe.id}&status=RUNNING`}
            />
            <StatTile
              label="Evicted"
              value={(jobsByStatus.EVICTED ?? 0).toString()}
              color={(jobsByStatus.EVICTED ?? 0) > 0 ? semantic.danger : undefined}
              sub={evictionBreakdown || undefined}
              href={`/jobs?platform_experiment_id=${pe.id}&status=EVICTED`}
            />
            <StatTile
              label="Hypotheses"
              value={(hypothesesPage?.total ?? 0).toString()}
              color={semantic.accent}
              sub={`${hypothesisProgress.finished} finished · ${hypothesisProgress.in_flight} in flight · ${hypothesisProgress.pending} pending`}
              href={`/hypotheses?pe=${pe.id}`}
            />
          </div>
        </PodContent>
      </Pod>

      {/* Add an idea to the pool */}
      <Pod>
        <PodHeader>Add an idea to the hypothesis pool</PodHeader>
        <PodContent>
          <AddIdeaForm platformExperimentID={pe.id} onAdded={() => mutateHypotheses()} />
        </PodContent>
      </Pod>

      {/* Scoreboard */}
      <Pod>
        <PodHeader>
          Scoreboard
          {metrics.length === 1 && ` — ranked by ${metrics[0].key} (${metrics[0].direction})`}
        </PodHeader>
        <PodContent scrollX>
          {scoreboard.length === 0 ? (
            <EmptyState>No agents signed up yet.</EmptyState>
          ) : (
            <table className="wa-table">
              <thead>
                <tr>
                  <th style={{ width: 36 }}>Rank</th>
                  <th>Agent</th>
                  {metrics.length > 0 ? metrics.map((m, i) => (
                    <th key={m.key} style={{ textAlign: 'right' }}>
                      {i === 0 && metrics.length > 1 && (
                        <span title="Primary metric — used for ranking" style={{ fontWeight: 700, marginRight: 4 }}>★</span>
                      )}
                      Best {m.key} <span className="text-dim" style={{ fontWeight: 400 }}>({m.direction === 'maximize' ? '↑' : '↓'})</span>
                    </th>
                  )) : <th style={{ textAlign: 'right' }}>Best Metric</th>}
                  <th style={{ textAlign: 'right' }}>Jobs</th>
                  <th style={{ textAlign: 'right' }}>Completed</th>
                  <th>Quota Used</th>
                </tr>
              </thead>
              <tbody>
                {scoreboard.map((entry, i) => {
                  const quota = (quotas ?? []).find(q => q.agent_id === entry.agentId)
                  const usedAccH = quota ? quota.used_guaranteed_acch + quota.used_burst_acch : 0
                  const totalAccH = quota ? quota.guaranteed_accelerator_hours + quota.burst_accelerator_hours : 0
                  const isCut = (stagesStatus?.cut_agents ?? []).some(c => c.agent_id === entry.agentId)
                  const isActive = !isCut && (stagesStatus?.cut_agents.length ?? 0) > 0
                    && (stagesStatus?.active_agents ?? []).includes(entry.agentId)
                  const isExpanded = expandedAgentId === entry.agentId
                  const colCount = 5 + Math.max(metrics.length, 1)
                  return (
                    <Fragment key={entry.agentId}>
                      <tr
                        onClick={() => setExpandedAgentId(isExpanded ? null : entry.agentId)}
                        style={{
                          background: isCut ? 'rgba(251,191,36,0.06)' : entry.isWinner ? 'rgba(124,108,240,0.05)' : undefined,
                          opacity: isCut ? 0.65 : 1, cursor: 'pointer',
                        }}
                      >
                        <td className="mono" style={{ fontWeight: 600 }}>#{i + 1}</td>
                        <td className="mono" style={{ fontWeight: 600 }}>
                          <span className="text-muted" style={{ display: 'inline-block', width: 12 }}>{isExpanded ? '▾' : '▸'}</span>
                          {entry.agentId}
                          {isCut && <span style={{ marginLeft: 8, fontSize: 10, color: semantic.danger, fontWeight: 400 }}>CUT</span>}
                          {isActive && <span style={{ marginLeft: 8, fontSize: 10, color: semantic.success, fontWeight: 400 }}>ACTIVE</span>}
                          {entry.nonRawMetrics.length > 0 && (
                            <span
                              className="mono"
                              title={`Also reported ${entry.nonRawMetrics.join(', ')} on a non-"raw" basis (a rescaled/transformed value). That value is never blended into ${entry.nonRawMetrics.length > 1 ? 'those rankings' : 'that ranking'} — only a raw-basis value (if any) counts here.`}
                              style={{
                                marginLeft: 8, fontSize: 10, padding: '1px 6px', borderRadius: 4, fontWeight: 400,
                                background: 'rgba(255,153,153,.12)', border: `1px solid ${semantic.danger}`, color: semantic.danger,
                              }}
                            >
                              NON-RAW BASIS
                            </span>
                          )}
                        </td>
                        {metrics.length > 0 ? metrics.map(m => {
                          const v = entry.metricBests[m.key]
                          return (
                            <td key={m.key} className="mono" style={{ textAlign: 'right', fontWeight: entry.isWinner ? 700 : 400, color: entry.isWinner ? semantic.accent : undefined }}>
                              {v != null ? v.toFixed(4) : '—'}
                            </td>
                          )
                        }) : <td className="mono" style={{ textAlign: 'right' }}>—</td>}
                        <td className="mono" style={{ textAlign: 'right' }}>{entry.jobCount}</td>
                        <td className="mono" style={{ textAlign: 'right' }}>{entry.completedCount}</td>
                        <td style={{ width: 160 }}>
                          {totalAccH > 0 ? (
                            <MetricBar value={usedAccH} max={totalAccH} />
                          ) : <span className="text-muted">no quota</span>}
                        </td>
                      </tr>
                      {isExpanded && (
                        <tr>
                          <td colSpan={colCount} style={{ background: 'var(--surface-2)' }}>
                            <AgentJobsPanel
                              agentId={entry.agentId}
                              jobs={jobsByAgent.get(entry.agentId) ?? []}
                              metricName={primaryMetric?.key}
                            />
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  )
                })}
              </tbody>
            </table>
          )}
        </PodContent>
      </Pod>

      {/* Running jobs */}
      {running.length > 0 && (
        <Pod>
          <PodHeader>Currently Running Jobs ({running.length})</PodHeader>
          <PodContent scrollX>
            <table className="wa-table">
              <thead>
                <tr>
                  <th>Experiment ID</th>
                  <th>Agent</th>
                  <th>Accelerator</th>
                  <th style={{ textAlign: 'right' }}>Est. Cost</th>
                  <th>Hypothesis</th>
                </tr>
              </thead>
              <tbody>
                {running.map(exp => (
                  <tr key={exp.id}>
                    <td className="mono" style={{ fontSize: 11 }}>
                      <Link href={`/jobs/${exp.id}`} className="text-link">{exp.id.slice(0, 8)}…</Link>
                    </td>
                    <td className="mono">{exp.agent_id}</td>
                    <td className="mono">{exp.accelerator_count}× {exp.accelerator_type}</td>
                    <td className="mono" style={{ textAlign: 'right' }}>{exp.estimated_cost_acch?.toFixed(1) ?? '—'} AccH</td>
                    <td style={{ fontSize: 12, maxWidth: 280, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{exp.hypothesis}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </PodContent>
        </Pod>
      )}

      {/* Live job metrics */}
      {running.length > 0 && (
        <Pod>
          <PodHeader>
            Live Metrics — {primaryMetric?.key ?? 'metric'}
            <span className="text-dim" style={{ marginLeft: 8 }}>auto-refreshes every 30 s</span>
          </PodHeader>
          <PodContent>
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(280px, 1fr))', gap: 12 }}>
              {running.map(exp => (
                <div key={exp.id} className="wa-mini-card">
                  <div style={{ fontSize: 11, marginBottom: 4, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                    <Link href={`/jobs/${exp.id}`} className="mono text-link" style={{ fontWeight: 600 }}>{exp.id.slice(0, 8)}…</Link>
                    <span className="mono text-dim">{exp.agent_id}</span>
                  </div>
                  <JobMetricMini jobId={exp.id} metricName={primaryMetric?.key} />
                </div>
              ))}
            </div>
          </PodContent>
        </Pod>
      )}

      {/* Recent jobs */}
      <Pod>
        <PodHeader>
          Recent Jobs
          {rankingMetric && (
            <span className="text-dim" style={{ marginLeft: 8, fontWeight: 400 }}>
              final metric = latest reported {rankingMetric.key}
            </span>
          )}
        </PodHeader>
        <PodContent scrollX>
          {!experiments || experiments.length === 0 ? (
            <EmptyState>No jobs submitted yet.</EmptyState>
          ) : (
            <table className="wa-table">
              <thead>
                <tr>
                  <th>ID</th>
                  <th>Agent</th>
                  <th>Status</th>
                  <th style={{ textAlign: 'right' }}>Final Metric{rankingMetric ? ` (${rankingMetric.key})` : ''}</th>
                  <th style={{ textAlign: 'right' }}>Est. Cost</th>
                  <th>Submitted</th>
                </tr>
              </thead>
              <tbody>
                {recent.map(exp => {
                  const metric = finalMetricByJobId.get(exp.id)
                  const cost = exp.estimated_cost_acch
                  const evictionText = exp.eviction_reason
                    ? evictionLabel(exp.eviction_reason)
                    : undefined
                  return (
                    <tr key={exp.id}>
                      <td className="mono" style={{ fontSize: 11 }}>
                        <Link href={`/jobs/${exp.id}`} className="text-link">{exp.id.slice(0, 8)}…</Link>
                      </td>
                      <td className="mono">{exp.agent_id}</td>
                      <td>
                        <StatusBadge status={exp.status} />
                        {evictionText && (
                          <div className="text-dim" style={{ fontSize: 10, marginTop: 2 }} title="eviction_reason">
                            {evictionText}
                          </div>
                        )}
                      </td>
                      <td className="mono" style={{ textAlign: 'right' }}>{metric != null ? metric.toFixed(4) : '—'}</td>
                      <td className="mono" style={{ textAlign: 'right' }}>{cost != null ? `${formatAccH(cost)} AccH` : '—'}</td>
                      <td className="text-dim">{new Date(exp.created_at).toLocaleString()}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          )}
        </PodContent>
      </Pod>

      {/* Quota table */}
      {quotas && quotas.length > 0 && (
        <Pod>
          <PodHeader>Agent Quotas</PodHeader>
          <PodContent scrollX>
            <table className="wa-table">
              <thead>
                <tr>
                  <th>Agent</th>
                  <th>Guaranteed</th>
                  <th>Burst</th>
                  <th>Remaining</th>
                  {!!pe.budget_cpu_core_hours && <th>CPU (core-h)</th>}
                </tr>
              </thead>
              <tbody>
                {quotas.map(q => {
                  const remaining = quotaRemainingAccH(q)
                  return (
                    <tr key={q.id}>
                      <td className="mono" style={{ fontWeight: 600 }}>{q.agent_id}</td>
                      <td className="mono" style={{ fontSize: 11 }}>{formatAccH(q.used_guaranteed_acch)} / {formatAccH(q.guaranteed_accelerator_hours)} AccH</td>
                      <td className="mono" style={{ fontSize: 11 }}>{formatAccH(q.used_burst_acch)} / {formatAccH(q.burst_accelerator_hours)} AccH</td>
                      <td className="mono" style={{ fontSize: 11, color: remaining > 0 ? semantic.success : semantic.danger }}>
                        {formatAccH(remaining)} AccH
                      </td>
                      {!!pe.budget_cpu_core_hours && (
                        <td className="mono" style={{ fontSize: 11 }}>
                          {formatAccH((q.used_guaranteed_cpu_core_h ?? 0) + (q.used_burst_cpu_core_h ?? 0))} / {formatAccH((q.guaranteed_cpu_core_hours ?? 0) + (q.burst_cpu_core_hours ?? 0))}
                        </td>
                      )}
                    </tr>
                  )
                })}
              </tbody>
            </table>
            {/* RAM/storage quota columns intentionally omitted: those dimensions are no longer
                hours-tracked (physical fit-only check at admission), so any guaranteed_ram_gb_hours/
                burst_ram_gb_hours/etc. on AgentQuota are frozen legacy values with nothing debiting
                them — showing them here would imply a live budget that no longer exists. */}
          </PodContent>
        </Pod>
      )}

      {/* Compute Donation Requests from agents in this experiment */}
      {relevantDonations.length > 0 && (
        <Pod>
          <PodHeader>Compute Donation Requests ({relevantDonations.length})</PodHeader>
          <PodContent scrollX>
            <table className="wa-table">
              <thead>
                <tr>
                  <th>Agent</th>
                  <th style={{ textAlign: 'right' }}>Wants</th>
                  <th>Reason</th>
                  <th>Requested</th>
                </tr>
              </thead>
              <tbody>
                {relevantDonations.map(d => (
                  <tr key={d.id}>
                    <td className="mono" style={{ fontWeight: 600 }}>{d.agent_name || d.agent_id}</td>
                    <td className="mono" style={{ textAlign: 'right' }}>{d.credits_want} AccH</td>
                    <td style={{ fontSize: 12 }}>{d.reason}</td>
                    <td className="text-dim">{new Date(d.created_at).toLocaleString()}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </PodContent>
        </Pod>
      )}
    </div>
  )
}
