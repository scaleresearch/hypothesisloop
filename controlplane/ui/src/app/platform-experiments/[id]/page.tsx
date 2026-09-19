'use client'

import { Fragment, useMemo, useState } from 'react'
import { useRouter } from 'next/navigation'
import Link from 'next/link'
import useSWR from 'swr'
import { Dialog, DialogPanel, DialogTitle } from '@headlessui/react'
import {
  LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip, Legend, ResponsiveContainer, ReferenceLine,
} from 'recharts'
import {
  fetchPlatformExperiment,
  fetchPlatformExperimentQuotas,
  fetchPlatformExperimentResults,
  fetchExperimentsPage,
  fetchExperimentStats,
  fetchDonationsPage,
  MAX_LIST_PAGE_SIZE,
  fetchStages,
  fetchExperimentMetrics,
  fetchPlatformExperimentTimeseries,
  fetchHypothesisStats,
  fetchFindings,
  HYPOTHESIS_STATUSES,
} from '@/lib/api'
import type { StagesStatus } from '@/lib/api'
import { quotaRemainingAccH } from '@/lib/quota'
import type { PlatformExperiment, AgentQuota, Experiment, MetricDataPoint, AgentMetricSeries } from '@/types'
import type { DonationRequest } from '@/lib/api'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Badge } from '@/components/ui/badge'
import { MetricBar } from '@/components/ui/metric-bar'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { SearchableSelect } from '@/components/ui/searchable-select'
import { StatTile } from '@/components/ui/stat-tile'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { semantic, agentPalette } from '@/lib/colors'
import { acceleratorLabel, formatAccH, formatDate, isZeroDate } from '@/lib/format'
import { evictionLabel, faultBreakdown } from '@/lib/eviction'
import { hypothesisActivity } from '@/lib/hypothesis-progress'
import { graphHref } from '@/lib/hypothesis-graph'

const TWO_LINES: React.CSSProperties = {
  display: '-webkit-box', WebkitLineClamp: 2, WebkitBoxOrient: 'vertical', overflow: 'hidden',
}

const CHART_GRID = 'rgba(255,255,255,0.08)'
const CHART_TICK = { fontSize: 10, fill: 'var(--muted-fg)' }
const CHART_TOOLTIP_STYLE: React.CSSProperties = {
  fontSize: 11, background: 'var(--surface-raised)', border: '1px solid var(--border)',
  borderRadius: 6, color: 'var(--foreground)',
}

// Bounded-score metrics (roughly 0-1) get their own axis so they aren't flattened to a
// hairline by unbounded metrics like seconds_per_subject/cv_seconds sitting in the tens+.
// Kept as a simple name check rather than a generic auto-scaling heuristic.
const BOUNDED_METRIC_RE = /auroc|accuracy|precision|recall|f1|auc/i

function isBoundedMetric(name: string): boolean {
  return BOUNDED_METRIC_RE.test(name)
}

const METRIC_COLORS = agentPalette

// Shared chart body for both the mini card and the expanded dialog view. `trackedMetrics`
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
    <ResponsiveContainer width="100%" height={height}>
      <LineChart data={chartData} margin={{ top: 4, right: 8, left: 0, bottom: 0 }}>
        <CartesianGrid strokeDasharray="3 3" stroke={CHART_GRID} />
        <XAxis dataKey="frac" type="number" domain={[0, 100]} tickFormatter={(v) => `${v}%`} tick={CHART_TICK} />
        {hasBounded && <YAxis yAxisId="left" domain={['auto', 'auto']} tick={CHART_TICK} width={40} />}
        {hasUnbounded && <YAxis yAxisId="right" orientation="right" domain={['auto', 'auto']} tick={CHART_TICK} width={40} />}
        <Tooltip formatter={(v: number) => v.toFixed(4)} labelFormatter={(l) => `${l}% of training`} contentStyle={CHART_TOOLTIP_STYLE} />
        <Legend wrapperStyle={{ fontSize: 10 }} />
        {metricNames.map((name, i) => (
          <Line
            key={name}
            yAxisId={axisOf(name)}
            dataKey={name}
            name={hasBounded && hasUnbounded ? `${name} (${axisOf(name)} axis)` : name}
            stroke={METRIC_COLORS[i % METRIC_COLORS.length]}
            dot={{ r: 2 }}
            strokeWidth={1.5}
            type="monotone"
            connectNulls
            isAnimationActive={false}
          />
        ))}
      </LineChart>
    </ResponsiveContainer>
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
  const [expanded, setExpanded] = useState(false)
  const points = (metrics ?? []).filter((p) => p.recorded_at)
  return (
    <div style={{ position: 'relative' }}>
      <button
        type="button"
        onClick={() => setExpanded(true)}
        title="Expand chart"
        aria-label="Expand chart"
        className="wa-chart-expand-btn"
        style={{
          position: 'absolute', top: 0, right: 0, zIndex: 1, border: '1px solid var(--border)',
          background: 'var(--surface)', borderRadius: 4, width: 20, height: 20, lineHeight: '18px',
          fontSize: 12, cursor: 'pointer', color: 'var(--muted-fg)',
        }}
      >
        ⤢
      </button>
      <MetricLineChart points={points} trackedMetrics={trackedMetrics} height={400} />
      <Dialog open={expanded} onClose={() => setExpanded(false)} className="wa-dialog">
        <div className="wa-dialog-backdrop" aria-hidden="true" />
        <div className="wa-dialog-container">
          <DialogPanel className="wa-dialog-panel" style={{ maxWidth: '90vw', width: '90vw' }}>
            <DialogTitle style={{ fontWeight: 600, marginBottom: 10 }} className="mono">{jobId}</DialogTitle>
            <MetricLineChart points={points} trackedMetrics={trackedMetrics} height={640} />
          </DialogPanel>
        </div>
      </Dialog>
    </div>
  )
}

