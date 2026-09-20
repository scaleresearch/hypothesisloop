'use client'

import useSWR from 'swr'
import { useRecordProject } from '@/lib/query-state'
import { Link } from '@/components/ui/link'
import {
  LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer,
} from 'recharts'
import { fetchExperiment, fetchExperimentMetrics, fetchExperimentLogs } from '@/lib/api'
import type { MetricDataPoint } from '@/types'
import { PlatformExperimentName } from '@/components/platform-experiment-name'
import { Card, CardHeader, CardContent } from '@/components/ui/card'
import { PageHeader } from '@/components/ui/page-header'
import { Badge, TierBadge } from '@/components/ui/badge'
import { Callout, Loading, EmptyState, ErrorMessage } from '@/components/ui/status-message'
import { Table } from '@/components/ui/table'
import { semantic, agentPalette, chart } from '@/lib/colors'
import { formatAccH, formatDuration } from '@/lib/format'
import { evictionLabel, notAdmittedLabel } from '@/lib/eviction'
import { graphHref } from '@/lib/hypothesis-graph'

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
  useRecordProject(job?.project_id)
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
      <PageHeader title="Job Not Found" />
      <ErrorMessage className="mb-3">Could not load job {id}</ErrorMessage>
      <Link href="/jobs" className="text-sm text-accent hover:underline">← Back to jobs</Link>
    </div>
  )

  if (!job) return <Loading />

  const j = job
  // evictionLabel maps the code to its human label and keeps any explanation the scheduler
  // attached — see lib/eviction.
  const evictionText = j.eviction_reason ? evictionLabel(j.eviction_reason) : null

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

  return (
    <div>
      <PageHeader
        title={
          <>
            <span className="font-mono text-base font-normal text-subtle">{j.id}</span>
            <Badge status={j.status ?? 'UNKNOWN'}>{j.status ?? 'UNKNOWN'}</Badge>
            <TierBadge tier={j.capacity_tier} />
          </>
        }
        description={
          <span className="italic">
            {j.hypothesis}
            {j.hypothesis_id && (
              <>
                {' '}
                <Link href={`/hypotheses/${j.hypothesis_id}`} className="whitespace-nowrap text-xs not-italic text-accent hover:underline">
                  View hypothesis →
                </Link>
                {' '}
                <Link href={graphHref(j.platform_experiment_id, j.hypothesis_id)} className="whitespace-nowrap text-xs not-italic text-accent hover:underline">
                  In graph →
                </Link>
              </>
            )}
          </span>
        }
        actions={
          <Link href={`/jobs?platform_experiment_id=${j.platform_experiment_id}`} className="whitespace-nowrap text-xs text-accent hover:underline">
            ← This experiment&apos;s jobs
          </Link>
        }
      />

      {/* Eviction alert */}
      {j.status === 'EVICTED' && evictionText && (
        <Callout tone="accent" className="text-accent">
          <strong>Evicted:</strong>{' '}
          <span>{evictionText}</span>
        </Callout>
      )}

      {/* Why isn't it running / why did it stop. phase_detail is the runtime's own account —
          an image that won't pull, a config the runtime rejected, or the exit code of a
          container that died. For a failed job this is the difference between "it broke" and
          knowing what broke. */}
      {j.phase_detail && (j.phase_detail.reason || j.phase_detail.message) && (
        <Callout tone="danger">
          <strong className="text-danger">Runtime:</strong>{' '}
          {j.phase_detail.reason && (
            <span className="font-mono text-danger">
              {j.phase_detail.reason}
            </span>
          )}
          {j.phase_detail.message && (
            <span className="ml-2 text-muted">
              {j.phase_detail.message}
            </span>
          )}
          {j.phase_detail.restart_count ? (
            <span className="ml-3 font-mono text-xs text-muted">
              restarts: {j.phase_detail.restart_count}
            </span>
          ) : null}
        </Callout>
      )}

      {/* A queued job never errors on its own; this is the scheduler's current reason. */}
      {j.status === 'QUEUED' && j.not_admitted_reason && (
        <Callout tone="accent">
          <strong className="text-accent">Not admitted:</strong>{' '}
          <span className="font-mono text-xs text-muted">{notAdmittedLabel(j.not_admitted_reason)}</span>
        </Callout>
      )}

      {(j.metrics || j.summary || j.status === 'COMPLETED') && (
        <Card>
          <CardHeader>Result</CardHeader>
          <CardContent>
            {(j.metrics ?? []).map(m => (
              <div key={m.name} className="mb-2.5 font-mono">
                <span className={m.name === primaryMetricName ? 'text-2xl font-bold text-success' : 'text-lg font-semibold'}>
                  {m.best.toFixed(4)}
                </span>
                <span className="ml-2 text-xs text-muted">
                  best {m.name} · last {m.last.toFixed(4)}{m.basis !== 'raw' && ` · basis ${m.basis}`}
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
        <CardHeader>Job Details</CardHeader>
        <CardContent>
          <Table>
            <tbody>
              <Row label="Agent" value={j.agent_id} />
              <Row label="Platform Experiment" value={
                <Link href={`/platform-experiments/${j.platform_experiment_id}`} className="text-accent hover:underline"><PlatformExperimentName id={j.platform_experiment_id} /></Link>
              } />
              <Row label="Capacity Tier" value={<TierBadge tier={j.capacity_tier} />} />
              <Row label="Accelerator" value={`${j.accelerator_count}× ${j.accelerator_type}`} />
              <Row label="Est. duration" value={j.estimated_duration_hours != null ? formatDuration(j.estimated_duration_hours * 3_600_000) : '—'} />
              <Row label="Est. cost" value={costEstimate != null ? `${costEstimate} AccH` : '—'} />
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
              {j.objective && <Row label="Objective" value={j.objective} />}
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
        <CardHeader>Metric Trajectories</CardHeader>
        <CardContent>
          {metricsError ? (
            <ErrorMessage>Could not read this job&apos;s metrics.</ErrorMessage>
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
                        className="ml-2 rounded-sm border border-danger bg-danger/10 px-1.5 py-px font-mono text-2xs text-danger"
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
          <CardHeader>Lineage &amp; Reproducibility</CardHeader>
          <CardContent>
            <Table>
              <tbody>
                {j.job?.image && <Row label="Image" value={j.job.image} />}
                {j.code_ref && <Row label="Code ref (git SHA)" value={j.code_ref} />}
                {j.config_hash && <Row label="Config hash (sha256)" value={j.config_hash} />}
                {j.data_ref && <Row label="Data ref" value={j.data_ref} />}
              </tbody>
            </Table>
          </CardContent>
        </Card>
      )}

      {/* Parent lineage */}
      {j.parent_id && (
        <Card>
          <CardHeader>Derived From</CardHeader>
          <CardContent>
            <p className="text-xs">
              Parent job: <Link href={`/jobs/${j.parent_id}`} className="font-mono text-accent hover:underline">{j.parent_id}</Link>
            </p>
          </CardContent>
        </Card>
      )}
    
      {/* The job's own output. For a failed job the traceback usually lives only here — the
          record carries a reason code, not the error. Includes the crashed instance's output
          when a container restarted, which is otherwise unreachable. */}
      <Card>
        <CardHeader>Logs</CardHeader>
        <CardContent>
          {logsError ? (
            <ErrorMessage>Could not read this job&apos;s log tail.</ErrorMessage>
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
