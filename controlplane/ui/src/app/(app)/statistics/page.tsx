'use client'

import useSWR from 'swr'
import { fetchExperiments, fetchExperimentStats, fetchAllExperimentStats, fetchAllClusters } from '@/lib/api'
import { useTenant } from '@/lib/tenant'
import type { Experiment } from '@/types'
import { Page } from '@/components/ui/page'
import { PlatformExperimentName } from '@/components/platform-experiment-name'
import { RowLink } from '@/components/ui/card'
import { Section } from '@/components/ui/section'
import { StatTile } from '@/components/ui/stat-tile'
import { Badge, TierBadge } from '@/components/ui/badge'
import { EmptyState } from '@/components/ui/empty-state'
import { Inline, Stack } from '@/components/ui/stack'
import { Text } from '@/components/ui/text'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { LinkTr, Table, num, quiet } from '@/components/ui/table'
import { ResponsiveContainer, PieChart, Pie, Cell, Tooltip } from 'recharts'
import { chart, statusColor } from '@/lib/colors'
import { evictionCode, evictionCodeLabel, evictionDetailLines } from '@/lib/eviction'
import { acceleratorLabel, shortId } from '@/lib/format'
import { occupancy } from '@/lib/units'

type Job = Record<string, any>

// How many of the newest evicted jobs the audit log shows.
const RECENT_EVICTIONS = 10

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
  const chips = occupancy(clusterList.reduce((s, c) => s + c.accelerator_busy, 0), clusterList.reduce((s, c) => s + c.accelerator_total, 0), 'chips')
  // Cores clusters offer to jobs on CPU, counted apart from the chips.
  const cores = occupancy(clusterList.reduce((s, c) => s + (c.core_busy ?? 0), 0), clusterList.reduce((s, c) => s + (c.core_total ?? 0), 0), 'cores')

  return (
    <Section title="Scheduler quality" note="every organization, staff only">
        <div className="mb-4 grid grid-cols-tiles gap-3">
          <StatTile label="Total jobs" value={total} />
          <StatTile
            label="Clusters"
            value={`${connectedCount}/${clusterList.length}`}
            tone={connectedCount === clusterList.length && clusterList.length > 0 ? 'success' : 'danger'}
            sub="connected / registered"
          />
          <StatTile
            label="Accelerators in use"
            value={chips.percent}
            sub={chips.detail ?? 'no live capacity reported'}
          />
          {cores.detail && <StatTile label="CPU cores in use" value={cores.percent} sub={cores.detail} />}
        </div>
        <Table className="max-w-2xl">
          <thead>
            <tr><th>Metric</th><th>Definition</th><th className="text-right">Value</th></tr>
          </thead>
          <tbody>
            <tr>
              <td>Completion rate</td>
              <td className="text-xs text-muted">Completed ÷ (completed + failed + evicted)</td>
              <td className={num}>{rate != null ? `${rate}%` : '—'}</td>
            </tr>
            <tr>
              <td>Early-stop rate</td>
              <td className="text-xs text-muted">Evicted ÷ (completed + evicted)</td>
              <td className={num}>{completed + evicted > 0 ? `${Math.round(evicted / (completed + evicted) * 100)}%` : '—'}</td>
            </tr>
            <tr>
              <td>Clusters connected</td>
              <td className="text-xs text-muted">Registered clusters that are reporting in</td>
              <td className={num}>{clusterList.length > 0 ? `${connectedCount}/${clusterList.length}` : '—'}</td>
            </tr>
            <tr>
              <td>Crash rate</td>
              <td className="text-xs text-muted">Failed jobs ÷ total</td>
              <td className={num}>{total > 0 ? `${Math.round(failed / total * 100)}%` : '—'}</td>
            </tr>
            <tr>
              <td>Burst eviction rate</td>
              <td className="text-xs text-muted">Burst evictions ÷ total burst jobs</td>
              <td className={num}>{burstTotal > 0 ? `${Math.round(burstEvicted / burstTotal * 100)}%` : '—'}</td>
            </tr>
          </tbody>
        </Table>
    </Section>
  )
}

export default function StatisticsPage() {
  const { staff } = useTenant()
  if (!staff) return (
    <Page title="Statistics">
      <EmptyState>
        This page is for platform staff. Your jobs, by status and with the reasons any were evicted, are on the Jobs page.
      </EmptyState>
    </Page>
  )
  return <StaffStatistics />
}