// Lets the user inspect any job belonging to this platform experiment (not just currently
// running ones) and pick which of that job's actually-reported metrics to chart, rather than
// being limited to the experiment's tracked/ranking subset like the per-agent panel above.
function LiveMetricsPicker({ jobs }: { jobs: Experiment[] }) {
  // Most-recently-active first, keyed by whichever timestamp is freshest for that job — updated_at
  // moves as a running job reports progress, submitted_at/created_at anchor jobs that haven't.
  const sorted = useMemo(
    () => [...jobs].sort((a, b) => {
      const tA = new Date(a.updated_at ?? a.submitted_at ?? a.created_at ?? 0).getTime()
      const tB = new Date(b.updated_at ?? b.submitted_at ?? b.created_at ?? 0).getTime()
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
    <div>
      <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap', alignItems: 'flex-start', marginBottom: 12 }}>
        <div>
          <div className="uppercase-label" style={{ marginBottom: 6 }}>Job</div>
          <SearchableSelect
            options={sorted.map(j => ({ value: j.id, label: `${j.id.slice(0, 8)}… · ${j.status} · ${j.agent_id}` }))}
            value={selectedJob.id}
            onChange={v => { if (v) setJobId(v) }}
            placeholder="Search jobs…"
            className="mono"
            style={{ minWidth: 320 }}
            hideAllOption
          />
        </div>
        <div style={{ flex: 1, minWidth: 200 }}>
          <div className="uppercase-label" style={{ marginBottom: 6 }}>Metrics</div>
          {availableMetrics.length === 0 ? (
            <span className="text-dim" style={{ fontSize: 12 }}>No metrics reported yet</span>
          ) : (
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
              {availableMetrics.map(name => {
                const isOn = selected.has(name)
                return (
                  <label
                    key={name}
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
                        isOn ? next.delete(name) : next.add(name)
                        setSelected(next)
                      }}
                      style={{ margin: 0 }}
                    />
                    {name}
                  </label>
                )
              })}
            </div>
          )}
        </div>
      </div>
      <MetricLineChart points={points} trackedMetrics={Array.from(selected)} height={420} />
    </div>
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
        <YAxis domain={['auto', 'auto']} tick={CHART_TICK} />
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
            type="stepAfter"
            connectNulls
          />
        ))}
      </LineChart>
    </ResponsiveContainer>
    </>
  )
}

