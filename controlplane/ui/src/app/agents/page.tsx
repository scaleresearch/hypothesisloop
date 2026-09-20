'use client'

import { Fragment, Suspense, useMemo, useState } from 'react'
import useSWR from 'swr'
import Link from 'next/link'
import { useReactTable, getCoreRowModel, getSortedRowModel, type SortingState } from '@tanstack/react-table'
import {
  fetchAgentsPage, fetchAgentStats, fetchPlatformExperiment, fetchPlatformExperimentsPage, fetchPlatformExperimentQuotas,
  fetchDonationsPage, fetchExperimentsPage, fetchExperimentStats, fetchPlatformExperimentResults, MAX_LIST_PAGE_SIZE,
} from '@/lib/api'
import { quotaRemainingAccH } from '@/lib/quota'
import type { PlatformExperiment, AgentQuota } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Badge, AgentKindBadge, NoSummary } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { StatTile } from '@/components/ui/stat-tile'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { PlatformExperimentSelect } from '@/components/platform-experiment-select'
import { Pagination } from '@/components/ui/pagination'
import { semantic } from '@/lib/colors'
import { formatAccH } from '@/lib/format'
import { useQueryParam } from '@/lib/query-state'

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
    <tr style={{ background: 'var(--muted)' }}>
      <td colSpan={colSpan} className="mono" style={{ paddingLeft: 28, fontSize: 11 }}>
        <div style={{ display: 'flex', gap: 24, alignItems: 'center', flexWrap: 'wrap' }}>
          <Link href={`/platform-experiments/${pe.id}`} className="text-link" style={{ fontSize: 12 }}>{pe.name}</Link>
          <span>
            <Badge status="guaranteed" className="mr-1">G</Badge>
            {formatAccH(q.used_guaranteed_acch)} / {formatAccH(q.guaranteed_accelerator_hours)} AccH used
          </span>
          <span>
            <Badge status="burst" className="mr-1">B</Badge>
            {formatAccH(q.used_burst_acch)} / {formatAccH(q.burst_accelerator_hours)} AccH used
          </span>
          <span style={{ color: totalRem > 0 ? semantic.success : semantic.danger }}>{formatAccH(totalRem)} AccH left</span>
        </div>
      </td>
    </tr>
  )
}

