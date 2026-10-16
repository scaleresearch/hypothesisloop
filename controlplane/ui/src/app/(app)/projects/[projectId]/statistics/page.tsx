'use client'

import useSWR from 'swr'
import { fetchExperiments, fetchExperimentStats, fetchAllExperimentStats, fetchAllClusters } from '@/lib/api'
import { useTenant } from '@/lib/tenant'
import { jobPath, projectPath, withQuery } from '@/lib/routes'
import { Page } from '@/components/ui/page'
import { PlatformExperimentName } from '@/components/platform-experiment-name'
import { RowLink } from '@/components/ui/card'
import { Section } from '@/components/ui/section'
import { StatTile } from '@/components/ui/stat-tile'
import { Badge, TierBadge } from '@/components/ui/badge'
import { EmptyState } from '@/components/ui/empty-state'
import { Fetched } from '@/components/ui/fetched'
import { Inline, Stack } from '@/components/ui/stack'
import { Text } from '@/components/ui/text'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { LinkTr, RowAnchor, Table, num, quiet } from '@/components/ui/table'
import { ResponsiveContainer, PieChart, Pie, Cell, Tooltip } from 'recharts'
import { chart, statusColor } from '@/lib/colors'
import { evictionCode, evictionCodeLabel, evictionDetailLines } from '@/lib/eviction'
import { acceleratorLabel, shortId } from '@/lib/format'
import { occupancy } from '@/lib/units'

// How many of the newest evicted jobs the audit log shows.
const RECENT_EVICTIONS = 10

/** A share as a whole percentage, or a dash when there is nothing to take a share of. */
function percent(part: number, whole: number): string {
  return whole > 0 ? `${Math.round((part / whole) * 100)}%` : '—'
}

// Completion rate: of jobs that reached a terminal state, the share that completed successfully
// rather than failing or being evicted.
function completionRate(byStatus: Record<string, number>): string {
  const completed = byStatus.COMPLETED ?? 0
  return percent(completed, completed + (byStatus.FAILED ?? 0) + (byStatus.EVICTED ?? 0))
}