function AgentJobsPanel({ platformExperimentId, agentId, trackedMetrics }: { platformExperimentId: string; agentId: string; trackedMetrics: string[] }) {
  const { data, error } = useSWR(
    ['pe-agent-jobs', platformExperimentId, agentId],
    () => fetchExperimentsPage({ platform_experiment_id: platformExperimentId, agent: agentId, sort: '-created_at' }),
    { refreshInterval: 15_000 },
  )
  if (error) return <ErrorMessage>Could not load jobs from {agentId}.</ErrorMessage>
  if (!data) return <Loading />
  const jobs = data.items
  if (jobs.length === 0) {
    return <EmptyState>No jobs from {agentId} yet.</EmptyState>
  }
  return (
    <div style={{ padding: '10px 0 4px', display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(480px, 1fr))', gap: 10 }}>
      {jobs.map(job => (
        <div key={job.id} className="wa-mini-card">
          <div style={{ fontSize: 11, marginBottom: 4, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
            <Link href={`/jobs/${job.id}`} className="mono text-link" style={{ fontWeight: 600 }}>{job.id.slice(0, 8)}…</Link>
            <StatusBadge status={job.status} />
          </div>
          <JobMetricMini jobId={job.id} trackedMetrics={trackedMetrics} />
        </div>
      ))}
    </div>
  )
}

function StatusBadge({ status }: { status: string }) {
  return <Badge status={status}>{status}</Badge>
}

// Only shown when a policy narrows past the 'mixed' default, so the common case (anyone may
// submit either) stays uncluttered — a restriction is the exceptional, worth-flagging state.
function SubmitPolicyChips({ pe }: { pe: PlatformExperiment }) {
  const chip = (label: string) => (
    <span
      key={label}
      className="mono"
      style={{ fontSize: 11, padding: '2px 8px', borderRadius: 999, background: 'var(--surface-2)', border: '1px solid var(--border)', color: 'var(--muted-fg)' }}
    >
      {label}
    </span>
  )
  const chips: React.ReactNode[] = []
  if (pe.hypothesis_submit_policy && pe.hypothesis_submit_policy !== 'mixed') {
    chips.push(chip(`Hypotheses: ${pe.hypothesis_submit_policy === 'human_only' ? 'humans only' : 'agents only'}`))
  }
  if (pe.job_submit_policy && pe.job_submit_policy !== 'mixed') {
    chips.push(chip(`Jobs: ${pe.job_submit_policy === 'human_only' ? 'humans only' : 'agents only'}`))
  }
  if (chips.length === 0) return null
  return <>{chips}</>
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

  // The newest jobs only, for the recent list and the live metrics picker.
  const { data: experimentsPage, error: experimentsError } = useSWR(
    ['pe-experiments', id],
    () => fetchExperimentsPage({ platform_experiment_id: id, sort: '-created_at', limit: MAX_LIST_PAGE_SIZE }),
    { refreshInterval: 15_000 },
  )
  const experiments = experimentsPage?.items

  // Asked for by status: a job that outlived the newest page of jobs is still running.
  const { data: runningPage, error: runningError } = useSWR(
    ['pe-running', id],
    () => fetchExperimentsPage({ platform_experiment_id: id, status: 'RUNNING', sort: '-created_at' }),
    { refreshInterval: 15_000 },
  )

  // Whole-experiment job counts, grouped server-side. The job list above is one bounded page,
  // so tallying it would report the page's shape under a label that reads as the experiment's.
  const { data: stats, error: statsError } = useSWR(
    ['pe-stats-by-agent', id],
    () => fetchExperimentStats({ platform_experiment_id: id, group_by: 'agent' }),
    { refreshInterval: 15_000 },
  )
  const jobsByAgent = stats?.groups ?? {}
  const { data: hypothesisStats, error: hypothesisStatsError } = useSWR(
    ['pe-stats-by-hypothesis', id],
    () => fetchExperimentStats({ platform_experiment_id: id, group_by: 'hypothesis' }),
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

  const { data: verdicts, error: hypothesesError } = useSWR(
    ['pe-hypothesis-stats', id],
    () => fetchHypothesisStats(id),
    { refreshInterval: 15_000 },
  )

  const { data: findings, error: findingsError } = useSWR(
    ['pe-findings', id],
    () => fetchFindings(id),
    { refreshInterval: 15_000 },
  )

  const { data: results, error: resultsError } = useSWR(
    ['pe-results', id],
    () => fetchPlatformExperimentResults(id),
    { refreshInterval: 15_000 },
  )

  // The history of every declared metric, for the chart. The heaviest read on the page, so it
  // polls slower than the rest.
  const peMetricKeys = (pe?.metrics ?? []).map(m => m.key)
  const { data: allMetricSeries, error: metricSeriesError } = useSWR<{ key: string; series: AgentMetricSeries[] }[]>(
    pe && peMetricKeys.length > 0 ? ['pe-all-metrics', id, peMetricKeys.join(','), pe.starts_at, pe.ends_at] : null,
    () => Promise.all(peMetricKeys.map(k =>
      fetchPlatformExperimentTimeseries(pe!, k).then(r => ({ key: k, series: r.series })),
    )),
    { refreshInterval: 30_000 },
  )

  // Every agent known to this platform experiment, whether via quota signup or a submitted job.
  // Computed unconditionally (before the loading/not-found early returns below) so hook order
  // stays stable across renders — useAgentColorMap can't follow a conditional return.
  const knownAgentIds = Array.from(new Set([
    ...(quotas ?? []).map(q => q.agent_id),
    ...Object.keys(jobsByAgent),
  ])).sort()
  const colorMap = useAgentColorMap(knownAgentIds)
  const colorFor = (agentId: string) => colorMap.get(agentId) ?? semantic.neutral

  // Every number below is derived by joining these reads together, so a failed one must not
  // pass through as an empty array: an unreachable quota service would render as "no agents
  // signed up", which is a different answer, not a missing one.
  const failed = [
    peError && 'the experiment',
    quotasError && 'quotas',
    (experimentsError || runningError) && 'jobs',
    stagesError && 'stage status',
    (hypothesesError || findingsError) && 'hypotheses',
    metricSeriesError && 'metric history',
    resultsError && 'results',
    (statsError || hypothesisStatsError) && 'job counts',
  ].filter(Boolean) as string[]

  if (peLoading) return <Loading />
  if (failed.length > 0) return <ErrorMessage>Could not load {failed.join(', ')} for this platform experiment.</ErrorMessage>
  if (!pe) return <ErrorMessage>Experiment not found.</ErrorMessage>

  const trackedMetricKeys = (pe.metrics ?? []).map((m) => m.key)
  const metrics = pe.metrics ?? []

  // Ordered as the server ranks the competitors on the primary ranking metric; an agent without a
  // standing (not a competitor, ineligible, or nothing reported) follows, by id.
  const primaryStandings = results?.metrics[0]
  const standingOf = new Map((primaryStandings?.standings ?? []).map(st => [st.agent_id, st]))
  const unranked = Number.MAX_SAFE_INTEGER
  const scoreboard = [...knownAgentIds].sort((a, b) => (standingOf.get(a)?.rank ?? unranked) - (standingOf.get(b)?.rank ?? unranked))
  const bestOf = (metricKey: string, agentId: string) =>
    results?.bests.find(b => b.metric === metricKey)?.agents.find(a => a.agent_id === agentId)
  const totalUsed = (quotas ?? []).reduce((s, q) => s + q.used_guaranteed_acch + q.used_burst_acch, 0)
  const hypothesisCount = verdicts?.total ?? 0
  const judgedCount = hypothesisCount - (verdicts?.by_status.open ?? 0)
  const hypothesisSub = [
    HYPOTHESIS_STATUSES.map(v => `${verdicts?.by_status[v] ?? 0} ${v}`).join(' · '),
    hypothesisActivity(hypothesisCount, hypothesisStats?.groups ?? {}),
  ].filter(Boolean).join(' · ')

  // SUBMITTED and RUNNING, the set max_concurrent_accelerators is checked against.
  const inFlightAccelerators = stats?.in_flight_accelerators ?? 0

  // Running experiments
  const jobsByStatus = stats?.by_status ?? {}
  const running = runningPage?.items ?? []
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

  // Whose fault those evictions were, folded from the same tally — an infrastructure count here
  // says the platform failed these jobs, not the agents running them.
  const evictionFaultBreakdown = faultBreakdown(stats?.evictions_by_class ?? {})

  // Default both selectors to "everything" until the user narrows them down.
  const activeAgentIds = selectedAgentIds ?? new Set(knownAgentIds)
  const activeMetricKeys = selectedMetricKeys ?? new Set(metrics.map(m => m.key))

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
            <Link href={graphHref(pe.id)} className="text-link" style={{ fontSize: 12 }}>
              {hypothesisCount} Hypotheses →
            </Link>
            <SubmitPolicyChips pe={pe} />
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
              {stagesStatus.stages.reduce<{ start: number; nodes: JSX.Element[] }>((acc, stage, i) => {
                const stageNo = i + 1
                const end = acc.start + stage.length_pct / 100
                const current = stageNo === stagesStatus.current_stage
                const filled = Math.max(0, Math.min(1, (stagesStatus.progress - acc.start) / (end - acc.start)))
                acc.nodes.push(
                  <div key={stageNo} style={{ flex: stage.length_pct, minWidth: 0 }} title={stage.max_job_hours ? `No single job may run longer than ${stage.max_job_hours}h during this stage` : 'No job length limit during this stage'}>
                    <div style={{
                      height: 8, borderRadius: 2, background: 'var(--border)', overflow: 'hidden',
                      boxShadow: current ? '0 0 8px rgba(124,108,240,0.6)' : undefined,
                    }}>
                      <div style={{
                        height: '100%', width: `${filled * 100}%`,
                        background: current ? semantic.accent : 'rgba(124,108,240,0.55)',
                      }} />
                    </div>
                    <div className="mono text-dim" style={{ fontSize: 10, marginTop: 4, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                      {stage.length_pct}%{stage.evict_pct > 0 && ` · cut ${stage.evict_pct}%`}{stage.max_job_hours ? ` · ≤${stage.max_job_hours}h/job` : ''}
                    </div>
                  </div>
                )
                return { start: end, nodes: acc.nodes }
              }, { start: 0, nodes: [] }).nodes}
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
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))', gap: 16 }}>
            <StatTile label="Budget" value={`${pe.budget_accelerator_hours} AccH`} />
            <StatTile
              label="Accelerators In Flight"
              value={pe.max_concurrent_accelerators == null ? `${inFlightAccelerators}` : `${inFlightAccelerators} / ${pe.max_concurrent_accelerators}`}
              color={pe.max_concurrent_accelerators != null && inFlightAccelerators >= pe.max_concurrent_accelerators ? semantic.accent : undefined}
              sub={pe.max_concurrent_accelerators == null ? 'no cap set' : undefined}
            />
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
              sub={[evictionFaultBreakdown, evictionBreakdown].filter(Boolean).join(' — ') || undefined}
              href={`/jobs?platform_experiment_id=${pe.id}&status=EVICTED`}
            />
            <StatTile
              label="Hypotheses"
              value={hypothesisCount.toString()}
              color={semantic.accent}
              sub={hypothesisSub}
              href={graphHref(pe.id)}
            />
          </div>
        </PodContent>
      </Pod>

      {/* Findings */}
      {findings && findings.length > 0 && (
        <Pod>
          <PodHeader>
            Findings
            <span className="text-muted" style={{ fontWeight: 400, marginLeft: 8 }}>
              ({judgedCount} hypotheses with a verdict{judgedCount > findings.length ? `, showing the newest ${findings.length}` : ''})
            </span>
          </PodHeader>
          <PodContent scrollX>
            <table className="wa-table" style={{ tableLayout: 'fixed', minWidth: 760 }}>
              <thead>
                <tr>
                  <th style={{ width: 110 }}>Verdict</th>
                  <th>Hypothesis</th>
                  <th>Newest finding</th>
                  <th style={{ width: 170, textAlign: 'right' }}>Best job</th>
                </tr>
              </thead>
              <tbody>
                {findings.map(h => (
                  <tr key={h.id} style={{ verticalAlign: 'top' }}>
                    <td><Badge status={h.status}>{h.status}</Badge></td>
                    <td style={{ fontSize: 12 }}>
                      <Link href={`/hypotheses/${h.id}`} className="text-link" style={TWO_LINES} title={h.text}>{h.text}</Link>
                    </td>
                    <td style={{ fontSize: 12 }}>
                      {h.finding ? <span style={TWO_LINES} title={h.finding}>{h.finding}</span> : <span className="text-muted">—</span>}
                    </td>
                    <td className="mono" style={{ textAlign: 'right' }}>
                      {h.best_job ? (
                        <Link href={`/jobs/${h.best_job.id}`} className="text-link" title={h.best_job.id}>
                          {h.best_job.metric.name} {h.best_job.metric.best.toFixed(4)}
                        </Link>
                      ) : <span className="text-muted">—</span>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </PodContent>
        </Pod>
      )}

      {/* Scoreboard */}
      <Pod>
        <PodHeader>
          Scoreboard
          {primaryStandings && ` — ranked by ${primaryStandings.metric} (${primaryStandings.direction})`}
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
                {scoreboard.map(agentId => {
                  const quota = (quotas ?? []).find(q => q.agent_id === agentId)
                  const usedAccH = quota ? quota.used_guaranteed_acch + quota.used_burst_acch : 0
                  const totalAccH = quota ? quota.guaranteed_accelerator_hours + quota.burst_accelerator_hours : 0
                  const isCut = (stagesStatus?.cut_agents ?? []).some(c => c.agent_id === agentId)
                  const isActive = !isCut && (stagesStatus?.cut_agents.length ?? 0) > 0
                    && (stagesStatus?.active_agents ?? []).includes(agentId)
                  const isExpanded = expandedAgentId === agentId
                  const colCount = 5 + Math.max(metrics.length, 1)
                  const rank = standingOf.get(agentId)?.rank
                  const isWinner = rank === 1
                  const nonRawMetrics = (results?.bests ?? []).filter(b => b.non_raw_basis_agents?.includes(agentId)).map(b => b.metric)
                  const jobs = jobsByAgent[agentId]
                  return (
                    <Fragment key={agentId}>
                      <tr
                        onClick={() => setExpandedAgentId(isExpanded ? null : agentId)}
                        style={{
                          background: isCut ? 'rgba(251,191,36,0.06)' : isWinner ? 'rgba(124,108,240,0.05)' : undefined,
                          opacity: isCut ? 0.65 : 1, cursor: 'pointer',
                        }}
                      >
                        <td className="mono" style={{ fontWeight: 600 }}>{rank != null ? `#${rank}` : '—'}</td>
                        <td className="mono" style={{ fontWeight: 600 }}>
                          <span className="text-muted" style={{ display: 'inline-block', width: 12 }}>{isExpanded ? '▾' : '▸'}</span>
                          {agentId}
                          {isCut && <span style={{ marginLeft: 8, fontSize: 10, color: semantic.danger, fontWeight: 400 }}>CUT</span>}
                          {isActive && <span style={{ marginLeft: 8, fontSize: 10, color: semantic.success, fontWeight: 400 }}>ACTIVE</span>}
                          {nonRawMetrics.length > 0 && (
                            <span
                              className="mono"
                              title={`Also reported ${nonRawMetrics.join(', ')} on a non-"raw" basis (a rescaled/transformed value). That value is never blended into ${nonRawMetrics.length > 1 ? 'those rankings' : 'that ranking'} — only a raw-basis value (if any) counts here.`}
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
                          const best = bestOf(m.key, agentId)
                          return (
                            <td key={m.key} className="mono" style={{ textAlign: 'right', fontWeight: isWinner ? 700 : 400 }}>
                              {best?.experiment_id ? (
                                <Link
                                  href={`/jobs/${best.experiment_id}`}
                                  className="text-link"
                                  title={`The job that set it: ${best.experiment_id}`}
                                  onClick={e => e.stopPropagation()}
                                >
                                  {best.best.toFixed(4)}
                                </Link>
                              ) : best ? best.best.toFixed(4) : '—'}
                            </td>
                          )
                        }) : <td className="mono" style={{ textAlign: 'right' }}>—</td>}
                        <td className="mono" style={{ textAlign: 'right' }}>{jobs?.total ?? 0}</td>
                        <td className="mono" style={{ textAlign: 'right' }}>{jobs?.by_status.COMPLETED ?? 0}</td>
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
                              platformExperimentId={pe.id}
                              agentId={agentId}
                              trackedMetrics={trackedMetricKeys}
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
          <PodHeader>Currently Running Jobs ({runningPage?.total ?? running.length})</PodHeader>
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
                    <td className="mono">{exp.accelerator_count}× {acceleratorLabel(exp.accelerator_type)}</td>
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
      {(experiments ?? []).length > 0 && (
        <Pod>
          <PodHeader>
            Job Metrics
            <span className="text-dim" style={{ marginLeft: 8 }}>over training progress, refreshed every 30 s</span>
          </PodHeader>
          <PodContent>
            <LiveMetricsPicker jobs={experiments ?? []} />
          </PodContent>
        </Pod>
      )}

      {/* Recent jobs */}
      <Pod>
        <PodHeader>
          Recent Jobs
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
                  <th>Hypothesis</th>
                  <th style={{ textAlign: 'right' }}>Best {primaryStandings?.metric ?? 'Metric'}</th>
                  <th style={{ textAlign: 'right' }}>Est. Cost</th>
                  <th>Submitted</th>
                </tr>
              </thead>
              <tbody>
                {recent.map(exp => {
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
                      <td style={{ maxWidth: 320, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', fontSize: 12 }} title={exp.hypothesis}>
                        {exp.hypothesis_id
                          ? <Link href={`/hypotheses/${exp.hypothesis_id}`} className="text-link">{exp.hypothesis}</Link>
                          : exp.hypothesis}
                      </td>
                      <td className="mono" style={{ textAlign: 'right' }}>{exp.metric?.best.toFixed(4) ?? '—'}</td>
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
                    </tr>
                  )
                })}
              </tbody>
            </table>
            {/* RAM/storage quota columns intentionally omitted: those dimensions are hard
                physical-fit-checked at admission, not hours-tracked, so AgentQuota carries no
                RAM/storage fields at all. */}
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