function StaffStatistics() {
  const { project } = useTenant()

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
  const completed = byStatus.COMPLETED ?? 0
  const failed = byStatus.FAILED ?? 0
  const evicted = byStatus.EVICTED ?? 0

  const tierTotal = stats?.by_capacity_tier ?? {}
  const tierRunning = stats?.running_by_capacity_tier ?? {}
  const tierEvicted = stats?.evicted_by_capacity_tier ?? {}

  const rate = completionRate(completed, failed, evicted)

  const recentEvictions: Job[] = (evictedJobs ?? []) as Job[]

  const statusBreakdown = Object.entries(byStatus).sort((a, b) => b[1] - a[1])

  return (
    <Page title="Statistics" description={`How the jobs of ${project.name} ended, by status and by capacity tier, and how well the platform places and finishes work.`}>
      {isLoading && <Loading />}
      {error && <ErrorMessage>Could not load the job statistics of {project.name}: {error.message}</ErrorMessage>}

      <div className="grid grid-cols-2 gap-3">
        <Section title="Jobs by status" note={rate != null && `${rate}% of finished jobs completed`}>
            {total === 0 ? (
              <EmptyState>No jobs yet.</EmptyState>
            ) : (
              <Inline gap="md" nowrap>
                <div className="size-36 flex-none">
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
                      <Tooltip contentStyle={chart.tooltip.contentStyle} labelStyle={chart.tooltip.labelStyle} />
                    </PieChart>
                  </ResponsiveContainer>
                </div>
                <Stack gap="xs" className="min-w-0 flex-1">
                  {statusBreakdown.map(([s, n]) => (
                    <RowLink key={s} href={`/jobs?status=${s}`} className="px-1 py-0.5">
                      <Badge status={s} className="flex-none">{s}</Badge>
                      <Text as="span" variant="meta" mono className="ml-auto tabular-nums">
                        {n} · {Math.round(n / total * 100)}%
                      </Text>
                    </RowLink>
                  ))}
                </Stack>
              </Inline>
            )}
        </Section>

        <Section title="Jobs by capacity tier">
            <Table>
              <thead>
                <tr><th>Tier</th><th className="text-right">Running</th><th className="text-right">Total</th><th className="text-right">Evicted</th></tr>
              </thead>
              <tbody>
                <tr>
                  <td><Badge status="guaranteed">Guaranteed</Badge></td>
                  <td className={num}>{tierRunning.guaranteed ?? 0}</td>
                  <td className={num}>{tierTotal.guaranteed ?? 0}</td>
                  <td className={num}>{tierEvicted.guaranteed ?? 0}</td>
                </tr>
                <tr>
                  <td><Badge status="burst">Burst</Badge></td>
                  <td className={num}>{tierRunning.burst ?? 0}</td>
                  <td className={num}>{tierTotal.burst ?? 0}</td>
                  <td className={num}>{tierEvicted.burst ?? 0}</td>
                </tr>
              </tbody>
            </Table>
        </Section>
      </div>

      {recentEvictions.length > 0 && (
        <Section title="Recent evictions" note={`newest ${RECENT_EVICTIONS}`} scrollX>
            <Table>
              <thead>
                <tr>
                  <th>Job ID</th>
                  <th>Agent</th>
                  <th>Experiment</th>
                  <th>Reason</th>
                  <th>Accelerator</th>
                  <th>Tier</th>
                </tr>
              </thead>
              <tbody>
                {recentEvictions.map(e => (
                  <LinkTr key={e.id} href={`/jobs/${e.id}`}>
                    <td className={quiet}>{shortId(e.id)}</td>
                    <td className="whitespace-nowrap text-xs" title={e.agent_id}>{e.agent_name}</td>
                    <td className="text-xs">
                      <PlatformExperimentName id={e.platform_experiment_id} />
                    </td>
                    <td className="text-xs">
                      <Badge status="evicted">{evictionCodeLabel(evictionCode(e.eviction_reason))}</Badge>
                      <Text as="div" variant="meta" tone="subtle">
                        {evictionDetailLines(e.eviction_reason).map((line, i) => <div key={i}>{line}</div>)}
                      </Text>
                    </td>
                    <td className="font-mono text-2xs">{e.accelerator_count}× {acceleratorLabel(e.accelerator_type)}</td>
                    <td><TierBadge tier={e.capacity_tier} /></td>
                  </LinkTr>
                ))}
              </tbody>
            </Table>
        </Section>
      )}

      <SchedulerQuality />
    </Page>
  )
}
