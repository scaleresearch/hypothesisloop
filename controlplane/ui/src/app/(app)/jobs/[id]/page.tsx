'use client'

import useSWR from 'swr'
import { useRecordScope } from '@/lib/read-only'
import { TextLink } from '@/components/ui/link'
import {
  LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer,
} from 'recharts'
import { fetchExperiment, fetchExperimentMetrics, fetchExperimentLogs } from '@/lib/api'
import type { MetricDataPoint } from '@/types'
import { PlatformExperimentName } from '@/components/platform-experiment-name'
import { Card, CardHeader, CardContent, CardTitle } from '@/components/ui/card'
import { CopyButton } from '@/components/ui/copy-button'
import { PageHeader } from '@/components/ui/page-header'
import { Badge, TierBadge } from '@/components/ui/badge'
import { Actor } from '@/components/actor'
import { Branch } from '@/components/code-ref'
import { BASELINE_JOB, measuresBaseline } from '@/components/job-hypothesis'
import { Callout, Loading, EmptyState, ErrorMessage } from '@/components/ui/status-message'
import { Table } from '@/components/ui/table'
import { semantic, agentPalette, chart } from '@/lib/colors'
import { formatAccH, formatDuration } from '@/lib/format'
import { budgetUnit, kindOf } from '@/lib/units'
import { notAdmittedLabel, saidLines } from '@/lib/eviction'
import { JobReason } from '@/components/job-reason'
import { graphHref } from '@/lib/hypothesis-graph'

// What eviction_reason says of a job in each status that carries one: a QUEUED job's is the
// reason its last attempt was requeued for.
const LAST_ATTEMPT: Record<string, string> = {
  EVICTED: 'Evicted',
  FAILED: 'Failed',
  REJECTED: 'Rejected',
  QUEUED: 'Requeued, runs again',
}

function Row({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <tr>
      <td className="w-[180px] whitespace-nowrap pr-3 align-top text-xs text-muted">{label}</td>
      <td className="font-mono text-xs">{value ?? '—'}</td>
    </tr>
  )
}

