'use client'

import { Fragment, useMemo, useState } from 'react'
import { useProjectRouter } from '@/lib/query-state'
import { useReadOnly, useRecordScope } from '@/lib/example'
import { Link } from '@/components/ui/link'
import useSWR from 'swr'
import { clsx } from 'clsx'
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
  fetchUnsummarizedJobIDs,
  HYPOTHESIS_STATUSES,
} from '@/lib/api'
import type { StagesStatus } from '@/lib/api'
import { quotaRemainingAccH } from '@/lib/quota'
import type { PlatformExperiment, AgentQuota, Experiment, MetricDataPoint, AgentMetricSeries } from '@/types'
import { ExperimentAgents } from '@/components/experiment-agents'
import { Card, CardHeader, CardContent, InsetCard } from '@/components/ui/card'
import { Badge, Dot, NoSummary } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog } from '@/components/ui/dialog'
import { MetricBar } from '@/components/ui/metric-bar'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { SearchableSelect } from '@/components/ui/searchable-select'
import { StatTile } from '@/components/ui/stat-tile'
import { PageHeader } from '@/components/ui/page-header'
import { ExperimentCode } from '@/components/experiment-code'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { Table } from '@/components/ui/table'
import { Eyebrow, Pill } from '@/components/ui/text'
import { semantic, agentPalette, chart } from '@/lib/colors'
import { acceleratorLabel, formatAccH, formatDate, isZeroDate } from '@/lib/format'
import { evictionLabel, faultBreakdown } from '@/lib/eviction'
import { envSettings, filedAfterVerdict, hypothesisActivity, settings, variedEnvByHypothesis, variedKeys, verdictSetBy } from '@/lib/hypothesis-progress'
import { graphHref } from '@/lib/hypothesis-graph'

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
    <div className="relative">
      <Button
        type="button"
        size="icon"
        onClick={() => setExpanded(true)}
        title="Expand chart"
        aria-label="Expand chart"
        className="absolute right-0 top-0 z-[1] text-muted"
      >
        ⤢
      </Button>
      <MetricLineChart points={points} trackedMetrics={trackedMetrics} height={400} />
      <Dialog open={expanded} onClose={() => setExpanded(false)} size="wide" title={<span className="font-mono">{jobId}</span>}>
        <MetricLineChart points={points} trackedMetrics={trackedMetrics} height={640} />
      </Dialog>
    </div>
  )
}

// Lets the user inspect any job belonging to this platform experiment (not just currently
// running ones) and pick which of that job's actually-reported metrics to chart, rather than
// being limited to the experiment's tracked/ranking subset like the per-agent panel above.
function LiveMetricsPicker({ jobs }: { jobs: Experiment[] }) {
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
    <div>
      <div className="mb-3 flex flex-wrap items-start gap-3">
        <div>
          <Eyebrow className="mb-1.5 block">Job</Eyebrow>
          <SearchableSelect
            options={sorted.map(j => ({ value: j.id, label: `${j.id.slice(0, 8)}… · ${j.status} · ${j.agent_id}` }))}
            value={selectedJob.id}
            onChange={v => { if (v) setJobId(v) }}
            placeholder="Search jobs…"
            className="min-w-[320px]"
            hideAllOption
          />
        </div>
        <div className="min-w-[200px] flex-1">
          <Eyebrow className="mb-1.5 block">Metrics</Eyebrow>
          {availableMetrics.length === 0 ? (
            <span className="text-xs text-subtle">No metrics reported yet</span>
          ) : (
            <div className="flex flex-wrap gap-1.5">
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
  const roster = [...agentIds].sort().join(',')
  return useMemo(
    () => new Map(roster.split(',').map((id, i) => [id, AGENT_COLORS[i % AGENT_COLORS.length]])),
    [roster],
  )
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
      <div className="mb-2 flex items-center justify-between">
        <Eyebrow>Agents</Eyebrow>
        <Button type="button" variant="link" size="sm" onClick={() => onChange(allSelected ? new Set() : new Set(agentIds))}>
          {allSelected ? 'Clear' : 'Select all'}
        </Button>
      </div>
      <div className="flex flex-wrap gap-1.5">
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
              label={<><Dot color={colorFor(id)} />{id}</>}
            />
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
      <div className="mb-2 flex items-center justify-between">
        <Eyebrow>Metrics</Eyebrow>
        <Button type="button" variant="link" size="sm" onClick={() => onChange(allSelected ? new Set() : new Set(metrics.map(m => m.key)))}>
          {allSelected ? 'Clear' : 'Select all'}
        </Button>
      </div>
      <div className="flex flex-wrap gap-1.5">
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
                {i === 0 && <span className="font-bold">★</span>}
                {m.key}
                <span className="opacity-70">{m.direction === 'maximize' ? '↑' : '↓'}</span>
              </>}
            />
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
          className="mb-2 inline-block px-2 py-0.5 rounded-sm border border-danger bg-danger/10 font-mono text-2xs text-danger"
          title="These agents also reported this metric on a non-&quot;raw&quot; basis (a rescaled/transformed value); that data is excluded from this chart and never compared to raw-basis agents."
        >
          Non-raw basis data excluded for: {Array.from(nonRawAgentIds).sort().join(', ')}
        </div>
      )}
    <ResponsiveContainer width="100%" height={300}>
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

