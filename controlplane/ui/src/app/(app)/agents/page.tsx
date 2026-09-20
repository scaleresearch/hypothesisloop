'use client'

import { Fragment, Suspense, useMemo, useState } from 'react'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { Link } from '@/components/ui/link'
import { useReactTable, getCoreRowModel, getSortedRowModel, type SortingState } from '@tanstack/react-table'
import {
  fetchAgentsPage, fetchAgentStats, fetchPlatformExperiment, fetchPlatformExperimentsPage, fetchPlatformExperimentQuotas,
  fetchDonationsPage, fetchExperimentsPage, fetchExperimentStats, fetchPlatformExperimentResults, MAX_LIST_PAGE_SIZE,
} from '@/lib/api'
import { quotaRemainingAccH } from '@/lib/quota'
import type { PlatformExperiment, AgentQuota } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { Card, CardHeader, CardContent } from '@/components/ui/card'
import { Badge, AgentKindBadge, NoSummary } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { StatTile } from '@/components/ui/stat-tile'
import { ColumnTh, Table } from '@/components/ui/table'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { PlatformExperimentSelect } from '@/components/platform-experiment-select'
import { Pagination } from '@/components/ui/pagination'
import { formatAccH } from '@/lib/format'
import { useQueryParam } from '@/lib/query-state'
import { useTenant } from '@/lib/tenant'

const PAGE_SIZE = 25

function AgentExperimentQuota({ agentId, pe, colSpan }: { agentId: string; pe: PlatformExperiment; colSpan: number }) {
  const { data: quotas } = useSWR<AgentQuota[]>(
    ['pe-quotas', pe.id],
    () => fetchPlatformExperimentQuotas(pe.id),
    { refreshInterval: 30_000 },
  )
  const q = quotas?.find(q => q.agent_id === agentId)
  if (!q) return null
  const totalRem = quotaRemainingAccH(q)
  return (
    <tr className="bg-foreground/5">
      <td colSpan={colSpan} className="pl-7 font-mono text-2xs">
        <div className="flex flex-wrap items-center gap-6">
          <Link href={`/platform-experiments/${pe.id}`} className="text-xs text-accent hover:underline">{pe.name}</Link>
          <span>
            <Badge status="guaranteed" className="mr-1">G</Badge>
            {formatAccH(q.used_guaranteed_acch)} / {formatAccH(q.guaranteed_accelerator_hours)} AccH used
          </span>
          <span>
            <Badge status="burst" className="mr-1">B</Badge>
            {formatAccH(q.used_burst_acch)} / {formatAccH(q.burst_accelerator_hours)} AccH used
          </span>
          <span className={totalRem > 0 ? 'text-success' : 'text-danger'}>{formatAccH(totalRem)} AccH left</span>
        </div>
      </td>
    </tr>
  )
}

function AgentQuotaRows({ agentId, experiments, colSpan }: { agentId: string; experiments: PlatformExperiment[]; colSpan: number }) {
  if (experiments.length === 0) return (
    <tr><td colSpan={colSpan} className="pl-5 text-xs text-subtle">No running platform experiments</td></tr>
  )
  return <>{experiments.map(pe => <AgentExperimentQuota key={pe.id} agentId={agentId} pe={pe} colSpan={colSpan} />)}</>
}

export default function AgentsPage() {
  return (
    <Suspense fallback={<Loading />}>
      <AgentsPageContent />
    </Suspense>
  )
}

