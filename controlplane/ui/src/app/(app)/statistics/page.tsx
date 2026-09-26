'use client'

import useSWR from 'swr'
import { useProjectRouter } from '@/lib/query-state'
import { fetchExperiments, fetchExperimentStats, fetchAllExperimentStats, fetchAllClusters } from '@/lib/api'
import { useTenant } from '@/lib/tenant'
import type { Experiment } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { PlatformExperimentName } from '@/components/platform-experiment-name'
import { Card, CardHeader, CardContent } from '@/components/ui/card'
import { StatTile } from '@/components/ui/stat-tile'
import { Badge, Dot } from '@/components/ui/badge'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { Table } from '@/components/ui/table'
import {
  ResponsiveContainer, PieChart, Pie, Cell, Tooltip, BarChart, Bar, XAxis, YAxis, CartesianGrid,
} from 'recharts'
import { chart, statusColor } from '@/lib/colors'
import { evictionCode, evictionCodeLabel, evictionDetail, faultBreakdown } from '@/lib/eviction'
import { acceleratorLabel } from '@/lib/format'

type Job = Record<string, any>

// How many of the newest evicted jobs the audit log shows.
const RECENT_EVICTIONS = 10

function ChartTooltip({ active, payload, label }: any) {
  if (!active || !payload?.length) return null
  return (
    <div className="rounded border border-border-strong bg-surface-raised px-2.5 py-1.5 text-xs shadow-md">
      {label && <div className="mb-0.5 text-muted">{label}</div>}
      {payload.map((p: any, i: number) => (
        <div key={i} className="font-mono" style={{ color: p.color ?? p.fill }}>{p.name}: {p.value}</div>
      ))}
    </div>
  )
}

// Completion rate: of jobs that reached a terminal state, the share that completed successfully
// rather than failing or being evicted.
function completionRate(completed: number, failed: number, evicted: number): number | null {
  const terminal = completed + failed + evicted
  return terminal > 0 ? Math.round((completed / terminal) * 100) : null
}

// How well the platform as a whole places and finishes work, over every org's jobs and clusters.
// The API answers these reads for staff alone.
function SchedulerQuality() {
  const { data: stats } = useSWR('all-experiment-stats', fetchAllExperimentStats, { refreshInterval: 15_000 })
  const { data: clusters } = useSWR('all-clusters', fetchAllClusters, { refreshInterval: 5_000 })

  const byStatus = stats?.by_status ?? {}
  const total = stats?.total ?? 0
  const completed = byStatus.COMPLETED ?? 0
  const failed = byStatus.FAILED ?? 0
  const evicted = byStatus.EVICTED ?? 0
  const burstTotal = stats?.by_capacity_tier?.burst ?? 0
  const burstEvicted = stats?.evicted_by_capacity_tier?.burst ?? 0
  const rate = completionRate(completed, failed, evicted)

  const clusterList = clusters?.clusters ?? []
  const connectedCount = clusterList.filter(c => c.connected).length

  // Real hardware occupancy — busy vs. total accelerators from each connected cluster's most
  // recent reconcile snapshot — as opposed to a platform experiment's budget-consumption ratio
  // (used/allocated AccH), which measures spend, not whether chips are actually idle or running
  // a job.
  const acceleratorBusy = clusterList.reduce((s, c) => s + c.accelerator_busy, 0)
  const acceleratorTotal = clusterList.reduce((s, c) => s + c.accelerator_total, 0)
  const occupancyPct = acceleratorTotal > 0 ? Math.round((acceleratorBusy / acceleratorTotal) * 100) : null

  return (
    <Card>
      <CardHeader>Scheduler Quality · every org · staff only</CardHeader>
      <CardContent>
        <div className="mb-4 grid grid-cols-3 gap-2.5">
          <StatTile label="Total Jobs" value={total} />
          <StatTile
            label="Clusters"
            value={`${connectedCount}/${clusterList.length}`}
            tone={connectedCount === clusterList.length && clusterList.length > 0 ? 'success' : 'danger'}
            sub="connected / registered"
          />
          <StatTile
            label="Accelerator Occupancy"
            value={occupancyPct != null ? `${occupancyPct}%` : '—'}
            tone="accent"
            sub={acceleratorTotal > 0 ? `${acceleratorBusy}/${acceleratorTotal} chips busy, live across connected clusters` : 'no live capacity reported'}
          />
        </div>
        <Table className="max-w-[640px]">
          <thead>
            <tr><th>Metric</th><th>Definition</th><th>Value</th></tr>
          </thead>
          <tbody>
            <tr>
              <td>Completion rate</td>
              <td className="text-xs text-muted">Completed ÷ (completed + failed + evicted)</td>
              <td className="font-mono text-xs">{rate != null ? `${rate}%` : 'n/a'}</td>
            </tr>
            <tr>
              <td>Early-stop rate</td>
              <td className="text-xs text-muted">Evicted ÷ (completed + evicted)</td>
              <td className="font-mono text-xs">{completed + evicted > 0 ? `${Math.round(evicted / (completed + evicted) * 100)}%` : 'n/a'}</td>
            </tr>
            <tr>
              <td>Clusters connected</td>
              <td className="text-xs text-muted">Registered clusters with a live cluster-agent heartbeat</td>
              <td className="font-mono text-xs">{clusterList.length > 0 ? `${connectedCount}/${clusterList.length}` : 'n/a'}</td>
            </tr>
            <tr>
              <td>Crash rate</td>
              <td className="text-xs text-muted">Failed jobs ÷ total</td>
              <td className="font-mono text-xs">{total > 0 ? `${Math.round(failed / total * 100)}%` : 'n/a'}</td>
            </tr>
            <tr>
              <td>Burst eviction rate</td>
              <td className="text-xs text-muted">Burst evictions ÷ total burst jobs</td>
              <td className="font-mono text-xs">{burstTotal > 0 ? `${Math.round(burstEvicted / burstTotal * 100)}%` : 'n/a'}</td>
            </tr>
          </tbody>
        </Table>
      </CardContent>
    </Card>
  )
}

