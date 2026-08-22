'use client'

import useSWR from 'swr'
import { useRouter } from 'next/navigation'
import { fetchExperiments, fetchExperimentStats, fetchPlatformExperiments, fetchClusters } from '@/lib/api'
import type { Experiment, PlatformExperiment, ClustersResponse } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { StatTile } from '@/components/ui/stat-tile'
import { Badge } from '@/components/ui/badge'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import {
  ResponsiveContainer, PieChart, Pie, Cell, Tooltip, BarChart, Bar, XAxis, YAxis, CartesianGrid,
} from 'recharts'
import { statusColor, semantic } from '@/lib/colors'
import { evictionCodeLabel, evictionLabel } from '@/lib/eviction'

type Job = Record<string, any>

// How many of the newest evicted jobs the audit log shows.
const RECENT_EVICTIONS = 10

function ChartTooltip({ active, payload, label }: any) {
  if (!active || !payload?.length) return null
  return (
    <div style={{
      background: 'var(--surface-raised)', border: '1px solid var(--border-dark)', borderRadius: 8,
      padding: '6px 10px', fontSize: 12, boxShadow: 'var(--shadow-md)',
    }}>
      {label && <div style={{ color: 'var(--muted-fg)', marginBottom: 2 }}>{label}</div>}
      {payload.map((p: any, i: number) => (
        <div key={i} className="mono" style={{ color: p.color ?? p.fill }}>{p.name}: {p.value}</div>
      ))}
    </div>
  )
}