function AgentsPageContent() {
  const { project } = useTenant()
  const [expandedAgent, setExpandedAgent] = useState<string | null>(null)
  const [experimentFilter, setExperimentFilter] = useQueryParam('platform_experiment_id')
  const [page, setPage] = useState(0)
  const [donationPage, setDonationPage] = useState(0)

  const { data, error, isLoading } = useSWR(
    ['agents-page', project.id, experimentFilter, page],
    () => fetchAgentsPage({ project_id: project.id, platform_experiment_id: experimentFilter, limit: PAGE_SIZE, offset: page * PAGE_SIZE }),
    { refreshInterval: 15_000, keepPreviousData: true },
  )
  const agents = data?.items
  const total = data?.total ?? 0

  const { data: stats } = useSWR(
    ['agent-stats', project.id, experimentFilter],
    () => fetchAgentStats({ project_id: project.id, platform_experiment_id: experimentFilter }),
    { refreshInterval: 15_000, keepPreviousData: true },
  )

  // Quota exists from an experiment's start, so only running ones have a row to show.
  const { data: running } = useSWR<PlatformExperiment[]>(
    ['platform-experiments-running', project.id],
    () => fetchPlatformExperimentsPage({ project_id: project.id, status: 'running', limit: MAX_LIST_PAGE_SIZE }).then(p => p.items),
    { refreshInterval: 30_000 },
  )

  const { data: donations } = useSWR(
    ['donations-open', project.id, donationPage],
    () => fetchDonationsPage({ project_id: project.id, status: 'open', limit: PAGE_SIZE, offset: donationPage * PAGE_SIZE }),
    { refreshInterval: 15_000, keepPreviousData: true },
  )
  const donationItems = donations?.items ?? []
  const donationTotal = donations?.total ?? 0

  const { data: selectedPE } = useSWR(
    experimentFilter ? ['pe', experimentFilter] : null,
    () => fetchPlatformExperiment(experimentFilter),
  )

  // What each agent did in the selected experiment: its jobs, its best result and standing, and the
  // completed jobs it left without a summary (tallied from at most the 50 newest of them).
  const { data: jobStats } = useSWR(
    experimentFilter ? ['pe-stats-by-agent', experimentFilter] : null,
    () => fetchExperimentStats({ project_id: project.id, platform_experiment_id: experimentFilter, group_by: 'agent' }),
    { refreshInterval: 15_000 },
  )
  const { data: results } = useSWR(
    experimentFilter ? ['pe-results', experimentFilter] : null,
    () => fetchPlatformExperimentResults(experimentFilter),
    { refreshInterval: 15_000 },
  )
  const { data: unsummarized } = useSWR(
    experimentFilter ? ['pe-unsummarized-jobs', experimentFilter] : null,
    () => fetchExperimentsPage({ project_id: project.id, platform_experiment_id: experimentFilter, needs_summary: true, limit: 50 }).then(p => p.items),
    { refreshInterval: 15_000 },
  )
  const ranking = results?.metrics[0]
  const colSpan = experimentFilter ? 7 : 4

  const [sorting, setSorting] = useState<SortingState>([{ id: 'performance_score', desc: true }])
  const agentColumns = useMemo(() => [
    { accessorKey: 'id', header: 'Agent ID' },
    { accessorKey: 'top3_count', header: 'Bonus Eligibility' },
    { accessorKey: 'performance_score', header: 'Perf Score' },
  ], [])
  const agentTable = useReactTable({
    data: agents ?? [],
    columns: agentColumns,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
  })
  const sortedAgents = agentTable.getRowModel().rows.map(r => r.original)

  const baseShare = selectedPE && selectedPE.signup_count > 0 ? selectedPE.budget_accelerator_hours / selectedPE.signup_count : 0

  return (
    <div>
      <PageHeader
        title="Research Agents"
        description="Researcher agent bonus eligibility and per-experiment quota allocation"
      />

      {/* KPI row: every agent the filter matches. */}
      <div className="mb-4 grid grid-cols-4 gap-2.5">
        <StatTile label="Registered Agents" value={total || '—'} />
        <StatTile label="Top-3 Eligible" value={stats?.top3_eligible ?? '—'} tone="success" sub="+25% quota bonus" />
        <StatTile label="Avg Perf Bonus" value={stats?.total ? stats.avg_performance_score.toFixed(3) : '—'} />
        <StatTile label="Open Donation Requests" value={donationTotal} tone={donationTotal > 0 ? 'warning' : undefined} />
      </div>

      <div className="grid grid-cols-2 items-start gap-3">
        {/* Experiment filter */}
        <Card>
          <CardHeader>Filter</CardHeader>
          <CardContent className="flex flex-wrap items-center gap-2.5">
            <span className="text-xs text-muted">By experiment:</span>
            <PlatformExperimentSelect
              value={experimentFilter}
              onChange={v => { setExperimentFilter(v); setPage(0) }}
              allLabel="All experiments"
            />
            {experimentFilter && (
              <Button size="sm" onClick={() => { setExperimentFilter(''); setPage(0) }}>Clear</Button>
            )}
          </CardContent>
        </Card>

        {/* Bonus formula — the underlying formula only produces a concrete number within a
            single platform experiment (base_share depends on that experiment's own budget and
            signup count), so show real computed numbers once one is selected; otherwise show
            the formula as a reference only. */}
        {selectedPE ? (
          <Card>
            <CardHeader>Quota Formula — {selectedPE.name}</CardHeader>
            <CardContent>
              <Table>
                <tbody>
                  <tr><td>Base share</td><td className="font-mono text-xs">{formatAccH(selectedPE.budget_accelerator_hours)} ÷ {selectedPE.signup_count} = {formatAccH(baseShare)} AccH</td></tr>
                  <tr><td>Top-3 bonus <Badge tone="success" className="ml-1">+25%</Badge></td><td className="font-mono text-xs text-muted">any top-3 placement in prior experiments</td></tr>
                  <tr><td>Guaranteed quota</td><td className="font-mono text-xs text-muted">{formatAccH(baseShare)} × (1 + bonus)</td></tr>
                  <tr><td>Burst quota</td><td className="font-mono text-xs text-muted">guaranteed × 2.0 — preemptable</td></tr>
                </tbody>
              </Table>
            </CardContent>
          </Card>
        ) : (
          <Card>
            <CardHeader>Quota Bonus Formula <span className="text-2xs font-normal text-subtle">(reference — select an experiment for real numbers)</span></CardHeader>
            <CardContent>
              <Table>
                <tbody>
                  <tr><td>Base share</td><td className="font-mono text-xs text-muted">total_budget_AccH ÷ signed_up_agent_count</td></tr>
                  <tr><td>Top-3 bonus <Badge tone="success" className="ml-1">+25%</Badge></td><td className="font-mono text-xs text-muted">any top-3 placement in prior experiments</td></tr>
                  <tr><td>Guaranteed quota</td><td className="font-mono text-xs text-muted">base_share × (1 + bonus)</td></tr>
                  <tr><td>Burst quota</td><td className="font-mono text-xs text-muted">guaranteed × 2.0 — preemptable</td></tr>
                </tbody>
              </Table>
            </CardContent>
          </Card>
        )}
      </div>

      {isLoading && <Loading />}
      {error && <ErrorMessage>Cannot reach quota service.</ErrorMessage>}

      {/* Agent table */}
      <Card>
        <CardHeader>
          Registered Agents
          {agents && <span className="text-xs font-normal text-subtle">({total})</span>}
        </CardHeader>
        <CardContent scrollX>
          <Table>
            <thead>
              <tr>
                {agentTable.getFlatHeaders().map(h => (
                  <ColumnTh
                    key={h.id}
                    header={h}
                    className={clsx(h.id === 'top3_count' && 'text-center', h.id === 'performance_score' && 'text-right')}
                  />
                ))}
                {experimentFilter && (
                  <>
                    <th className="text-right">Best {ranking?.metric ?? 'metric'}</th>
                    <th>Jobs</th>
                    <th>Summaries filed</th>
                  </>
                )}
                <th></th>
              </tr>
            </thead>
            <tbody>
              {!agents || agents.length === 0 ? (
                <tr>
                  <td colSpan={colSpan} className="p-0">
                    <EmptyState>
                      {experimentFilter ? 'No agents signed up for this experiment.' : 'No agents registered.'}
                    </EmptyState>
                  </td>
                </tr>
              ) : sortedAgents.map(a => {
                const isExpanded = expandedAgent === a.id
                const standing = ranking?.standings.find(st => st.agent_id === a.id)
                const jobs = jobStats?.groups?.[a.id]
                const completed = jobs?.by_status.COMPLETED ?? 0
                const missing = (unsummarized ?? []).filter(j => j.agent_id === a.id)
                return (
                  <Fragment key={a.id}>
                    <tr className="cursor-pointer" onClick={() => setExpandedAgent(isExpanded ? null : a.id)}>
                      <td className="font-mono text-xs font-bold">
                        {a.id}
                        <AgentKindBadge kind={a.kind} className="ml-2" />
                      </td>
                      <td className="text-center">
                        {a.top3_count > 0 ? <Badge tone="success">+25% Top-3</Badge> : <span className="text-xs text-subtle">—</span>}
                      </td>
                      <td className="text-right font-mono text-xs">{a.performance_score.toFixed(3)}</td>
                      {experimentFilter && (
                        <>
                          <td className="text-right font-mono text-xs">
                            {standing?.experiment_id ? (
                              <Link href={`/jobs/${standing.experiment_id}`} className="text-accent hover:underline" onClick={e => e.stopPropagation()}>
                                #{standing.rank} · {standing.best.toFixed(4)}
                              </Link>
                            ) : <span className="text-xs text-subtle">—</span>}
                            {missing.some(j => j.id === standing?.experiment_id) && <div><NoSummary /></div>}
                          </td>
                          <td className="font-mono text-2xs">
                            {jobs
                              ? Object.entries(jobs.by_status).map(([status, n]) => `${n} ${status.toLowerCase()}`).join(' · ')
                              : <span className="text-xs text-subtle">none</span>}
                          </td>
                          <td className={clsx('font-mono text-2xs', missing.length > 0 && 'text-warning')}>
                            {completed - missing.length} of {completed} completed
                          </td>
                        </>
                      )}
                      <td className="text-xs text-subtle">{isExpanded ? '▲' : '▼'}</td>
                    </tr>
                    {isExpanded && (selectedPE ? [selectedPE] : running) && (
                      <AgentQuotaRows agentId={a.id} experiments={(selectedPE ? [selectedPE] : running)!} colSpan={colSpan} />
                    )}
                  </Fragment>
                )
              })}
            </tbody>
          </Table>
          <Pagination page={page} pageSize={PAGE_SIZE} total={total} onPageChange={setPage} />
        </CardContent>
      </Card>

      {/* Compute Donation Requests */}
      <Card>
        <CardHeader>
          Compute Donation Requests
          {donationTotal > 0 && <span className="font-normal text-accent">({donationTotal} open)</span>}
        </CardHeader>
        <CardContent>
          {donationItems.length === 0 ? (
            <EmptyState>No open donation requests. Agents can request extra compute via POST /donations.</EmptyState>
          ) : (
            <Table>
              <thead>
                <tr>
                  <th>Agent</th>
                  <th className="text-right">Wants (AccH)</th>
                  <th>Reason</th>
                  <th>Status</th>
                  <th>Requested</th>
                </tr>
              </thead>
              <tbody>
                {donationItems.map(d => (
                  <tr key={d.id}>
                    <td className="font-mono text-xs font-semibold">{d.agent_name || d.agent_id}</td>
                    <td className="text-right font-mono text-xs">{d.credits_want}</td>
                    <td className="max-w-[300px] text-xs text-muted">{d.reason}</td>
                    <td><Badge status={d.status}>{d.status}</Badge></td>
                    <td className="text-2xs text-subtle">{new Date(d.created_at).toLocaleString()}</td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
          <Pagination page={donationPage} pageSize={PAGE_SIZE} total={donationTotal} onPageChange={setDonationPage} />
        </CardContent>
      </Card>
    </div>
  )
}
