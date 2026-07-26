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
  fetchExperimentsByPlatformExperiment,
  fetchDonations,
  fetchPhase2Status,
  fetchExperimentMetrics,
  fetchPlatformExperimentTimeseries,
  fetchHypotheses,
} from '@/lib/api'
import type { Phase2Status } from '@/lib/api'
import type { PlatformExperiment, AgentQuota, Experiment, MetricDataPoint, AgentMetricSeries, MetricDefinition, Hypothesis } from '@/types'
import type { DonationRequest } from '@/lib/api'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Badge } from '@/components/ui/badge'
import { MetricBar } from '@/components/ui/metric-bar'
import { StatTile } from '@/components/ui/stat-tile'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { semantic, agentPalette } from '@/lib/colors'
import { formatAccH } from '@/lib/format'

const CHART_GRID = 'rgba(255,255,255,0.08)'
const CHART_TICK = { fontSize: 10, fill: 'var(--muted-fg)' }
const CHART_TOOLTIP_STYLE: React.CSSProperties = {
  fontSize: 11, background: 'var(--surface-raised)', border: '1px solid var(--border)',
  borderRadius: 6, color: 'var(--foreground)',
}

function JobMetricMini({ jobId, metricName }: { jobId: string; metricName?: string }) {
  const { data: metrics } = useSWR<MetricDataPoint[]>(
    `metrics-${jobId}`,
    () => fetchExperimentMetrics(jobId),
    { refreshInterval: 10_000 },
  )
  const chartData = (metrics ?? [])
    .filter((p) => p.recorded_at)
    .map((p) => ({
      t: new Date(p.recorded_at as string).getTime(),
      value: p.metric_value ?? (p as any).value,
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
  series: allSeries, metricName, direction, selectedAgents, colorFor, phase2TriggeredAt,
}: {
  series: AgentMetricSeries[] | undefined
  metricName: string
  direction: 'maximize' | 'minimize'
  selectedAgents: Set<string>
  colorFor: (agentId: string) => string
  phase2TriggeredAt?: string
}) {
  if (allSeries === undefined) return <Loading />
  const series = allSeries.filter(
    s => s.points.length > 0 && selectedAgents.has(s.agent_id || s.experiment_id),
  )
  if (series.length === 0) {
    return <EmptyState>No metric data to show for &quot;{metricName}&quot; with the current agent selection.</EmptyState>
  }

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

  return (
    <ResponsiveContainer width="100%" height={300}>
      <LineChart data={rows} margin={{ top: 8, right: 24, left: 0, bottom: 0 }}>
        <CartesianGrid strokeDasharray="3 3" stroke={CHART_GRID} />
        <XAxis dataKey="t" type="number" domain={['dataMin', 'dataMax']} tickFormatter={(v) => new Date(v).toLocaleTimeString()} tick={CHART_TICK} />
        <YAxis tick={CHART_TICK} />
        <Tooltip formatter={(v: number) => v.toFixed(4)} labelFormatter={(l) => new Date(l).toLocaleTimeString()} contentStyle={CHART_TOOLTIP_STYLE} />
        {phase2TriggeredAt && (
          <ReferenceLine
            x={new Date(phase2TriggeredAt).getTime()}
            stroke={semantic.warning}
            strokeDasharray="4 4"
            label={{ value: 'Phase 2', position: 'insideTopLeft', fill: semantic.warning, fontSize: 10 }}
          />
        )}
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
): Array<{ agentId: string; metricBests: MetricBests; jobCount: number; completedCount: number; isWinner: boolean }> {
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
function bestPerAgentFromSeries(series: AgentMetricSeries[], direction: 'maximize' | 'minimize'): Map<string, number> {
  const best = new Map<string, number>()
  for (const s of series) {
    const agentId = s.agent_id || s.experiment_id
    for (const p of s.points) {
      const cur = best.get(agentId)
      if (cur === undefined) {
        best.set(agentId, p.value)
      } else {
        best.set(agentId, direction === 'maximize' ? Math.max(cur, p.value) : Math.min(cur, p.value))
      }
    }
  }
  return best
}

export default function PlatformExperimentDetailPage({ params }: { params: { id: string } }) {
  const { id } = params
  const router = useRouter()
  const [selectedAgentIds, setSelectedAgentIds] = useState<Set<string> | null>(null)
  const [selectedMetricKeys, setSelectedMetricKeys] = useState<Set<string> | null>(null)
  const [expandedAgentId, setExpandedAgentId] = useState<string | null>(null)

  const { data: pe, isLoading: peLoading } = useSWR<PlatformExperiment>(
    ['pe', id],
    () => fetchPlatformExperiment(id),
    { refreshInterval: 15_000 },
  )

  const { data: quotas } = useSWR<AgentQuota[]>(
    ['pe-quotas', id],
    () => fetchPlatformExperimentQuotas(id),
    { refreshInterval: 15_000 },
  )

  const { data: experiments } = useSWR<Experiment[]>(
    ['pe-experiments', id],
    () => fetchExperimentsByPlatformExperiment(id),
    { refreshInterval: 10_000 },
  )

  const { data: donations } = useSWR<DonationRequest[]>(
    'donations-open',
    () => fetchDonations('open'),
    { refreshInterval: 15_000 },
  )

  const { data: phase2Status } = useSWR<Phase2Status>(
    ['pe-phase2', id],
    () => fetchPhase2Status(id),
    { refreshInterval: 10_000 },
  )

  const { data: hypotheses } = useSWR<Hypothesis[]>(
    ['pe-hypotheses', id],
    () => fetchHypotheses(id),
    { refreshInterval: 15_000 },
  )

  // Fetch the full timeseries for every objective metric so the scoreboard can compute each
  // agent's best value per metric (needed for Pareto winner detection below).
  const peMetricKeys = (pe?.metrics ?? []).map(m => m.key)
  const { data: allMetricSeries } = useSWR<{ key: string; series: AgentMetricSeries[] }[]>(
    pe && peMetricKeys.length > 0 ? ['pe-all-metrics', id, peMetricKeys.join(',')] : null,
    () => Promise.all(peMetricKeys.map(k =>
      fetchPlatformExperimentTimeseries(id, k).then(r => ({ key: k, series: r.series })),
    )),
    { refreshInterval: 10_000 },
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

  if (peLoading) return <Loading />
  if (!pe) return <ErrorMessage>Experiment not found.</ErrorMessage>

  const primaryMetric = pe.metrics?.[0]
  const primaryDir = primaryMetric?.direction ?? 'maximize'
  const metrics = pe.metrics ?? []

  // Per-agent best value for each metric, built from the metrics' full timeseries.
  const metricBestsByAgent = new Map<string, MetricBests>()
  for (const m of metrics) {
    const entry = (allMetricSeries ?? []).find(e => e.key === m.key)
    const bestMap = bestPerAgentFromSeries(entry?.series ?? [], m.direction)
    bestMap.forEach((value, agentId) => {
      const rec = metricBestsByAgent.get(agentId) ?? {}
      rec[m.key] = value
      metricBestsByAgent.set(agentId, rec)
    })
  }

  const scoreboard = buildScoreboard(experiments ?? [], quotas ?? [], metrics, metricBestsByAgent)
  const totalUsed = (quotas ?? []).reduce((s, q) => s + q.used_guaranteed_acch + q.used_burst_acch, 0)

  // Running experiments
  const running = (experiments ?? []).filter(e => e.status === 'RUNNING')
  const recent = (experiments ?? []).slice(0, 20)

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
  const relevantDonations = (donations ?? []).filter(d => relevantAgents.has(d.agent_id))

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
            {pe.starts_at && (
              <span className="text-dim">
                {new Date(pe.starts_at).toLocaleDateString()} – {pe.ends_at ? new Date(pe.ends_at).toLocaleDateString() : '?'}
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
              <p style={{ fontSize: 13, whiteSpace: 'pre-wrap', marginBottom: pe.metrics?.length ? 12 : 0 }}>{pe.description}</p>
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
                      phase2TriggeredAt={phase2Status?.phase2_triggered_at}
                    />
                  </div>
                ))}
              </div>
            )}
          </PodContent>
        </Pod>
      )}

      {/* Phase Banner */}
      {phase2Status && (
        <Pod style={{ borderLeft: `3px solid ${phase2Status.phase === 2 ? semantic.warning : 'var(--accent)'}` }}>
          <PodContent>
            <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
              <div className="mono" style={{
                fontSize: 12, fontWeight: 700, padding: '4px 10px', borderRadius: 999,
                background: phase2Status.phase === 2 ? 'rgba(251, 191, 36, 0.14)' : 'rgba(124, 108, 240, 0.16)',
                color: phase2Status.phase === 2 ? semantic.warning : semantic.accent,
              }}>
                {phase2Status.phase === 2 ? 'PHASE 2 — Signal-Gated' : 'PHASE 1 — Open Competition'}
              </div>
              {phase2Status.phase === 1 && (
                <span className="text-dim">
                  Phase 2 triggers at {Math.round(phase2Status.boundary_fraction * 100)}% budget consumed
                </span>
              )}
              {phase2Status.phase === 2 && phase2Status.phase2_triggered_at && (
                <span className="text-dim">
                  Triggered {new Date(phase2Status.phase2_triggered_at).toLocaleString()}
                </span>
              )}
            </div>
            {phase2Status.phase === 2 && (
              <div style={{ marginTop: 12, display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
                <div>
                  <div className="uppercase-label" style={{ color: semantic.success, marginBottom: 6 }}>
                    Active Agents ({phase2Status.active_agents.length})
                  </div>
                  {phase2Status.active_agents.length === 0
                    ? <span className="text-dim">none</span>
                    : phase2Status.active_agents.map(a => (
                      <div key={a} className="mono" style={{ fontSize: 12, padding: '2px 8px', margin: '2px 4px 2px 0', background: 'rgba(74,222,128,0.12)', color: semantic.success, borderRadius: 999, display: 'inline-block' }}>{a}</div>
                    ))}
                </div>
                <div>
                  <div className="uppercase-label" style={{ color: semantic.danger, marginBottom: 6 }}>
                    Held Agents ({phase2Status.held_agents.length})
                  </div>
                  {phase2Status.held_agents.length === 0
                    ? <span className="text-dim">none</span>
                    : phase2Status.held_agents.map(a => (
                      <div key={a} className="mono" style={{ fontSize: 12, padding: '2px 8px', margin: '2px 4px 2px 0', background: 'rgba(242,89,107,0.12)', color: semantic.danger, borderRadius: 999, display: 'inline-block' }}>{a}</div>
                    ))}
                </div>
              </div>
            )}
          </PodContent>
        </Pod>
      )}

      {/* Stats row */}
      <Pod>
        <PodContent>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(6, 1fr)', gap: 16 }}>
            <StatTile label="Budget" value={`${pe.budget_accelerator_hours} AccH`} />
            <StatTile label="Agents" value={`${pe.signup_count} / ${pe.max_agents}`} />
            <StatTile label="Budget Used" value={`${formatAccH(totalUsed)} AccH`} />
            <StatTile label="Jobs" value={(experiments?.length ?? 0).toString()} />
            <StatTile label="Running" value={running.length.toString()} color={running.length > 0 ? semantic.success : undefined} />
            <StatTile label="Hypotheses" value={(hypotheses?.length ?? 0).toString()} color={semantic.accent} />
          </div>
        </PodContent>
      </Pod>

      {/* Scoreboard */}
      <Pod>
        <PodHeader>
          Scoreboard
          {metrics.length === 1 && ` — ranked by ${metrics[0].key} (${metrics[0].direction})`}
          {metrics.length > 1 && ' — winners are the Pareto front (not beaten on every metric at once)'}
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
                  const isHeld = phase2Status?.phase === 2 && phase2Status.held_agents.includes(entry.agentId)
                  const isActive = phase2Status?.phase === 2 && phase2Status.active_agents.includes(entry.agentId)
                  const isExpanded = expandedAgentId === entry.agentId
                  const colCount = 5 + Math.max(metrics.length, 1)
                  return (
                    <Fragment key={entry.agentId}>
                      <tr
                        onClick={() => setExpandedAgentId(isExpanded ? null : entry.agentId)}
                        style={{
                          background: isHeld ? 'rgba(251,191,36,0.06)' : entry.isWinner ? 'rgba(124,108,240,0.05)' : undefined,
                          opacity: isHeld ? 0.65 : 1, cursor: 'pointer',
                        }}
                      >
                        <td className="mono" style={{ fontWeight: 600 }}>#{i + 1}</td>
                        <td className="mono" style={{ fontWeight: 600 }}>
                          <span className="text-muted" style={{ display: 'inline-block', width: 12 }}>{isExpanded ? '▾' : '▸'}</span>
                          {entry.agentId}
                          {entry.isWinner && <span style={{ marginLeft: 8, fontSize: 10, color: semantic.accent, fontWeight: 700 }}>WINNER</span>}
                          {isHeld && <span style={{ marginLeft: 8, fontSize: 10, color: semantic.danger, fontWeight: 400 }}>HELD</span>}
                          {isActive && <span style={{ marginLeft: 8, fontSize: 10, color: semantic.success, fontWeight: 400 }}>ACTIVE</span>}
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
            <span className="text-dim" style={{ marginLeft: 8 }}>auto-refreshes every 10 s</span>
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
        <PodHeader>Recent Jobs</PodHeader>
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
                  <th style={{ textAlign: 'right' }}>Final Metric</th>
                  <th style={{ textAlign: 'right' }}>Cost</th>
                  <th>Submitted</th>
                </tr>
              </thead>
              <tbody>
                {recent.map(exp => {
                  const metric = (exp as any).final_metric ?? (exp as any).final_metric_value
                  const cost = (exp as any).actual_cost_acch ?? exp.actual_cost_acch
                  return (
                    <tr key={exp.id}>
                      <td className="mono" style={{ fontSize: 11 }}>
                        <Link href={`/jobs/${exp.id}`} className="text-link">{exp.id.slice(0, 8)}…</Link>
                      </td>
                      <td className="mono">{exp.agent_id}</td>
                      <td><StatusBadge status={exp.status} /></td>
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
                  const gRem = q.guaranteed_accelerator_hours - q.used_guaranteed_acch
                  const bRem = q.burst_accelerator_hours - q.used_burst_acch
                  return (
                    <tr key={q.id}>
                      <td className="mono" style={{ fontWeight: 600 }}>{q.agent_id}</td>
                      <td className="mono" style={{ fontSize: 11 }}>{formatAccH(q.used_guaranteed_acch)} / {formatAccH(q.guaranteed_accelerator_hours)} AccH</td>
                      <td className="mono" style={{ fontSize: 11 }}>{formatAccH(q.used_burst_acch)} / {formatAccH(q.burst_accelerator_hours)} AccH</td>
                      <td className="mono" style={{ fontSize: 11, color: gRem + bRem > 0 ? semantic.success : semantic.danger }}>
                        {formatAccH(Math.max(0, gRem) + Math.max(0, bRem))} AccH
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