function AgentJobsPanel({ projectId, platformExperimentId, agentId, trackedMetrics }: { projectId: string; platformExperimentId: string; agentId: string; trackedMetrics: string[] }) {
  const { data, error } = useSWR(
    ['pe-agent-jobs', platformExperimentId, agentId],
    () => fetchExperimentsPage({ project_id: projectId, platform_experiment_id: platformExperimentId, agent: agentId, sort: '-created_at' }),
    { refreshInterval: 15_000 },
  )
  if (error) return <ErrorMessage>Could not load jobs from {agentId}.</ErrorMessage>
  if (!data) return <Loading />
  const jobs = data.items
  if (jobs.length === 0) {
    return <EmptyState>No jobs from {agentId} yet.</EmptyState>
  }
  return (
    <div className="grid grid-cols-[repeat(auto-fill,minmax(480px,1fr))] gap-2.5 pb-1 pt-2.5">
      {jobs.map(job => (
        <InsetCard key={job.id}>
          <div className="mb-1 flex items-center justify-between text-2xs">
            <Link href={`/jobs/${job.id}`} className="font-mono font-semibold text-accent hover:underline">{job.id.slice(0, 8)}…</Link>
            <StatusBadge status={job.status} />
          </div>
          <JobMetricMini jobId={job.id} trackedMetrics={trackedMetrics} />
        </InsetCard>
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
  const chip = (label: string) => <Pill key={label}>{label}</Pill>
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
  const router = useProjectRouter()
  const [selectedAgentIds, setSelectedAgentIds] = useState<Set<string> | null>(null)
  const [selectedMetricKeys, setSelectedMetricKeys] = useState<Set<string> | null>(null)
  const [expandedAgentId, setExpandedAgentId] = useState<string | null>(null)

  const { data: pe, error: peError, isLoading: peLoading } = useSWR<PlatformExperiment>(
    ['pe', id],
    () => fetchPlatformExperiment(id),
    { refreshInterval: 15_000 },
  )
  useRecordScope(pe && { project_id: pe.project_id, platform_experiment_id: pe.id })
  const readOnly = useReadOnly()

  const { data: quotas, error: quotasError } = useSWR<AgentQuota[]>(
    ['pe-quotas', id],
    () => fetchPlatformExperimentQuotas(id),
    { refreshInterval: 15_000 },
  )

  // Each list below is asked for inside the platform experiment's own project, so it waits for
  // the experiment itself. The newest jobs only, for the recent list and the live metrics picker.
  const { data: experimentsPage, error: experimentsError } = useSWR(
    pe ? ['pe-experiments', id] : null,
    () => fetchExperimentsPage({ project_id: pe!.project_id, platform_experiment_id: id, sort: '-created_at', limit: MAX_LIST_PAGE_SIZE }),
    { refreshInterval: 15_000 },
  )
  const experiments = experimentsPage?.items

  // Asked for by status: a job that outlived the newest page of jobs is still running.
  const { data: runningPage, error: runningError } = useSWR(
    pe ? ['pe-running', id] : null,
    () => fetchExperimentsPage({ project_id: pe!.project_id, platform_experiment_id: id, status: 'RUNNING', sort: '-created_at' }),
    { refreshInterval: 15_000 },
  )

  // Whole-experiment job counts, grouped server-side. The job list above is one bounded page,
  // so tallying it would report the page's shape under a label that reads as the experiment's.
  const { data: stats, error: statsError } = useSWR(
    pe ? ['pe-stats-by-agent', id] : null,
    () => fetchExperimentStats({ project_id: pe!.project_id, platform_experiment_id: id, group_by: 'agent' }),
    { refreshInterval: 15_000 },
  )
  const jobsByAgent = stats?.groups ?? {}
  const { data: hypothesisStats, error: hypothesisStatsError } = useSWR(
    pe ? ['pe-stats-by-hypothesis', id] : null,
    () => fetchExperimentStats({ project_id: pe!.project_id, platform_experiment_id: id, group_by: 'hypothesis' }),
    { refreshInterval: 15_000 },
  )

  const { data: donations } = useSWR(
    pe ? ['pe-donations-open', id] : null,
    () => fetchDonationsPage({ project_id: pe!.project_id, platform_experiment_id: id, status: 'open', limit: MAX_LIST_PAGE_SIZE }),
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

  const { data: unsummarized } = useSWR(
    pe ? ['pe-unsummarized', id] : null,
    () => fetchUnsummarizedJobIDs({ project_id: pe!.project_id, platform_experiment_id: id }),
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
    quotasError && 'quotas',
    (experimentsError || runningError) && 'jobs',
    stagesError && 'stage status',
    (hypothesesError || findingsError) && 'hypotheses',
    metricSeriesError && 'metric history',
    resultsError && 'results',
    (statsError || hypothesisStatsError) && 'job counts',
  ].filter(Boolean) as string[]

  if (peLoading) return <Loading />
  if (peError) return <ErrorMessage>Could not load this platform experiment: {peError.message}</ErrorMessage>
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
  // What tells the agents' best jobs apart; a lone agent's best job shows every setting.
  const standingEnvs = (primaryStandings?.standings ?? []).map(st => st.env)
  const standingKeys = standingEnvs.length > 1 ? variedKeys(standingEnvs) : Object.keys(standingEnvs[0] ?? {}).sort()
  const hypothesisText = new Map((findings ?? []).map(h => [h.id, h.text]))
  const bestOf = (metricKey: string, agentId: string) =>
    results?.bests.find(b => b.metric === metricKey)?.agents.find(a => a.agent_id === agentId)
  const totalUsed = (quotas ?? []).reduce((s, q) => s + q.used_guaranteed_acch + q.used_burst_acch, 0)
  const hypothesisCount = verdicts?.total ?? 0
  const hypothesisSub = [
    HYPOTHESIS_STATUSES.map(v => `${verdicts?.by_status[v] ?? 0} ${v}`).join(' · '),
    hypothesisActivity(hypothesisCount, hypothesisStats?.groups ?? {}),
  ].filter(Boolean).join(' · ')

  // Best result first; a hypothesis whose jobs reported no metric comes last.
  const bestFirst = primaryStandings?.direction === 'minimize' ? 1 : -1
  const found = [...(findings ?? [])].sort((a, b) =>
    (a.best_job ? bestFirst * a.best_job.metric.best : Infinity) - (b.best_job ? bestFirst * b.best_job.metric.best : Infinity))

  // SUBMITTED and RUNNING, the set max_concurrent_accelerators is checked against.
  const inFlightAccelerators = stats?.in_flight_accelerators ?? 0

  // Running experiments
  const jobsByStatus = stats?.by_status ?? {}
  const running = runningPage?.items ?? []
  const recent = (experiments ?? []).slice(0, 20)
  const variedByHypothesis = variedEnvByHypothesis(recent)

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
      <PageHeader
        back={readOnly
          ? { label: 'Examples', onClick: () => router.push('/examples') }
          : { label: 'Platform Experiments', onClick: () => router.push('/platform-experiments') }}
        title={pe.name}
        description={
          <div className="flex flex-wrap items-center gap-2.5">
            <StatusBadge status={pe.status} />
            {!isZeroDate(pe.starts_at) && (
              <span className="text-xs text-subtle">
                {formatDate(pe.starts_at)} – {formatDate(pe.ends_at)}
              </span>
            )}
            <Link href={graphHref(pe.id)} className="text-xs text-accent hover:underline">
              {hypothesisCount} Hypotheses →
            </Link>
            <SubmitPolicyChips pe={pe} />
            <div className="basis-full">
              <ExperimentCode pe={pe} />
            </div>
          </div>
        }
      />

      {/* Description + metrics */}
      {(pe.description || (pe.metrics && pe.metrics.length > 0)) && (
        <Card>
          <CardHeader>About this Experiment</CardHeader>
          <CardContent>
            {pe.description && (
              <CollapsibleDescription text={pe.description} className={pe.metrics?.length ? 'mb-3' : undefined} />
            )}
            {pe.metrics && pe.metrics.length > 0 && (
              <div>
                <Eyebrow className="mb-1.5 block">Optimization Metrics</Eyebrow>
                <div className="flex flex-wrap gap-2">
                  {pe.metrics.map((m, i) => (
                    <Pill key={m.key} tone={m.direction === 'maximize' ? 'success' : 'warning'} active={i === 0}>
                      {i === 0 && <span className="font-bold">PRIMARY</span>}
                      {m.key} {m.direction === 'maximize' ? '↑ maximize' : '↓ minimize'}
                    </Pill>
                  ))}
                </div>
              </div>
            )}
          </CardContent>
        </Card>
      )}

      {/* Findings */}
      {found.length > 0 && (
        <Card>
          <CardHeader>
            Findings
            <span className="ml-2 text-xs font-normal text-muted">
              ({found.length} hypotheses with a verdict or a result)
            </span>
          </CardHeader>
          <CardContent scrollX>
            <Table className="min-w-[760px] table-fixed">
              <thead>
                <tr>
                  <th className="w-[110px]">Verdict</th>
                  <th>Hypothesis</th>
                  <th className="w-[170px] text-right">Best job</th>
                  <th title="The best job's own summary. Without one, the newest finding of the hypothesis, named with the job it is about.">Finding</th>
                </tr>
              </thead>
              <tbody>
                {found.map(h => (
                  <tr key={h.id} className="[&>td]:align-top">
                    <td>
                      <Badge status={h.status}>{h.status}</Badge>
                      <div className="mt-0.5 text-2xs text-muted">{verdictSetBy(h)}</div>
                      {h.finding && filedAfterVerdict(h, h.finding.created_at) && (
                        <div className="mt-0.5 text-2xs text-warning" title="The newest finding was filed after this verdict was set">
                          newer finding
                        </div>
                      )}
                    </td>
                    <td className="text-xs">
                      <Link href={`/hypotheses/${h.id}`} className="line-clamp-2 text-accent hover:underline" title={h.text}>{h.text}</Link>
                    </td>
                    <td className="text-right font-mono text-xs">
                      {h.best_job ? (
                        <Link href={`/jobs/${h.best_job.id}`} className="text-accent hover:underline" title={h.best_job.id}>
                          {h.best_job.metric.name} {h.best_job.metric.best.toFixed(4)}
                        </Link>
                      ) : <span className="text-xs text-muted">—</span>}
                      {h.best_job && unsummarized?.has(h.best_job.id) && <div><NoSummary /></div>}
                    </td>
                    <td className="text-xs">
                      {h.best_job?.summary ? (
                        <span className="line-clamp-2" title={h.best_job.summary}>{h.best_job.summary}</span>
                      ) : h.finding ? (
                        <span className="line-clamp-2" title={h.finding.summary}>
                          <Link href={`/jobs/${h.finding.experiment_id}`} className="font-mono text-accent hover:underline">{h.finding.experiment_id}</Link>
                          {': '}{h.finding.summary}
                        </span>
                      ) : <span className="text-xs text-muted">no finding filed</span>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </Table>
          </CardContent>
        </Card>
      )}

      {/* Competing agents over time */}
      {metrics.length > 0 && (
        <Card>
          <CardHeader>Competing Agents over time</CardHeader>
          <CardContent>
            <div className={clsx('mb-4 grid gap-6', knownAgentIds.length > 0 ? 'grid-cols-2' : 'grid-cols-1')}>
              <MetricSelector metrics={metrics} selected={activeMetricKeys} onChange={setSelectedMetricKeys} />
              {knownAgentIds.length > 0 && (
                <AgentSelector agentIds={knownAgentIds} selected={activeAgentIds} onChange={setSelectedAgentIds} colorFor={colorFor} />
              )}
            </div>
            {activeMetricKeys.size === 0 ? (
              <EmptyState>Select at least one metric to plot.</EmptyState>
            ) : (
              <div className="grid gap-4">
                {metrics.filter(m => activeMetricKeys.has(m.key)).map(m => (
                  <div key={m.key}>
                    <div className="mb-1 text-xs font-semibold">
                      {m.key} <span className="font-normal text-subtle">({m.direction})</span>
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
          </CardContent>
        </Card>
      )}

      {/* Stage ladder — boundaries are published, per-agent rank deliberately is not
          (see docs/stages.md). */}
      {stagesStatus && stagesStatus.stages.length > 0 && (
        <Card className={clsx('border-l-[3px]', stagesStatus.cut_agents.length > 0 ? 'border-l-warning hover:border-l-warning' : 'border-l-accent hover:border-l-accent')}>
          <CardContent>
            <div className="flex flex-wrap items-center gap-3">
              <div className="rounded-full bg-accent/15 px-2.5 py-1 font-mono text-xs font-bold text-accent">
                STAGE {stagesStatus.current_stage} OF {stagesStatus.stages.length}
              </div>
              <span className="text-xs text-subtle">
                {Math.round(stagesStatus.progress * 100)}% through the experiment
                {stagesStatus.current_stage < stagesStatus.stages.length && (
                  <> — next boundary at {Math.round(stagesStatus.next_boundary_progress * 100)}%,
                    cutting {stagesStatus.stages[stagesStatus.current_stage - 1].evict_pct}% of survivors</>
                )}
                {stagesStatus.current_stage >= stagesStatus.stages.length && <> — final stage, no cut remaining</>}
              </span>
            </div>

            {/* The ladder itself: one segment per stage, sized by its share of the experiment. */}
            <div className="mt-3 flex gap-[3px]">
              {stagesStatus.stages.reduce<{ start: number; nodes: JSX.Element[] }>((acc, stage, i) => {
                const stageNo = i + 1
                const end = acc.start + stage.length_pct / 100
                const current = stageNo === stagesStatus.current_stage
                const filled = Math.max(0, Math.min(1, (stagesStatus.progress - acc.start) / (end - acc.start)))
                acc.nodes.push(
                  <div key={stageNo} className="min-w-0" style={{ flex: stage.length_pct }} title={stage.max_job_hours ? `No single job may run longer than ${stage.max_job_hours}h during this stage` : 'No job length limit during this stage'}>
                    <div className={clsx('h-2 overflow-hidden rounded-sm bg-border', current && 'shadow-glow')}>
                      <div className={clsx('h-full', current ? 'bg-accent' : 'bg-accent/55')} style={{ width: `${filled * 100}%` }} />
                    </div>
                    <div className="mt-1 truncate font-mono text-2xs text-subtle">
                      {stage.length_pct}%{stage.evict_pct > 0 && ` · cut ${stage.evict_pct}%`}{stage.max_job_hours ? ` · ≤${stage.max_job_hours}h/job` : ''}
                    </div>
                  </div>
                )
                return { start: end, nodes: acc.nodes }
              }, { start: 0, nodes: [] }).nodes}
            </div>

            <div className="mt-3 grid grid-cols-2 gap-4">
              <div>
                <Eyebrow tone="success" className="mb-1.5 block">
                  Active Agents ({stagesStatus.active_agents.length})
                </Eyebrow>
                {stagesStatus.active_agents.length === 0
                  ? <span className="text-xs text-subtle">none</span>
                  : stagesStatus.active_agents.map(a => (
                    <Pill key={a} tone="success" className="my-0.5 mr-1">{a}</Pill>
                  ))}
              </div>
              <div>
                <Eyebrow tone="danger" className="mb-1.5 block">
                  Cut Agents ({stagesStatus.cut_agents.length})
                </Eyebrow>
                {stagesStatus.cut_agents.length === 0
                  ? <span className="text-xs text-subtle">none</span>
                  : stagesStatus.cut_agents.map(c => (
                    <Pill key={c.agent_id} tone="danger" className="my-0.5 mr-1">
                      {c.agent_id} <span className="opacity-70">· stage {c.stage_index}</span>
                    </Pill>
                  ))}
              </div>
            </div>
          </CardContent>
        </Card>
      )}

      {/* Stats row */}
      <Card>
        <CardContent>
          <div className="grid grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-4">
            <StatTile label="Budget" value={`${pe.budget_accelerator_hours} AccH`} />
            <StatTile
              label="Accelerators In Flight"
              value={pe.max_concurrent_accelerators == null ? `${inFlightAccelerators}` : `${inFlightAccelerators} / ${pe.max_concurrent_accelerators}`}
              tone={pe.max_concurrent_accelerators != null && inFlightAccelerators >= pe.max_concurrent_accelerators ? 'accent' : undefined}
              sub={pe.max_concurrent_accelerators == null ? 'no cap set' : undefined}
            />
            <StatTile label="Agents" value={`${pe.signup_count} / ${pe.max_agents}`} />
            <StatTile label="Budget Used" value={`${formatAccH(totalUsed)} AccH`} />
            <StatTile label="Jobs" value={(stats?.total ?? 0).toString()} href={`/jobs?platform_experiment_id=${pe.id}`} />
            <StatTile
              label="Running"
              value={(jobsByStatus.RUNNING ?? 0).toString()}
              tone={(jobsByStatus.RUNNING ?? 0) > 0 ? 'success' : undefined}
              href={`/jobs?platform_experiment_id=${pe.id}&status=RUNNING`}
            />
            <StatTile
              label="Evicted"
              value={(jobsByStatus.EVICTED ?? 0).toString()}
              tone={(jobsByStatus.EVICTED ?? 0) > 0 ? 'danger' : undefined}
              sub={[evictionFaultBreakdown, evictionBreakdown].filter(Boolean).join(' — ') || undefined}
              href={`/jobs?platform_experiment_id=${pe.id}&status=EVICTED`}
            />
            <StatTile
              label="Hypotheses"
              value={hypothesisCount.toString()}
              tone="accent"
              sub={hypothesisSub}
              href={graphHref(pe.id)}
            />
          </div>
        </CardContent>
      </Card>

      <ExperimentAgents pe={pe} />

      {/* Scoreboard */}
      <Card>
        <CardHeader>
          Scoreboard
          {primaryStandings && ` — ranked by ${primaryStandings.metric} (${primaryStandings.direction})`}
        </CardHeader>
        <CardContent scrollX>
          {scoreboard.length === 0 ? (
            <EmptyState>No agents signed up yet.</EmptyState>
          ) : (
            <Table>
              <thead>
                <tr>
                  <th className="w-9">Rank</th>
                  <th>Agent</th>
                  {metrics.length > 0 ? metrics.map((m, i) => (
                    <th key={m.key} className="text-right">
                      {i === 0 && metrics.length > 1 && (
                        <span title="Primary metric — used for ranking" className="mr-1 font-bold">★</span>
                      )}
                      Best {m.key} <span className="font-normal text-subtle">({m.direction === 'maximize' ? '↑' : '↓'})</span>
                    </th>
                  )) : <th className="text-right">Best Metric</th>}
                  <th title={`The hypothesis each agent's best ${primaryStandings?.metric ?? 'ranked'} job tested, and the settings that differ between those jobs`}>
                    Best job: hypothesis · settings
                  </th>
                  <th className="text-right">Jobs</th>
                  <th className="text-right">Completed</th>
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
                  const colCount = 6 + Math.max(metrics.length, 1)
                  const standing = standingOf.get(agentId)
                  const rank = standing?.rank
                  const isWinner = rank === 1
                  const nonRawMetrics = (results?.bests ?? []).filter(b => b.non_raw_basis_agents?.includes(agentId)).map(b => b.metric)
                  const jobs = jobsByAgent[agentId]
                  return (
                    <Fragment key={agentId}>
                      <tr
                        onClick={() => setExpandedAgentId(isExpanded ? null : agentId)}
                        className={clsx('cursor-pointer', isCut ? 'bg-warning/5 opacity-60' : isWinner && 'bg-primary/5')}
                      >
                        <td className="font-mono text-xs font-semibold">{rank != null ? `#${rank}` : '—'}</td>
                        <td className="whitespace-nowrap font-mono text-xs font-semibold">
                          <span className="inline-block w-3 text-muted">{isExpanded ? '▾' : '▸'}</span>
                          {agentId}
                          {isCut && <span className="ml-2 text-2xs font-normal text-danger">CUT</span>}
                          {isActive && <span className="ml-2 text-2xs font-normal text-success">ACTIVE</span>}
                          {nonRawMetrics.length > 0 && (
                            <span
                              className="ml-2 px-1.5 py-px font-normal rounded-sm border border-danger bg-danger/10 font-mono text-2xs text-danger"
                              title={`Also reported ${nonRawMetrics.join(', ')} on a non-"raw" basis (a rescaled/transformed value). That value is never blended into ${nonRawMetrics.length > 1 ? 'those rankings' : 'that ranking'} — only a raw-basis value (if any) counts here.`}
                            >
                              NON-RAW BASIS
                            </span>
                          )}
                        </td>
                        {metrics.length > 0 ? metrics.map(m => {
                          const best = bestOf(m.key, agentId)
                          return (
                            <td key={m.key} className={clsx('text-right font-mono text-xs', isWinner && 'font-bold')}>
                              {best?.experiment_id ? (
                                <Link
                                  href={`/jobs/${best.experiment_id}`}
                                  className="text-accent hover:underline"
                                  title={`The job that set it: ${best.experiment_id}`}
                                  onClick={e => e.stopPropagation()}
                                >
                                  {best.best.toFixed(4)}
                                </Link>
                              ) : best ? best.best.toFixed(4) : '—'}
                              {best?.experiment_id && unsummarized?.has(best.experiment_id) && <div><NoSummary /></div>}
                            </td>
                          )
                        }) : <td className="text-right font-mono text-xs">—</td>}
                        <td className="max-w-[360px] text-xs">
                          {standing?.hypothesis_id ? (
                            <>
                              <Link
                                href={`/hypotheses/${standing.hypothesis_id}`}
                                className="line-clamp-2 text-accent hover:underline"
                                title={hypothesisText.get(standing.hypothesis_id)}
                                onClick={e => e.stopPropagation()}
                              >
                                {hypothesisText.get(standing.hypothesis_id) ?? standing.hypothesis_id}
                              </Link>
                              <div className="font-mono text-2xs text-muted" title={settings(standing.env, Object.keys(standing.env ?? {}).sort())}>
                                {settings(standing.env, standingKeys)}
                              </div>
                            </>
                          ) : <span className="text-xs text-muted">—</span>}
                        </td>
                        <td className="text-right font-mono text-xs">{jobs?.total ?? 0}</td>
                        <td className="text-right font-mono text-xs">{jobs?.by_status.COMPLETED ?? 0}</td>
                        <td className="w-40">
                          {totalAccH > 0 ? (
                            <MetricBar value={usedAccH} max={totalAccH} />
                          ) : <span className="text-xs text-muted">no quota</span>}
                        </td>
                      </tr>
                      {isExpanded && (
                        <tr>
                          <td colSpan={colCount} className="bg-surface-inset">
                            <AgentJobsPanel
                              projectId={pe.project_id}
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
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Running jobs */}
      {running.length > 0 && (
        <Card>
          <CardHeader>Currently Running Jobs ({runningPage?.total ?? running.length})</CardHeader>
          <CardContent scrollX>
            <Table>
              <thead>
                <tr>
                  <th>Experiment ID</th>
                  <th>Agent</th>
                  <th>Accelerator</th>
                  <th className="text-right">Est. Cost</th>
                  <th>Hypothesis</th>
                </tr>
              </thead>
              <tbody>
                {running.map(exp => (
                  <tr key={exp.id}>
                    <td className="font-mono text-2xs">
                      <Link href={`/jobs/${exp.id}`} className="text-accent hover:underline">{exp.id.slice(0, 8)}…</Link>
                    </td>
                    <td className="font-mono text-xs">{exp.agent_id}</td>
                    <td className="font-mono text-xs">{exp.accelerator_count}× {acceleratorLabel(exp.accelerator_type)}</td>
                    <td className="text-right font-mono text-xs">{exp.estimated_cost_acch != null ? `${formatAccH(exp.estimated_cost_acch)} AccH` : '—'}</td>
                    <td className="max-w-[280px] truncate text-xs">{exp.hypothesis}</td>
                  </tr>
                ))}
              </tbody>
            </Table>
          </CardContent>
        </Card>
      )}

      {/* Live job metrics */}
      {(experiments ?? []).length > 0 && (
        <Card>
          <CardHeader>
            Job Metrics
            <span className="ml-2 text-xs font-normal text-subtle">over training progress, refreshed every 30 s</span>
          </CardHeader>
          <CardContent>
            <LiveMetricsPicker jobs={experiments ?? []} />
          </CardContent>
        </Card>
      )}

      {/* Recent jobs */}
      <Card>
        <CardHeader>
          Recent Jobs
        </CardHeader>
        <CardContent scrollX>
          {!experiments || experiments.length === 0 ? (
            <EmptyState>No jobs submitted yet.</EmptyState>
          ) : (
            <Table>
              <thead>
                <tr>
                  <th>ID</th>
                  <th>Agent</th>
                  <th>Status</th>
                  <th title="Under each hypothesis: the settings that differ between these jobs of it">Hypothesis · varied settings</th>
                  <th className="text-right">Best {primaryStandings?.metric ?? 'Metric'}</th>
                  <th className="text-right">Est. Cost</th>
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
                      <td className="font-mono text-2xs">
                        <Link href={`/jobs/${exp.id}`} className="text-accent hover:underline">{exp.id.slice(0, 8)}…</Link>
                      </td>
                      <td className="font-mono text-xs">{exp.agent_id}</td>
                      <td>
                        <StatusBadge status={exp.status} />
                        {unsummarized?.has(exp.id) && <NoSummary className="ml-1.5" />}
                        {evictionText && (
                          <div className="mt-0.5 text-2xs text-subtle" title="eviction_reason">
                            {evictionText}
                          </div>
                        )}
                      </td>
                      <td className="max-w-xs truncate text-xs" title={exp.hypothesis}>
                        {exp.hypothesis_id
                          ? <Link href={`/hypotheses/${exp.hypothesis_id}`} className="text-accent hover:underline">{exp.hypothesis}</Link>
                          : exp.hypothesis}
                        <div className="mt-0.5 whitespace-normal font-mono text-2xs text-muted">
                          {envSettings(exp, variedByHypothesis.get(exp.hypothesis_id) ?? [])}
                        </div>
                      </td>
                      <td className="text-right font-mono text-xs">{exp.metric?.best.toFixed(4) ?? '—'}</td>
                      <td className="text-right font-mono text-xs">{cost != null ? `${formatAccH(cost)} AccH` : '—'}</td>
                      <td className="text-xs text-subtle">{new Date(exp.created_at).toLocaleString()}</td>
                    </tr>
                  )
                })}
              </tbody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Quota table */}
      {quotas && quotas.length > 0 && (
        <Card>
          <CardHeader>Agent Quotas</CardHeader>
          <CardContent scrollX>
            <Table>
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
                      <td className="font-mono text-xs font-semibold">{q.agent_id}</td>
                      <td className="font-mono text-2xs">{formatAccH(q.used_guaranteed_acch)} / {formatAccH(q.guaranteed_accelerator_hours)} AccH</td>
                      <td className="font-mono text-2xs">{formatAccH(q.used_burst_acch)} / {formatAccH(q.burst_accelerator_hours)} AccH</td>
                      <td className={clsx('font-mono text-2xs', remaining > 0 ? 'text-success' : 'text-danger')}>
                        {formatAccH(remaining)} AccH
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </Table>
            {/* RAM/storage quota columns intentionally omitted: those dimensions are hard
                physical-fit-checked at admission, not hours-tracked, so AgentQuota carries no
                RAM/storage fields at all. */}
          </CardContent>
        </Card>
      )}

      {/* Compute Donation Requests from agents in this experiment */}
      {relevantDonations.length > 0 && (
        <Card>
          <CardHeader>Compute Donation Requests ({relevantDonations.length})</CardHeader>
          <CardContent scrollX>
            <Table>
              <thead>
                <tr>
                  <th>Agent</th>
                  <th className="text-right">Wants</th>
                  <th>Reason</th>
                  <th>Requested</th>
                </tr>
              </thead>
              <tbody>
                {relevantDonations.map(d => (
                  <tr key={d.id}>
                    <td className="font-mono text-xs font-semibold">{d.agent_name || d.agent_id}</td>
                    <td className="text-right font-mono text-xs">{d.credits_want} AccH</td>
                    <td className="text-xs">{d.reason}</td>
                    <td className="text-xs text-subtle">{new Date(d.created_at).toLocaleString()}</td>
                  </tr>
                ))}
              </tbody>
            </Table>
          </CardContent>
        </Card>
      )}
    </div>
  )
}