function AgentQuotaRows({ agentId, experiments, colSpan }: { agentId: string; experiments: PlatformExperiment[]; colSpan: number }) {
  if (experiments.length === 0) return (
    <tr><td colSpan={colSpan} className="text-dim" style={{ fontSize: 12, paddingLeft: 20 }}>No running platform experiments</td></tr>
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
  const [expandedAgent, setExpandedAgent] = useState<string | null>(null)
  const [experimentFilter, setExperimentFilter] = useQueryParam('platform_experiment_id')
  const [page, setPage] = useState(0)
  const [donationPage, setDonationPage] = useState(0)

  const { data, error, isLoading } = useSWR(
    ['agents-page', experimentFilter, page],
    () => fetchAgentsPage({ platform_experiment_id: experimentFilter, limit: PAGE_SIZE, offset: page * PAGE_SIZE }),
    { refreshInterval: 15_000, keepPreviousData: true },
  )
  const agents = data?.items
  const total = data?.total ?? 0

  const { data: stats } = useSWR(
    ['agent-stats', experimentFilter],
    () => fetchAgentStats(experimentFilter),
    { refreshInterval: 15_000, keepPreviousData: true },
  )

  // Quota exists from an experiment's start, so only running ones have a row to show.
  const { data: running } = useSWR<PlatformExperiment[]>(
    'platform-experiments-running',
    () => fetchPlatformExperimentsPage({ status: 'running', limit: MAX_LIST_PAGE_SIZE }).then(p => p.items),
    { refreshInterval: 30_000 },
  )

  const { data: donations } = useSWR(
    ['donations-open', donationPage],
    () => fetchDonationsPage({ status: 'open', limit: PAGE_SIZE, offset: donationPage * PAGE_SIZE }),
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
    () => fetchExperimentStats({ platform_experiment_id: experimentFilter, group_by: 'agent' }),
    { refreshInterval: 15_000 },
  )
  const { data: results } = useSWR(
    experimentFilter ? ['pe-results', experimentFilter] : null,
    () => fetchPlatformExperimentResults(experimentFilter),
    { refreshInterval: 15_000 },
  )
  const { data: unsummarized } = useSWR(
    experimentFilter ? ['pe-unsummarized-jobs', experimentFilter] : null,
    () => fetchExperimentsPage({ platform_experiment_id: experimentFilter, needs_summary: true, limit: 50 }).then(p => p.items),
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
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, minmax(0, 1fr))', gap: 10, marginBottom: 16 }}>
        <StatTile label="Registered Agents" value={total || '—'} />
        <StatTile label="Top-3 Eligible" value={stats?.top3_eligible ?? '—'} color={semantic.success} sub="+25% quota bonus" />
        <StatTile label="Avg Perf Bonus" value={stats?.total ? stats.avg_performance_score.toFixed(3) : '—'} />
        <StatTile label="Open Donation Requests" value={donationTotal} color={donationTotal > 0 ? semantic.warning : undefined} />
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, alignItems: 'start' }}>
        {/* Experiment filter */}
        <Pod>
          <PodHeader>Filter</PodHeader>
          <PodContent style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
            <span className="text-muted">By experiment:</span>
            <PlatformExperimentSelect
              value={experimentFilter}
              onChange={v => { setExperimentFilter(v); setPage(0) }}
              allLabel="All experiments"
            />
            {experimentFilter && (
              <Button size="sm" onClick={() => { setExperimentFilter(''); setPage(0) }}>Clear</Button>
            )}
          </PodContent>
        </Pod>

        {/* Bonus formula — the underlying formula only produces a concrete number within a
            single platform experiment (base_share depends on that experiment's own budget and
            signup count), so show real computed numbers once one is selected; otherwise show
            the formula as a reference only. */}
        {selectedPE ? (
          <Pod>
            <PodHeader>Quota Formula — {selectedPE.name}</PodHeader>
            <PodContent>
              <table className="wa-table">
                <tbody>
                  <tr><td>Base share</td><td className="mono" style={{ fontSize: 11.5 }}>{formatAccH(selectedPE.budget_accelerator_hours)} ÷ {selectedPE.signup_count} = {formatAccH(baseShare)} AccH</td></tr>
                  <tr><td>Top-3 bonus <span className="badge badge-running" style={{ marginLeft: 4 }}>+25%</span></td><td className="mono text-muted" style={{ fontSize: 11.5 }}>any top-3 placement in prior experiments</td></tr>
                  <tr><td>Guaranteed quota</td><td className="mono text-muted" style={{ fontSize: 11.5 }}>{formatAccH(baseShare)} × (1 + bonus)</td></tr>
                  <tr><td>Burst quota</td><td className="mono text-muted" style={{ fontSize: 11.5 }}>guaranteed × 2.0 — preemptable</td></tr>
                </tbody>
              </table>
            </PodContent>
          </Pod>
        ) : (
          <Pod>
            <PodHeader>Quota Bonus Formula <span className="text-dim" style={{ fontWeight: 400, fontSize: 11 }}>(reference — select an experiment for real numbers)</span></PodHeader>
            <PodContent>
              <table className="wa-table">
                <tbody>
                  <tr><td>Base share</td><td className="mono text-muted" style={{ fontSize: 11.5 }}>total_budget_AccH ÷ signed_up_agent_count</td></tr>
                  <tr><td>Top-3 bonus <span className="badge badge-running" style={{ marginLeft: 4 }}>+25%</span></td><td className="mono text-muted" style={{ fontSize: 11.5 }}>any top-3 placement in prior experiments</td></tr>
                  <tr><td>Guaranteed quota</td><td className="mono text-muted" style={{ fontSize: 11.5 }}>base_share × (1 + bonus)</td></tr>
                  <tr><td>Burst quota</td><td className="mono text-muted" style={{ fontSize: 11.5 }}>guaranteed × 2.0 — preemptable</td></tr>
                </tbody>
              </table>
            </PodContent>
          </Pod>
        )}
      </div>

      {isLoading && <Loading />}
      {error && <ErrorMessage>Cannot reach quota service.</ErrorMessage>}

      {/* Agent table */}
      <Pod>
        <PodHeader>
          Registered Agents
          {agents && <span className="text-dim" style={{ fontWeight: 400, marginLeft: 8 }}>({total})</span>}
        </PodHeader>
        <PodContent scrollX>
          <table className="wa-table">
            <thead>
              <tr>
                {agentTable.getFlatHeaders().map(h => (
                  <th
                    key={h.id}
                    style={{
                      textAlign: h.id === 'top3_count' ? 'center' : h.id === 'performance_score' ? 'right' : undefined,
                      cursor: 'pointer', userSelect: 'none',
                    }}
                    onClick={h.column.getToggleSortingHandler()}
                  >
                    {String(h.column.columnDef.header)}
                    {h.column.getIsSorted() === 'desc' ? ' ▼' : h.column.getIsSorted() === 'asc' ? ' ▲' : ''}
                  </th>
                ))}
                {experimentFilter && (
                  <>
                    <th style={{ textAlign: 'right' }}>Best {ranking?.metric ?? 'metric'}</th>
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
                  <td colSpan={colSpan} style={{ padding: 0 }}>
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
                    <tr style={{ cursor: 'pointer' }} onClick={() => setExpandedAgent(isExpanded ? null : a.id)}>
                      <td className="mono" style={{ fontWeight: 700 }}>
                        {a.id}
                        <AgentKindBadge kind={a.kind} className="ml-2" />
                      </td>
                      <td style={{ textAlign: 'center' }}>
                        {a.top3_count > 0 ? (
                          <span
                            className="wa-chip"
                            style={{ cursor: 'default', color: semantic.success, borderColor: 'rgba(74, 222, 128, 0.35)', background: 'rgba(74, 222, 128, 0.1)' }}
                          >
                            +25% Top-3
                          </span>
                        ) : <span className="text-dim">—</span>}
                      </td>
                      <td className="mono" style={{ textAlign: 'right' }}>{a.performance_score.toFixed(3)}</td>
                      {experimentFilter && (
                        <>
                          <td className="mono" style={{ textAlign: 'right' }}>
                            {standing?.experiment_id ? (
                              <Link href={`/jobs/${standing.experiment_id}`} className="text-link" onClick={e => e.stopPropagation()}>
                                #{standing.rank} · {standing.best.toFixed(4)}
                              </Link>
                            ) : <span className="text-dim">—</span>}
                            {missing.some(j => j.id === standing?.experiment_id) && <div><NoSummary /></div>}
                          </td>
                          <td className="mono" style={{ fontSize: 11 }}>
                            {jobs
                              ? Object.entries(jobs.by_status).map(([status, n]) => `${n} ${status.toLowerCase()}`).join(' · ')
                              : <span className="text-dim">none</span>}
                          </td>
                          <td className="mono" style={{ fontSize: 11, color: missing.length > 0 ? semantic.warning : undefined }}>
                            {completed - missing.length} of {completed} completed
                          </td>
                        </>
                      )}
                      <td className="text-dim" style={{ fontSize: 12 }}>{isExpanded ? '▲' : '▼'}</td>
                    </tr>
                    {isExpanded && (selectedPE ? [selectedPE] : running) && (
                      <AgentQuotaRows agentId={a.id} experiments={(selectedPE ? [selectedPE] : running)!} colSpan={colSpan} />
                    )}
                  </Fragment>
                )
              })}
            </tbody>
          </table>
          <Pagination page={page} pageSize={PAGE_SIZE} total={total} onPageChange={setPage} />
        </PodContent>
      </Pod>

      {/* Compute Donation Requests */}
      <Pod>
        <PodHeader>
          Compute Donation Requests
          {donationTotal > 0 && <span className="text-link" style={{ fontWeight: 400, marginLeft: 8 }}>({donationTotal} open)</span>}
        </PodHeader>
        <PodContent>
          {donationItems.length === 0 ? (
            <EmptyState>No open donation requests. Agents can request extra compute via POST /donations.</EmptyState>
          ) : (
            <table className="wa-table">
              <thead>
                <tr>
                  <th>Agent</th>
                  <th style={{ textAlign: 'right' }}>Wants (AccH)</th>
                  <th>Reason</th>
                  <th>Status</th>
                  <th>Requested</th>
                </tr>
              </thead>
              <tbody>
                {donationItems.map(d => (
                  <tr key={d.id}>
                    <td className="mono" style={{ fontWeight: 600 }}>{d.agent_name || d.agent_id}</td>
                    <td className="mono" style={{ textAlign: 'right' }}>{d.credits_want}</td>
                    <td className="text-muted" style={{ fontSize: 12, maxWidth: 300 }}>{d.reason}</td>
                    <td><Badge status={d.status}>{d.status}</Badge></td>
                    <td className="text-dim" style={{ fontSize: 11 }}>{new Date(d.created_at).toLocaleString()}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <Pagination page={donationPage} pageSize={PAGE_SIZE} total={donationTotal} onPageChange={setDonationPage} />
        </PodContent>
      </Pod>
    </div>
  )
}