// How well the platform as a whole places and finishes work, over every org's jobs and clusters.
// The API answers these reads for staff alone.
function SchedulerQuality() {
  const { data: stats, error: statsError } = useSWR('all-experiment-stats', fetchAllExperimentStats, { refreshInterval: 15_000 })
  const { data: clusters, error: clustersError } = useSWR('all-clusters', () => fetchAllClusters().then(r => r.clusters), { refreshInterval: 5_000 })

  const connected = clusters?.filter(c => c.connected).length ?? 0
  // Real hardware occupancy, busy against total accelerators in each connected cluster's latest
  // reconcile snapshot, which says whether chips are idle where a budget ratio only says what was spent.
  const chips = clusters && occupancy(clusters.reduce((s, c) => s + c.accelerator_busy, 0), clusters.reduce((s, c) => s + c.accelerator_total, 0), 'chips')
  // Cores clusters offer to jobs on CPU, counted apart from the chips.
  const cores = clusters && occupancy(clusters.reduce((s, c) => s + (c.core_busy ?? 0), 0), clusters.reduce((s, c) => s + (c.core_total ?? 0), 0), 'cores')
  const ofStats = (figure: (s: NonNullable<typeof stats>) => string) => <Fetched data={stats} error={statsError}>{figure}</Fetched>

  return (
    <Section title="Scheduler quality" note="every organization, staff only">
      <div className="mb-4 grid grid-cols-tiles gap-3">
        <StatTile label="Total jobs" value={ofStats(s => String(s.total))} />
        <StatTile
          label="Clusters"
          value={<Fetched data={clusters} error={clustersError}>{cs => `${connected}/${cs.length}`}</Fetched>}
          tone={clusters && connected === clusters.length && clusters.length > 0 ? 'success' : clusters ? 'danger' : undefined}
          sub="connected / registered"
        />
        <StatTile
          label="Accelerators in use"
          value={<Fetched data={chips} error={clustersError}>{c => c.percent}</Fetched>}
          sub={chips ? chips.detail ?? 'no live capacity reported' : undefined}
        />
        {cores?.detail && <StatTile label="CPU cores in use" value={cores.percent} sub={cores.detail} />}
      </div>
      <Table className="max-w-2xl">
        <thead>
          <tr><th>Metric</th><th>Definition</th><th className="text-right">Value</th></tr>
        </thead>
        <tbody>
          <tr>
            <td>Completion rate</td>
            <td className="text-xs text-muted">Completed ÷ (completed + failed + evicted)</td>
            <td className={num}>{ofStats(s => completionRate(s.by_status))}</td>
          </tr>
          <tr>
            <td>Early-stop rate</td>
            <td className="text-xs text-muted">Evicted ÷ (completed + evicted)</td>
            <td className={num}>{ofStats(s => percent(s.by_status.EVICTED ?? 0, (s.by_status.COMPLETED ?? 0) + (s.by_status.EVICTED ?? 0)))}</td>
          </tr>
          <tr>
            <td>Clusters connected</td>
            <td className="text-xs text-muted">Registered clusters that are reporting in</td>
            <td className={num}><Fetched data={clusters} error={clustersError}>{cs => cs.length > 0 ? `${connected}/${cs.length}` : '—'}</Fetched></td>
          </tr>
          <tr>
            <td>Crash rate</td>
            <td className="text-xs text-muted">Failed jobs ÷ total</td>
            <td className={num}>{ofStats(s => percent(s.by_status.FAILED ?? 0, s.total))}</td>
          </tr>
          <tr>
            <td>Burst eviction rate</td>
            <td className="text-xs text-muted">Burst evictions ÷ total burst jobs</td>
            <td className={num}>{ofStats(s => percent(s.evicted_by_capacity_tier?.burst ?? 0, s.by_capacity_tier?.burst ?? 0))}</td>
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
  const { data: stats, error } = useSWR(
    ['experiment-stats', project.id],
    () => fetchExperimentStats({ project_id: project.id }),
    { refreshInterval: 15_000 },
  )

  // The audit log below is the one place that needs actual rows, the newest handful of them.
  const { data: recentEvictions, error: evictionsError } = useSWR(
    ['recent-evictions', project.id],
    () => fetchExperiments({ project_id: project.id, status: 'EVICTED', sort: '-updated_at', limit: RECENT_EVICTIONS }),
    { refreshInterval: 15_000 },
  )

  return (
    <Page title="Statistics">
      {error && <ErrorMessage>Could not load the job statistics of {project.name}: {error.message}</ErrorMessage>}
      {!stats && !error && <Loading />}

      {stats && <ProjectStatistics stats={stats} />}

      {evictionsError && <ErrorMessage>Could not load the recent evictions of {project.name}: {evictionsError.message}</ErrorMessage>}
      {recentEvictions && recentEvictions.length > 0 && (
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
                <LinkTr key={e.id} href={jobPath(e.platform_experiment_id, e.id)}>
                  <td className={quiet}><RowAnchor>{shortId(e.id)}</RowAnchor></td>
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

function ProjectStatistics({ stats }: { stats: Awaited<ReturnType<typeof fetchExperimentStats>> }) {
  const { project } = useTenant()
  const statusBreakdown = Object.entries(stats.by_status).sort((a, b) => b[1] - a[1])
  const rate = completionRate(stats.by_status)
  const tiers = [
    { tier: 'guaranteed', label: 'Guaranteed' },
    { tier: 'burst', label: 'Burst' },
  ] as const

  return (
    <div className="grid grid-cols-2 gap-3">
      <Section title="Jobs by status" note={rate !== '—' && `${rate} of finished jobs completed`}>
        {stats.total === 0 ? (
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
                <RowLink key={s} href={withQuery(projectPath(project.id, 'jobs'), { status: s })} className="px-1 py-0.5">
                  <Badge status={s} className="flex-none">{s}</Badge>
                  <Text as="span" variant="meta" mono className="ml-auto tabular-nums">
                    {n} · {percent(n, stats.total)}
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
            {tiers.map(({ tier, label }) => (
              <tr key={tier}>
                <td><Badge status={tier}>{label}</Badge></td>
                <td className={num}>{stats.running_by_capacity_tier?.[tier] ?? 0}</td>
                <td className={num}>{stats.by_capacity_tier?.[tier] ?? 0}</td>
                <td className={num}>{stats.evicted_by_capacity_tier?.[tier] ?? 0}</td>
              </tr>
            ))}
          </tbody>
        </Table>
      </Section>
    </div>
  )
}