export default function JobDetailPage({ params }: { params: { id: string } }) {
  const { id } = params

  const { data: job, error } = useSWR(id, fetchExperiment, { refreshInterval: 5_000 })
  useRecordScope(job?.project_id)
  const { data: logs, error: logsError } = useSWR(
    job ? `logs-${id}` : null,
    () => fetchExperimentLogs(id),
    { refreshInterval: 10_000 },
  )
  const { data: metrics, error: metricsError } = useSWR(
    job ? `metrics-${id}` : null,
    () => fetchExperimentMetrics(id),
    { refreshInterval: 10_000 },
  )
  if (error) return (
    <div>
      <PageHeader title="Job" />
      <ErrorMessage className="mb-3">Could not load job {id}: {error.message}</ErrorMessage>
      <TextLink href="/jobs" className="text-sm">← Back to jobs</TextLink>
    </div>
  )

  if (!job) return <Loading />

  const j = job
  const lastAttempt = LAST_ATTEMPT[j.status]

  // Jobs report one or more metrics (the ranking metric plus any secondary ones the workload
  // emits, e.g. val_loss alongside val_accuracy) — group by metric_name so each gets its
  // own chart instead of interleaving unrelated scales on a single line.
  const primaryMetricName = j.metric?.name
  // The API appends one contiguous block per underlying series (metric_name, or per retry
  // attempt if a retry changed agent_id) rather than a single time-ordered stream, so a job
  // with a retry has two blocks for the same metric_name back to back. Sort each metric's
  // points by when they were recorded before charting, or the connected line jumps backward
  // in time at the block boundary and renders as several crossing lines instead of one.
  const sortedMetrics = [...((metrics ?? []) as MetricDataPoint[])].sort(
    (a, b) => new Date(a.recorded_at as any).getTime() - new Date(b.recorded_at as any).getTime(),
  )
  const seriesByMetric = new Map<string, { frac: number; value: number }[]>()
  // metric_basis defaults to "raw" server-side; a metric whose points carry more than one
  // basis (or any non-"raw" basis) changed what its value means mid-run, and that must be
  // visible next to the chart, not silently plotted as one uniform line.
  const basesByMetric = new Map<string, Set<string>>()
  for (const p of sortedMetrics) {
    const name = p.metric_name
    if (!seriesByMetric.has(name)) seriesByMetric.set(name, [])
    seriesByMetric.get(name)!.push({
      frac: parseFloat((p.fraction_complete * 100).toFixed(1)),
      value: p.metric_value,
    })
    if (!basesByMetric.has(name)) basesByMetric.set(name, new Set())
    basesByMetric.get(name)!.add(p.metric_basis || 'raw')
  }
  const metricNames = Array.from(seriesByMetric.keys()).sort((a, b) => {
    if (a === primaryMetricName) return -1
    if (b === primaryMetricName) return 1
    return a.localeCompare(b)
  })

  const costEstimate = j.estimated_cost_acch != null ? formatAccH(j.estimated_cost_acch) : null
  const baseline = measuresBaseline(j)

  return (
    <div>
      <PageHeader
        title={
          <>
            <span className="line-clamp-3 min-w-0 text-lg font-medium leading-snug" title={j.hypothesis}>{baseline ? BASELINE_JOB : j.hypothesis}</span>
            <Badge status={j.status ?? 'UNKNOWN'} />
          </>
        }
        description={
          <span className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
            <span className="font-mono text-2xs">Job {j.id}</span>
            <CopyButton value={j.id} />
            {!baseline && <TextLink href={`/hypotheses/${j.hypothesis_id}`} className="whitespace-nowrap">View hypothesis →</TextLink>}
            <TextLink href={graphHref(j.platform_experiment_id, j.hypothesis_id)} className="whitespace-nowrap">In graph →</TextLink>
          </span>
        }
        actions={
          <TextLink href={`/jobs?platform_experiment_id=${j.platform_experiment_id}`} className="whitespace-nowrap text-xs">
            ← This experiment&apos;s jobs
          </TextLink>
        }
      />

      {lastAttempt && j.eviction_reason && (
        <Callout tone="warning">
          <strong className="text-warning">{lastAttempt}</strong>
          <JobReason reason={j.eviction_reason} />
        </Callout>
      )}

      {/* Why isn't it running / why did it stop. phase_detail is the runtime's own account —
          an image that won't pull, a config the runtime rejected, or the exit code of a
          container that died. For a failed job this is the difference between "it broke" and
          knowing what broke. */}
      {j.phase_detail && (j.phase_detail.reason || j.phase_detail.message) && (
        <Callout tone="danger">
          <strong className="text-danger">Reported by the cluster:</strong>{' '}
          {j.phase_detail.reason && (
            <span className="font-mono text-danger">
              {j.phase_detail.reason}
            </span>
          )}
          {j.phase_detail.message && saidLines(j.phase_detail.message).map((line, i) => (
            <span key={i} className={i === 0 ? 'ml-2 text-muted' : 'block font-mono text-xs text-muted'}>{line}</span>
          ))}
          {j.phase_detail.restart_count ? (
            <span className="ml-3 font-mono text-xs text-muted">
              restarts: {j.phase_detail.restart_count}
            </span>
          ) : null}
        </Callout>
      )}

      {/* A queued job never errors on its own; this is the scheduler's current reason. */}
      {j.status === 'QUEUED' && j.not_admitted_reason && (
        <Callout tone="warning">
          <strong className="text-warning">Not admitted:</strong>{' '}
          <span className="font-mono text-xs text-muted">{notAdmittedLabel(j.not_admitted_reason, j.not_admitted_message)}</span>
        </Callout>
      )}

      {(j.metrics || j.summary || j.status === 'COMPLETED') && (
        <Card>
          <CardHeader><CardTitle>Result</CardTitle></CardHeader>
          <CardContent>
            {(j.metrics ?? []).map(m => (
              <div key={m.name} className="mb-2.5 font-mono">
                <span className={m.name === primaryMetricName ? 'text-2xl font-medium text-success' : 'text-lg font-medium'}>
                  {m.value.toFixed(4)}
                </span>
                <span className="ml-2 text-xs text-muted">
                  {m.name}{m.basis !== 'raw' && ` · basis ${m.basis}`}
                </span>
              </div>
            ))}
            {j.summary && <p className="max-w-[900px] whitespace-pre-wrap text-sm leading-[1.6]">{j.summary}</p>}
            {!j.summary && j.status === 'COMPLETED' && (
              <p className="text-sm text-warning">No summary: the agent did not write up what this job found.</p>
            )}
          </CardContent>
        </Card>
      )}

      {/* Job metadata */}
      <Card>
        <CardHeader><CardTitle>Job details</CardTitle></CardHeader>
        <CardContent>
          <Table>
            <tbody>
              <Row label="Agent" value={<span title={j.agent_id}>{j.agent_name}</span>} />
              <Row label="Submitted by" value={<Actor actor={j.actor} />} />
              <Row label="Experiment" value={
                <TextLink href={`/platform-experiments/${j.platform_experiment_id}`}><PlatformExperimentName id={j.platform_experiment_id} /></TextLink>
              } />
              <Row label="Capacity tier" value={<TierBadge tier={j.capacity_tier} />} />
              <Row label="Accelerator" value={`${j.accelerator_count}× ${j.accelerator_type}`} />
              <Row label="Est. duration" value={j.estimated_duration_hours != null ? formatDuration(j.estimated_duration_hours * 3_600_000) : '—'} />
              <Row label="Est. cost" value={costEstimate != null ? `${costEstimate} ${budgetUnit(kindOf(j.accelerator_type))}` : '—'} />
              <Row label="Submitted" value={new Date(j.created_at).toLocaleString()} />
              {j.submitted_at && <Row label="Sent to cluster" value={new Date(j.submitted_at).toLocaleString()} />}
              {j.finished_at && (
                <Row label="Finished" value={
                  <>
                    {new Date(j.finished_at).toLocaleString()}
                    {j.submitted_at && <span className="text-muted"> · {formatDuration(new Date(j.finished_at).getTime() - new Date(j.submitted_at).getTime())} after it was sent</span>}
                  </>
                } />
              )}
              {j.theory && <Row label="Rationale" value={j.theory} />}
              {j.job?.env && Object.keys(j.job.env).length > 0 && (
                <Row label="Environment" value={
                  <div className="flex flex-wrap gap-x-4 gap-y-1">
                    {Object.entries(j.job.env).sort().map(([k, v]) => <span key={k}>{k}={v}</span>)}
                  </div>
                } />
              )}
            </tbody>
          </Table>
        </CardContent>
      </Card>

      {/* Metric trajectories — one chart per reported metric, the ranking metric first */}
      <Card>
        <CardHeader><CardTitle>Metrics over the run</CardTitle></CardHeader>
        <CardContent>
          {metricsError ? (
            <ErrorMessage>Could not read this job&apos;s metrics: {metricsError.message}</ErrorMessage>
          ) : metricNames.length > 0 ? (
            <div className={metricNames.length > 1 ? 'grid grid-cols-2 gap-4' : 'grid gap-4'}>
              {metricNames.map((name, i) => {
                const bases = Array.from(basesByMetric.get(name) ?? [])
                const nonRaw = bases.filter(b => b !== 'raw')
                const points = seriesByMetric.get(name) ?? []
                // A Line needs 2+ points to draw a segment; a job that reports a metric only
                // once (many one-shot diagnostics do) has exactly 1 point, and dot={false}
                // would render nothing at all for it — real data, empty-looking chart. Show
                // dots whenever there aren't enough points for the line itself to be visible.
                const showDots = points.length < 2
                return (
                <div key={name}>
                  <div className="mb-1 font-mono text-2xs text-muted">
                    {name}{name === primaryMetricName ? ' (ranking metric)' : ''}
                    {nonRaw.length > 0 && (
                      <span
                        className="ml-2 rounded-sm border border-danger bg-danger-tint px-1.5 py-px font-mono text-2xs text-danger"
                        title="One or more values on this chart are not on the metric's normal ('raw') scale — do not compare them to a raw-basis run."
                      >
                        basis: {bases.join(', ')}
                      </span>
                    )}
                  </div>
                  <ResponsiveContainer width="100%" height={metricNames.length > 1 ? 200 : 260}>
                    <LineChart data={seriesByMetric.get(name)} margin={{ top: 8, right: 16, left: 0, bottom: 14 }}>
                      <CartesianGrid strokeDasharray="3 3" stroke={chart.grid} />
                      <XAxis
                        dataKey="frac"
                        type="number"
                        domain={[0, 100]}
                        tickFormatter={(v) => `${v}%`}
                        tick={chart.tick}
                        stroke={chart.grid}
                        label={{ value: 'Training progress', position: 'insideBottom', offset: -2, ...chart.tick }}
                      />
                      <YAxis domain={['auto', 'auto']} tick={chart.tick} stroke={chart.grid} />
                      <Tooltip
                        formatter={(v: number) => v.toFixed(4)}
                        labelFormatter={(l) => `${l}% complete`}
                        contentStyle={chart.tooltip.contentStyle}
                        labelStyle={chart.tooltip.labelStyle}
                      />
                      <Line
                        dataKey="value"
                        name={name}
                        stroke={name === primaryMetricName ? semantic.primary : agentPalette[i % agentPalette.length]}
                        dot={showDots ? { r: 3 } : false}
                        strokeWidth={2}
                        type="monotone"
                      />
                    </LineChart>
                  </ResponsiveContainer>
                </div>
              )})}
            </div>
          ) : (
            <EmptyState>No metric data yet — waiting for job to start reporting.</EmptyState>
          )}
        </CardContent>
      </Card>

      {/* Lineage */}
      {(j.job?.image || j.code_ref || j.config_hash || j.data_ref) && (
        <Card>
          <CardHeader><CardTitle>What it ran</CardTitle></CardHeader>
          <CardContent>
            <Table>
              <tbody>
                {j.job?.image && <Row label="Image" value={j.job.image} />}
                {j.branch && <Row label="Branch" value={<Branch branch={j.branch} codeRef={j.code_ref} platformExperimentID={j.platform_experiment_id} />} />}
                {j.code_ref && <Row label="Code (git commit)" value={j.code_ref} />}
                {j.config_hash && <Row label="Configuration checksum" value={j.config_hash} />}
                {j.data_ref && <Row label="Data" value={j.data_ref} />}
              </tbody>
            </Table>
          </CardContent>
        </Card>
      )}

      {/* Parent lineage */}
      {j.parent_id && (
        <Card>
          <CardHeader><CardTitle>Derived from</CardTitle></CardHeader>
          <CardContent>
            <p className="text-xs">
              Parent job: <TextLink inline href={`/jobs/${j.parent_id}`} className="font-mono">{j.parent_id}</TextLink>
            </p>
          </CardContent>
        </Card>
      )}
    
      {/* The job's own output. For a failed job the traceback usually lives only here — the
          record carries a reason code, not the error. Includes the crashed instance's output
          when a container restarted, which is otherwise unreachable. */}
      <Card>
        <CardHeader><CardTitle>Logs</CardTitle></CardHeader>
        <CardContent>
          {logsError ? (
            <ErrorMessage>Could not read this job&apos;s logs: {logsError.message}</ErrorMessage>
          ) : logs && logs.length > 0 ? (
            <pre className="m-0 max-h-[420px] overflow-auto whitespace-pre-wrap break-words font-mono text-xs">
              {logs.join('\n')}
            </pre>
          ) : (
            <EmptyState>No log output reported yet.</EmptyState>
          )}
        </CardContent>
      </Card>
</div>
  )
}
