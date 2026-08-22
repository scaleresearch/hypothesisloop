'use client'

import useSWR from 'swr'
import Link from 'next/link'
import {
  LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer,
} from 'recharts'
import { fetchExperiment, fetchExperimentMetrics, fetchExperimentLogs } from '@/lib/api'
import type { MetricDataPoint } from '@/types'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Badge, TierBadge } from '@/components/ui/badge'
import { Loading, EmptyState } from '@/components/ui/status-message'
import { semantic, agentPalette } from '@/lib/colors'
import { formatAccH } from '@/lib/format'
import { evictionLabel } from '@/lib/eviction'

function Row({ label, value, highlight }: { label: string; value: React.ReactNode; highlight?: string }) {
  return (
    <tr>
      <td className="text-muted" style={{ width: 180, verticalAlign: 'top', paddingRight: 12, whiteSpace: 'nowrap' }}>{label}</td>
      <td className="mono" style={highlight ? { color: highlight, fontWeight: 600 } : undefined}>{value ?? '—'}</td>
    </tr>
  )
}

export default function JobDetailPage({ params }: { params: { id: string } }) {
  const { id } = params

  const { data: job, error } = useSWR(id, fetchExperiment, { refreshInterval: 5_000 })
  const { data: logs } = useSWR(
    job ? `logs-${id}` : null,
    () => fetchExperimentLogs(id),
    { refreshInterval: 10_000 },
  )
  const { data: metrics } = useSWR(
    job ? `metrics-${id}` : null,
    () => fetchExperimentMetrics(id),
    { refreshInterval: 10_000 },
  )

  if (error) return (
    <div>
      <div className="wa-title"><h1>Job Not Found</h1></div>
      <p className="text-error">Could not load job {id}</p>
      <Link href="/jobs" className="text-link" style={{ fontSize: 13 }}>← Back to jobs</Link>
    </div>
  )

  if (!job) return <Loading />

  const j = job
  // evictionLabel maps the code to its human label and keeps any explanation the scheduler
  // attached — see lib/eviction.
  const evictionText = j.eviction_reason ? evictionLabel(j.eviction_reason) : null

  // Jobs report one or more metrics (the objective plus any secondary ones the workload
  // emits, e.g. val_loss alongside val_accuracy) — group by metric_name so each gets its
  // own chart instead of interleaving unrelated scales on a single line.
  const primaryMetricName = j.objective ? String(j.objective).split(/\s+/).pop() : undefined
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
    const name = p.metric_name ?? primaryMetricName ?? 'metric'
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
  // The job's final metric comes from the metrics store, never from the job record — the record
  // has no such field, and metric values live in one place (important.md #3). The series is
  // already sorted by recorded_at above, so the last point of the primary metric is the answer.
  const primarySeries = primaryMetricName ? seriesByMetric.get(primaryMetricName) : undefined
  const finalPrimaryMetric = primarySeries?.length ? primarySeries[primarySeries.length - 1].value : null


  const axisColor = 'rgba(255,255,255,.45)'
  const gridColor = 'rgba(255,255,255,.08)'

  return (
    <div>
      <div className="wa-title" style={{ display: 'flex', alignItems: 'flex-end', gap: 16 }}>
        <div style={{ flex: 1 }}>
          <h1 style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
            <span className="mono text-dim" style={{ fontSize: 14 }}>{j.id?.slice(0, 8)}…</span>
            <Badge status={j.status ?? 'UNKNOWN'}>{j.status ?? 'UNKNOWN'}</Badge>
            <TierBadge tier={j.capacity_tier} />
          </h1>
          <p style={{ fontStyle: 'italic' }}>
            {j.hypothesis}
            {j.hypothesis_id && (
              <>
                {' '}
                <Link href={`/hypotheses/${j.hypothesis_id}`} className="text-link" style={{ fontStyle: 'normal', fontSize: 12, whiteSpace: 'nowrap' }}>
                  View hypothesis →
                </Link>
              </>
            )}
          </p>
        </div>
        <Link href="/jobs" className="text-link" style={{ fontSize: 12, marginBottom: 4 }}>← All jobs</Link>
      </div>

      {/* Eviction alert */}
      {j.status === 'EVICTED' && evictionText && (
        <div
          style={{
            background: 'rgba(166,152,255,.08)',
            border: '1px solid rgba(166,152,255,.35)',
            borderRadius: 8,
            padding: '10px 14px',
            marginBottom: 14,
          }}
        >
          <strong style={{ color: semantic.accent }}>Evicted:</strong>{' '}
          <span style={{ color: semantic.accent, fontSize: 13 }}>{evictionText}</span>
        </div>
      )}

      {/* Why isn't it running / why did it stop. phase_detail is the runtime's own account —
          an image that won't pull, a config the runtime rejected, or the exit code of a
          container that died. For a failed job this is the difference between "it broke" and
          knowing what broke. */}
      {j.phase_detail && (j.phase_detail.reason || j.phase_detail.message) && (
        <div
          style={{
            background: 'rgba(255,153,153,.07)',
            border: '1px solid rgba(255,153,153,.3)',
            borderRadius: 8,
            padding: '10px 14px',
            marginBottom: 14,
          }}
        >
          <strong style={{ color: semantic.danger }}>Runtime:</strong>{' '}
          {j.phase_detail.reason && (
            <span className="mono" style={{ color: semantic.danger, fontSize: 13 }}>
              {j.phase_detail.reason}
            </span>
          )}
          {j.phase_detail.message && (
            <span className="text-muted" style={{ marginLeft: 8, fontSize: 13 }}>
              {j.phase_detail.message}
            </span>
          )}
          {j.phase_detail.restart_count ? (
            <span className="mono text-muted" style={{ marginLeft: 12, fontSize: 12 }}>
              restarts: {j.phase_detail.restart_count}
            </span>
          ) : null}
        </div>
      )}

      {/* A queued job never errors on its own; this is the scheduler's current reason. */}
      {j.status === 'QUEUED' && j.not_admitted_reason && (
        <div
          style={{
            background: 'rgba(166,152,255,.06)',
            border: '1px solid rgba(166,152,255,.25)',
            borderRadius: 8,
            padding: '10px 14px',
            marginBottom: 14,
          }}
        >
          <strong style={{ color: semantic.accent }}>Not admitted:</strong>{' '}
          <span className="mono text-muted" style={{ fontSize: 12 }}>{j.not_admitted_reason}</span>
        </div>
      )}

      {/* Job metadata */}
      <Pod style={{ marginBottom: 12 }}>
        <PodHeader>Job Details</PodHeader>
        <PodContent>
          <table className="wa-table">
            <tbody>
              <Row label="Agent" value={j.agent_id} />
              <Row label="Platform Experiment" value={j.platform_experiment_id
                ? <Link href="/platform-experiments" className="text-link">{j.platform_experiment_id.slice(0, 12)}…</Link>
                : '—'
              } />
              <Row label="Capacity Tier" value={<TierBadge tier={j.capacity_tier} />} />
              <Row label="Accelerator" value={`${j.accelerator_count}× ${j.accelerator_type}`} />
              <Row label="Est. duration" value={j.estimated_duration_hours != null ? `${j.estimated_duration_hours.toFixed(2)} h` : '—'} />
              <Row label="Est. cost" value={costEstimate != null ? `${costEstimate} AccH` : '—'} />
              <Row label="Submitted" value={j.created_at ? new Date(j.created_at).toLocaleString() : '—'} />
              {finalPrimaryMetric != null && <Row label="Final metric" value={finalPrimaryMetric.toFixed(4)} highlight={semantic.success} />}
              {j.objective && <Row label="Objective" value={j.objective} />}
            </tbody>
          </table>
        </PodContent>
      </Pod>

      {/* Metric trajectories — one chart per reported metric, primary (the objective) first */}
      <Pod style={{ marginBottom: 12 }}>
        <PodHeader>Metric Trajectories{j.objective ? ` — ${j.objective}` : ''}</PodHeader>
        <PodContent>
          {metricNames.length > 0 ? (
            <div style={{ display: 'grid', gridTemplateColumns: metricNames.length > 1 ? '1fr 1fr' : '1fr', gap: 16 }}>
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
                  <div className="mono text-muted" style={{ fontSize: 11, marginBottom: 4 }}>
                    {name}{name === primaryMetricName ? ' (objective)' : ''}
                    {nonRaw.length > 0 && (
                      <span
                        className="mono"
                        title="One or more values on this chart are not on the metric's normal ('raw') scale — do not compare them to a raw-basis run."
                        style={{
                          marginLeft: 8, fontSize: 10, padding: '1px 6px', borderRadius: 4,
                          background: 'rgba(255,153,153,.12)', border: `1px solid ${semantic.danger}`, color: semantic.danger,
                        }}
                      >
                        basis: {bases.join(', ')}
                      </span>
                    )}
                  </div>
                  <ResponsiveContainer width="100%" height={metricNames.length > 1 ? 200 : 260}>
                    <LineChart data={seriesByMetric.get(name)} margin={{ top: 8, right: 16, left: 0, bottom: 0 }}>
                      <CartesianGrid strokeDasharray="3 3" stroke={gridColor} />
                      <XAxis
                        dataKey="frac"
                        type="number"
                        domain={[0, 100]}
                        tickFormatter={(v) => `${v}%`}
                        tick={{ fontSize: 11, fill: axisColor }}
                        stroke={gridColor}
                        label={{ value: 'Training progress', position: 'insideBottom', offset: -2, fontSize: 11, fill: axisColor }}
                      />
                      <YAxis tick={{ fontSize: 11, fill: axisColor }} stroke={gridColor} />
                      <Tooltip
                        formatter={(v: number) => v.toFixed(4)}
                        labelFormatter={(l) => `${l}% complete`}
                        contentStyle={{ background: 'var(--surface-raised)', border: '1px solid rgba(255,255,255,.16)', borderRadius: 8, fontSize: 12 }}
                        labelStyle={{ color: 'var(--foreground)' }}
                      />
                      <Line
                        dataKey="value"
                        name={name}
                        stroke={name === primaryMetricName ? semantic.accentStrong : agentPalette[i % agentPalette.length]}
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
        </PodContent>
      </Pod>

      {/* Lineage */}
      {(j.code_ref || j.config_hash || j.data_ref) && (
        <Pod style={{ marginBottom: 12 }}>
          <PodHeader>Lineage &amp; Reproducibility</PodHeader>
          <PodContent>
            <table className="wa-table" style={{ maxWidth: 700 }}>
              <tbody>
                {j.code_ref && <Row label="Code ref (git SHA)" value={j.code_ref} />}
                {j.config_hash && <Row label="Config hash (sha256)" value={j.config_hash} />}
                {j.data_ref && <Row label="Data ref" value={j.data_ref} />}
              </tbody>
            </table>
          </PodContent>
        </Pod>
      )}

      {/* Parent lineage */}
      {j.parent_id && (
        <Pod>
          <PodHeader>Derived From</PodHeader>
          <PodContent>
            <p style={{ fontSize: 12 }}>
              Parent job: <Link href={`/jobs/${j.parent_id}`} className="text-link mono">{j.parent_id}</Link>
            </p>
          </PodContent>
        </Pod>
      )}
    
      {/* The job's own output. For a failed job the traceback usually lives only here — the
          record carries a reason code, not the error. Includes the crashed instance's output
          when a container restarted, which is otherwise unreachable. */}
      <Pod style={{ marginBottom: 12 }}>
        <PodHeader>Logs</PodHeader>
        <PodContent>
          {logs && logs.length > 0 ? (
            <pre
              className="mono"
              style={{
                margin: 0, fontSize: 12, lineHeight: 1.5, maxHeight: 420,
                overflow: 'auto', whiteSpace: 'pre-wrap', wordBreak: 'break-word',
              }}
            >
              {logs.join('\n')}
            </pre>
          ) : (
            <EmptyState>No log output reported yet.</EmptyState>
          )}
        </PodContent>
      </Pod>
</div>
  )
}