export default function StatisticsPage() {
  const router = useProjectRouter()
  const { project, staff } = useTenant()

  // Counts come from the server's own aggregate, never from tallying fetched rows: list reads
  // are capped at one page, so a client-side tally would silently report the page's shape as
  // the platform's.
  const { data: stats, error, isLoading } = useSWR(
    ['experiment-stats', project.id],
    () => fetchExperimentStats({ project_id: project.id }),
    { refreshInterval: 15_000 },
  )

  // The audit log below is the one place that needs actual rows — the newest handful of them.
  const { data: evictedJobs } = useSWR<Experiment[]>(
    ['recent-evictions', project.id],
    () => fetchExperiments({ project_id: project.id, status: 'EVICTED', sort: '-updated_at', limit: RECENT_EVICTIONS }),
    { refreshInterval: 15_000 },
  )

  const byStatus = stats?.by_status ?? {}
  const total = stats?.total ?? 0
  const running = byStatus.RUNNING ?? 0
  const completed = byStatus.COMPLETED ?? 0
  const failed = byStatus.FAILED ?? 0
  const evicted = byStatus.EVICTED ?? 0
  const pending = (byStatus.SUBMITTED ?? 0) + (byStatus.QUEUED ?? 0)

  // Eviction reasons arrive already grouped by code — a reason may carry a ': detail'
  // explanation, and every distinct one would otherwise become its own category.
  const evictionByReason = stats?.evictions_by_reason ?? {}

  // Whose fault, folded from the same tally. This is the line that keeps an agent from being
  // read as unreliable because a node broke, and a stage-cut job from reading as a failure.
  const faultSummary = faultBreakdown(stats?.evictions_by_class ?? {})

  const tierTotal = stats?.by_capacity_tier ?? {}
  const tierRunning = stats?.running_by_capacity_tier ?? {}
  const tierEvicted = stats?.evicted_by_capacity_tier ?? {}

  const rate = completionRate(completed, failed, evicted)

  const recentEvictions: Job[] = (evictedJobs ?? []) as Job[]

  const statusBreakdown = Object.entries(byStatus).sort((a, b) => b[1] - a[1])

  const evictionChartData = Object.entries(evictionByReason)
    .sort((a, b) => b[1] - a[1])
    .map(([r, n]) => ({ reason: evictionCodeLabel(r), count: n }))

  return (
    <div>
      <PageHeader title="Statistics" description="Job outcomes in this project: status, capacity tiers, the eviction audit log and completion rate" />

      {isLoading && <Loading />}
      {error && <ErrorMessage>Could not load the job statistics of {project.name}: {error.message}</ErrorMessage>}

      {/* KPI row */}
      <div className="mb-4 grid grid-cols-[repeat(auto-fit,minmax(150px,1fr))] gap-2.5">
        <StatTile label="Total Jobs" value={total} />
        <StatTile label="Running" value={running} tone="success" />
        <StatTile label="Pending" value={pending} tone="warning" sub="submitted + queued" />
        <StatTile label="Completion Rate" value={rate != null ? `${rate}%` : '—'} sub="of finished jobs" />
        <StatTile label="Completed" value={completed} tone="accent" />
        <StatTile label="Evicted" value={evicted} tone="purple" />
        <StatTile label="Failed" value={failed} tone="danger" />
      </div>

      <div className="grid grid-cols-[minmax(0,1.1fr)_minmax(0,1fr)_minmax(0,1fr)] gap-3">
        {/* Job status breakdown as donut chart */}
        <Card>
          <CardHeader>Job Status Breakdown</CardHeader>
          <CardContent>
            {total === 0 ? (
              <EmptyState>No jobs yet.</EmptyState>
            ) : (
              <div className="flex items-center gap-3">
                <div className="h-[140px] w-[140px] flex-none">
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
                <div className="flex min-w-0 flex-1 flex-col gap-1.5">
                  {statusBreakdown.map(([s, n]) => (
                    <div
                      key={s}
                      tabIndex={0}
                      role="link"
                      className="flex cursor-pointer items-center gap-2"
                      onClick={() => router.push(`/jobs`)}
                      onKeyDown={e => { if (e.key === 'Enter') router.push('/jobs') }}
                    >
                      <Dot color={statusColor(s)} />
                      <Badge status={s} className="flex-none">{s}</Badge>
                      <span className="ml-auto font-mono text-2xs text-muted">
                        {n} · {Math.round(n / total * 100)}%
                      </span>
                    </div>
                  ))}
                </div>
              </div>
            )}
          </CardContent>
        </Card>

        {/* Capacity tier breakdown */}
        <Card>
          <CardHeader>Jobs by Capacity Tier</CardHeader>
          <CardContent>
            <Table>
              <thead>
                <tr><th>Tier</th><th>Running</th><th>Total</th><th>Evicted</th></tr>
              </thead>
              <tbody>
                <tr>
                  <td><Badge status="guaranteed">Guaranteed</Badge></td>
                  <td className="font-mono text-xs">{tierRunning.guaranteed ?? 0}</td>
                  <td className="font-mono text-xs">{tierTotal.guaranteed ?? 0}</td>
                  <td className="font-mono text-xs">{tierEvicted.guaranteed ?? 0}</td>
                </tr>
                <tr>
                  <td><Badge status="burst">Burst</Badge></td>
                  <td className="font-mono text-xs">{tierRunning.burst ?? 0}</td>
                  <td className="font-mono text-xs">{tierTotal.burst ?? 0}</td>
                  <td className="font-mono text-xs">{tierEvicted.burst ?? 0}</td>
                </tr>
              </tbody>
            </Table>
          </CardContent>
        </Card>

        {/* Eviction breakdown by reason */}
        <Card>
          <CardHeader>Eviction Reasons</CardHeader>
          <CardContent>
            {faultSummary && (
              <div className="mb-2 text-2xs text-muted">{faultSummary}</div>
            )}
            {evictionChartData.length === 0 ? (
              <EmptyState>No evictions recorded.</EmptyState>
            ) : (
              <div className="w-full" style={{ height: evictionChartData.length * 30 + 30 }}>
                <ResponsiveContainer width="100%" height="100%">
                  <BarChart data={evictionChartData} layout="vertical" margin={{ left: 0, right: 12, top: 4, bottom: 4 }}>
                    <CartesianGrid stroke={chart.grid} horizontal={false} />
                    <XAxis type="number" tick={chart.tick} axisLine={false} tickLine={false} allowDecimals={false} />
                    <YAxis
                      type="category"
                      dataKey="reason"
                      width={170}
                      interval={0}
                      tick={chart.tick}
                      axisLine={false}
                      tickLine={false}
                    />
                    <Tooltip content={<ChartTooltip />} cursor={chart.tooltip.cursor} />
                    <Bar dataKey="count" fill={statusColor('EVICTED')} radius={[0, 4, 4, 0]} barSize={14} />
                  </BarChart>
                </ResponsiveContainer>
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      {/* Recent evictions — audit log */}
      {recentEvictions.length > 0 && (
        <Card>
          <CardHeader>Recent Evictions · Audit Log</CardHeader>
          <CardContent scrollX>
            <Table>
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
                    className="cursor-pointer"
                    onClick={() => router.push(`/jobs/${e.id}`)}
                    onKeyDown={ev => { if (ev.key === 'Enter') router.push(`/jobs/${e.id}`) }}
                  >
                    <td className="whitespace-nowrap font-mono text-2xs text-subtle">{e.id.slice(0, 8)}…</td>
                    <td className="whitespace-nowrap font-mono text-xs">{e.agent_id}</td>
                    <td className="text-xs text-accent">
                      <PlatformExperimentName id={e.platform_experiment_id} />
                    </td>
                    <td className="text-xs">
                      <Badge status="evicted">{evictionCodeLabel(evictionCode(e.eviction_reason))}</Badge>
                      <div className="mt-0.5 text-2xs text-subtle">{evictionDetail(e.eviction_reason)}</div>
                    </td>
                    <td className="font-mono text-2xs">{e.accelerator_count}× {acceleratorLabel(e.accelerator_type)}</td>
                    <td><Badge status={e.capacity_tier ?? 'unknown'}>{e.capacity_tier ?? '—'}</Badge></td>
                  </tr>
                ))}
              </tbody>
            </Table>
          </CardContent>
        </Card>
      )}

      {staff && <SchedulerQuality />}
    </div>
  )
}