export default function DashboardPage() {
  const router = useRouter()

  // Counts come from the server's own aggregate, never from tallying fetched rows: list reads
  // are capped at one page, so a client-side tally would silently report the page's shape as
  // the platform's.
  const { data: stats, error, isLoading } = useSWR(
    'experiment-stats',
    () => fetchExperimentStats(),
    { refreshInterval: 15_000 },
  )

  // The audit log below is the one place that needs actual rows — the newest handful of them.
  const { data: evictedJobs } = useSWR<Experiment[]>(
    'recent-evictions',
    () => fetchExperiments({ status: 'EVICTED', sort: '-updated_at', limit: RECENT_EVICTIONS }),
    { refreshInterval: 15_000 },
  )

  const { data: platformExps } = useSWR<PlatformExperiment[]>(
    'pe-all',
    () => fetchPlatformExperiments(),
    { refreshInterval: 30_000 },
  )

  const { data: clusters } = useSWR<ClustersResponse>(
    'clusters',
    fetchClusters,
    { refreshInterval: 5_000 },
  )

  const byStatus = stats?.by_status ?? {}
  const total = stats?.total ?? 0
  const running = byStatus.RUNNING ?? 0
  const completed = byStatus.COMPLETED ?? 0
  const failed = byStatus.FAILED ?? 0
  const evicted = byStatus.EVICTED ?? 0
  const pending = (byStatus.SUBMITTED ?? 0) + (byStatus.QUEUED ?? 0) + (byStatus.ADMITTED ?? 0)

  // Eviction reasons arrive already grouped by code — a reason may carry a ': detail'
  // explanation, and every distinct one would otherwise become its own category.
  const evictionByReason = stats?.evictions_by_reason ?? {}

  const tierTotal = stats?.by_capacity_tier ?? {}
  const tierRunning = stats?.running_by_capacity_tier ?? {}
  const tierEvicted = stats?.evicted_by_capacity_tier ?? {}

  // Completion rate: of jobs that reached a terminal state, the share that completed
  // successfully rather than failing or being evicted.
  const terminalCount = completed + failed + evicted
  const completionRate = terminalCount > 0
    ? Math.round((completed / terminalCount) * 100)
    : null

  // Connected clusters
  const clusterList = clusters?.clusters ?? []
  const connectedCount = clusterList.filter(c => c.connected).length

  // Real hardware occupancy — busy vs. total accelerators from each connected cluster's most
  // recent reconcile snapshot — as opposed to a platform experiment's budget-consumption ratio
  // (used/allocated AccH), which measures spend, not whether chips are actually idle or running
  // a job.
  const acceleratorBusy = clusterList.reduce((s, c) => s + c.accelerator_busy, 0)
  const acceleratorTotal = clusterList.reduce((s, c) => s + c.accelerator_total, 0)
  const occupancyPct = acceleratorTotal > 0 ? Math.round((acceleratorBusy / acceleratorTotal) * 100) : null

  const recentEvictions: Job[] = (evictedJobs ?? []) as Job[]

  // Per-platform-experiment summary
  const peMap = Object.fromEntries((platformExps ?? []).map(pe => [pe.id, pe.name]))

  const statusBreakdown = Object.entries(byStatus).sort((a, b) => b[1] - a[1])

  const evictionChartData = Object.entries(evictionByReason)
    .sort((a, b) => b[1] - a[1])
    .map(([r, n]) => ({ reason: evictionCodeLabel(r), count: n }))

  return (
    <div>
      <PageHeader title="Scheduler Quality" description="Platform health, eviction audit log, capacity utilisation, and completion rate" />

      {isLoading && <Loading />}
      {error && <ErrorMessage>Cannot reach registry service.</ErrorMessage>}

      {/* KPI row */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, minmax(0, 1fr))', gap: 10, marginBottom: 10 }}>
        <StatTile label="Total Jobs" value={total} />
        <StatTile label="Running" value={running} color={semantic.success} />
        <StatTile label="Pending" value={pending} color={semantic.warning} sub="submitted + queued + admitted" />
        <StatTile label="Completion Rate" value={completionRate != null ? `${completionRate}%` : '—'} sub="completed of all terminal jobs" />
      </div>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, minmax(0, 1fr))', gap: 10, marginBottom: 16 }}>
        <StatTile label="Completed" value={completed} color={semantic.accent} />
        <StatTile label="Evicted" value={evicted} color={semantic.pink} sub="contract / plateau / guard" />
        <StatTile label="Failed" value={failed} color={semantic.danger} />
        <StatTile
          label="Clusters"
          value={`${connectedCount}/${clusterList.length}`}
          color={connectedCount === clusterList.length && clusterList.length > 0 ? semantic.success : semantic.danger}
          sub="connected / registered"
        />
      </div>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, minmax(0, 1fr))', gap: 10, marginBottom: 16 }}>
        <StatTile
          label="Accelerator Occupancy"
          value={occupancyPct != null ? `${occupancyPct}%` : '—'}
          color={semantic.accent}
          sub={acceleratorTotal > 0 ? `${acceleratorBusy}/${acceleratorTotal} chips busy, live across connected clusters` : 'no live capacity reported'}
        />
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: 'minmax(0, 1.1fr) minmax(0, 1fr) minmax(0, 1fr)', gap: 12 }}>
        {/* Job status breakdown as donut chart */}
        <Pod>
          <PodHeader>Job Status Breakdown</PodHeader>
          <PodContent>
            {total === 0 ? (
              <EmptyState>No jobs yet.</EmptyState>
            ) : (
              <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
                <div style={{ width: 140, height: 140, flexShrink: 0 }}>
                  <ResponsiveContainer width="100%" height="100%">
                    <PieChart>
                      <Pie
                        data={statusBreakdown.map(([s, n]) => ({ name: s, value: n }))}
                        dataKey="value"
                        nameKey="name"
                        innerRadius={42}
                        outerRadius={64}
                        paddingAngle={2}
                        stroke="none"
                      >
                        {statusBreakdown.map(([s]) => (
                          <Cell key={s} fill={statusColor(s)} />
                        ))}
                      </Pie>
                      <Tooltip content={<ChartTooltip />} />
                    </PieChart>
                  </ResponsiveContainer>
                </div>
                <div style={{ display: 'flex', flexDirection: 'column', gap: 6, flex: 1, minWidth: 0 }}>
                  {statusBreakdown.map(([s, n]) => (
                    <div
                      key={s}
                      tabIndex={0}
                      role="link"
                      style={{ display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer' }}
                      onClick={() => router.push(`/jobs`)}
                      onKeyDown={e => { if (e.key === 'Enter') router.push('/jobs') }}
                    >
                      <span style={{ width: 8, height: 8, borderRadius: '50%', background: statusColor(s), flexShrink: 0 }} />
                      <Badge status={s} style={{ flexShrink: 0 }}>{s}</Badge>
                      <span className="mono text-muted" style={{ fontSize: 11, marginLeft: 'auto' }}>
                        {n} · {Math.round(n / total * 100)}%
                      </span>
                    </div>
                  ))}
                </div>
              </div>
            )}
          </PodContent>
        </Pod>

        {/* Capacity tier breakdown */}
        <Pod>
          <PodHeader>Jobs by Capacity Tier</PodHeader>
          <PodContent>
            <table className="wa-table">
              <thead>
                <tr><th>Tier</th><th>Running</th><th>Total</th><th>Evicted</th></tr>
              </thead>
              <tbody>
                <tr>
                  <td><Badge status="guaranteed">Guaranteed</Badge></td>
                  <td className="mono">{tierRunning.guaranteed ?? 0}</td>
                  <td className="mono">{tierTotal.guaranteed ?? 0}</td>
                  <td className="mono">{tierEvicted.guaranteed ?? 0}</td>
                </tr>
                <tr>
                  <td><Badge status="burst">Burst</Badge></td>
                  <td className="mono">{tierRunning.burst ?? 0}</td>
                  <td className="mono">{tierTotal.burst ?? 0}</td>
                  <td className="mono">{tierEvicted.burst ?? 0}</td>
                </tr>
              </tbody>
            </table>
          </PodContent>
        </Pod>

        {/* Eviction breakdown by reason */}
        <Pod>
          <PodHeader>Eviction Reasons</PodHeader>
          <PodContent>
            {evictionChartData.length === 0 ? (
              <EmptyState>No evictions recorded.</EmptyState>
            ) : (
              <div style={{ width: '100%', height: 150 }}>
                <ResponsiveContainer width="100%" height="100%">
                  <BarChart data={evictionChartData} layout="vertical" margin={{ left: 0, right: 12, top: 4, bottom: 4 }}>
                    <CartesianGrid stroke="var(--border)" horizontal={false} />
                    <XAxis type="number" tick={{ fill: 'var(--muted-fg)', fontSize: 10 }} axisLine={false} tickLine={false} allowDecimals={false} />
                    <YAxis
                      type="category"
                      dataKey="reason"
                      width={110}
                      tick={{ fill: 'var(--muted-fg)', fontSize: 10 }}
                      axisLine={false}
                      tickLine={false}
                    />
                    <Tooltip content={<ChartTooltip />} cursor={{ fill: 'rgba(255,255,255,0.04)' }} />
                    <Bar dataKey="count" fill={semantic.pink} radius={[0, 4, 4, 0]} barSize={14} />
                  </BarChart>
                </ResponsiveContainer>
              </div>
            )}
          </PodContent>
        </Pod>
      </div>

      {/* Recent evictions — audit log */}
      {recentEvictions.length > 0 && (
        <Pod>
          <PodHeader>Recent Evictions — Audit Log</PodHeader>
          <PodContent scrollX>
            <table className="wa-table">
              <thead>
                <tr>
                  <th>Job ID</th>
                  <th>Agent</th>
                  <th>Platform Experiment</th>
                  <th>Eviction Reason</th>
                  <th>Accelerator</th>
                  <th>Tier</th>
                </tr>
              </thead>
              <tbody>
                {recentEvictions.map(e => (
                  <tr
                    key={e.id}
                    tabIndex={0}
                    role="link"
                    style={{ cursor: 'pointer' }}
                    onClick={() => router.push(`/jobs/${e.id}`)}
                    onKeyDown={ev => { if (ev.key === 'Enter') router.push(`/jobs/${e.id}`) }}
                  >
                    <td className="mono text-dim" style={{ fontSize: 11 }}>{e.id.slice(0, 8)}…</td>
                    <td className="mono">{e.agent_id}</td>
                    <td className="text-link" style={{ fontSize: 12 }}>
                      {e.platform_experiment_id ? (peMap[e.platform_experiment_id] ?? e.platform_experiment_id.slice(0, 8) + '…') : '—'}
                    </td>
                    <td style={{ fontSize: 12 }}>
                      <Badge status="evicted">{e.eviction_reason ? evictionLabel(e.eviction_reason) : '—'}</Badge>
                    </td>
                    <td className="mono" style={{ fontSize: 11 }}>{e.accelerator_count}× {e.accelerator_type}</td>
                    <td><Badge status={e.capacity_tier ?? 'unknown'}>{e.capacity_tier ?? '—'}</Badge></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </PodContent>
        </Pod>
      )}

      {/* Scheduler quality metrics (spec §8) */}
      <Pod>
        <PodHeader>Key Metrics</PodHeader>
        <PodContent>
          <table className="wa-table" style={{ maxWidth: 640 }}>
            <thead>
              <tr><th>Metric</th><th>Definition</th><th>Value</th></tr>
            </thead>
            <tbody>
              <tr>
                <td>Completion rate</td>
                <td className="text-muted" style={{ fontSize: 12 }}>Completed ÷ (completed + failed + evicted)</td>
                <td className="mono">{completionRate != null ? `${completionRate}%` : 'n/a'}</td>
              </tr>
              <tr>
                <td>Early-stop rate</td>
                <td className="text-muted" style={{ fontSize: 12 }}>Evicted ÷ (completed + evicted)</td>
                <td className="mono">{completed + evicted > 0 ? `${Math.round(evicted / (completed + evicted) * 100)}%` : 'n/a'}</td>
              </tr>
              <tr>
                <td>Clusters connected</td>
                <td className="text-muted" style={{ fontSize: 12 }}>Registered clusters with a live cluster-agent heartbeat</td>
                <td className="mono">{clusterList.length > 0 ? `${connectedCount}/${clusterList.length}` : 'n/a'}</td>
              </tr>
              <tr>
                <td>Crash rate</td>
                <td className="text-muted" style={{ fontSize: 12 }}>Failed jobs ÷ total</td>
                <td className="mono">{total > 0 ? `${Math.round(failed / total * 100)}%` : 'n/a'}</td>
              </tr>
              <tr>
                <td>Burst eviction rate</td>
                <td className="text-muted" style={{ fontSize: 12 }}>Burst evictions ÷ total burst jobs</td>
                <td className="mono">
                  {(tierTotal.burst ?? 0) > 0
                    ? `${Math.round((tierEvicted.burst ?? 0) / (tierTotal.burst ?? 1) * 100)}%`
                    : 'n/a'}
                </td>
              </tr>
            </tbody>
          </table>
        </PodContent>
      </Pod>
    </div>
  )
}
