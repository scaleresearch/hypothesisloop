'use client'

import useSWR from 'swr'
import Link from 'next/link'
import {
  LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer,
} from 'recharts'
import { fetchExperiment, fetchExperimentMetrics } from '@/lib/api'
import type { MetricDataPoint } from '@/types'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Badge, TierBadge } from '@/components/ui/badge'
import { Loading, EmptyState } from '@/components/ui/status-message'
import { semantic, agentPalette } from '@/lib/colors'
import { formatAccH } from '@/lib/format'
import { EVICTION_REASON_LABELS } from '@/lib/eviction'

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

  const j = job as any
  const evictionLabel = j.eviction_reason ? (EVICTION_REASON_LABELS[j.eviction_reason] ?? j.eviction_reason) : null

  // Jobs report one or more metrics (the objective plus any secondary ones the workload
  // emits, e.g. val_loss alongside val_accuracy) — group by metric_name so each gets its
  // own chart instead of interleaving unrelated scales on a single line.
  const primaryMetricName = j.objective ? String(j.objective).split(/\s+/).pop() : undefined
  const seriesByMetric = new Map<string, { frac: number; value: number }[]>()
  for (const p of (metrics ?? []) as MetricDataPoint[]) {
    const name = p.metric_name ?? primaryMetricName ?? 'metric'
    if (!seriesByMetric.has(name)) seriesByMetric.set(name, [])
    seriesByMetric.get(name)!.push({
      frac: parseFloat((p.fraction_complete * 100).toFixed(1)),
      value: p.metric_value ?? (p as any).value,
    })
  }
  const metricNames = Array.from(seriesByMetric.keys()).sort((a, b) => {
    if (a === primaryMetricName) return -1
    if (b === primaryMetricName) return 1
    return a.localeCompare(b)
  })

  const costEstimate = j.estimated_cost_acch != null ? formatAccH(j.estimated_cost_acch) : null
  const costActual = j.actual_cost_acch != null ? formatAccH(j.actual_cost_acch) : null

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
            {j.hypothesis_id ? (
              <Link href={`/hypotheses/${j.hypothesis_id}`} className="text-link">{j.hypothesis}</Link>
            ) : j.hypothesis}
          </p>
        </div>
        <Link href="/jobs" className="text-link" style={{ fontSize: 12, marginBottom: 4 }}>← All jobs</Link>
      </div>

      {/* Eviction alert */}
      {j.status === 'EVICTED' && evictionLabel && (
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
          <span style={{ color: semantic.accent, fontSize: 13 }}>{evictionLabel}</span>
          {j.metric_at_eviction != null && (
            <span className="mono text-muted" style={{ marginLeft: 12, fontSize: 12 }}>
              metric at eviction: {j.metric_at_eviction.toFixed(4)}
            </span>
          )}
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
              {costActual != null && <Row label="Actual cost" value={`${costActual} AccH`} highlight={semantic.accent} />}
              <Row label="Submitted" value={j.created_at ? new Date(j.created_at).toLocaleString() : '—'} />
              {j.started_at && <Row label="Started" value={new Date(j.started_at).toLocaleString()} />}
              {j.completed_at && <Row label="Completed" value={new Date(j.completed_at).toLocaleString()} />}
              {j.final_metric_value != null && <Row label="Final metric" value={j.final_metric_value.toFixed(4)} highlight={semantic.success} />}
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
              {metricNames.map((name, i) => (
                <div key={name}>
                  <div className="mono text-muted" style={{ fontSize: 11, marginBottom: 4 }}>
                    {name}{name === primaryMetricName ? ' (objective)' : ''}
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
                        dot={false}
                        strokeWidth={2}
                        type="monotone"
                      />
                    </LineChart>
                  </ResponsiveContainer>
                </div>
              ))}
            </div>
          ) : (
            <EmptyState>No metric data yet — waiting for job to start reporting.</EmptyState>
          )}
        </PodContent>
      </Pod>

      {/* Lineage */}
      {(j.code_ref || j.config_hash || j.data_ref || j.env_image) && (
        <Pod style={{ marginBottom: 12 }}>
          <PodHeader>Lineage &amp; Reproducibility</PodHeader>
          <PodContent>
            <table className="wa-table" style={{ maxWidth: 700 }}>
              <tbody>
                {j.code_ref && <Row label="Code ref (git SHA)" value={j.code_ref} />}
                {j.config_hash && <Row label="Config hash (sha256)" value={j.config_hash} />}
                {j.data_ref && <Row label="Data ref" value={j.data_ref} />}
                {j.env_image && <Row label="Env image (digest)" value={j.env_image} />}
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
    </div>
  )
}
