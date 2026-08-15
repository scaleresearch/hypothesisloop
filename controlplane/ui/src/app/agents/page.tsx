'use client'

import { Fragment, useMemo, useState } from 'react'
import useSWR from 'swr'
import { useReactTable, getCoreRowModel, getSortedRowModel, type SortingState } from '@tanstack/react-table'
import { fetchAgentBalancesPage, fetchPlatformExperiments, fetchPlatformExperimentQuotas, fetchDonations } from '@/lib/api'
import type { AgentBalance, PlatformExperiment, AgentQuota } from '@/types'
import type { DonationRequest } from '@/lib/api'
import { PageHeader } from '@/components/ui/page-header'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { StatTile } from '@/components/ui/stat-tile'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { SearchableSelect } from '@/components/ui/searchable-select'
import { Pagination } from '@/components/ui/pagination'
import { semantic } from '@/lib/colors'
import { formatAccH } from '@/lib/format'

const PAGE_SIZE = 25
// Filtering by experiment cross-references the full signed-up-agents set, which server-side
// pagination can't do without a dedicated endpoint — so a filtered view asks for one big page
// (bounded like every other list read) instead of paging through per-experiment signups.
const MAX_PAGE_SIZE = 200

function BonusChip({ label, active }: { label: string; active: boolean }) {
  return (
    <span
      className="wa-chip"
      style={{
        display: 'inline-flex', alignItems: 'center', gap: 5, cursor: 'default',
        color: active ? semantic.success : 'var(--dim-fg)',
        borderColor: active ? 'rgba(74, 222, 128, 0.35)' : undefined,
        background: active ? 'rgba(74, 222, 128, 0.1)' : undefined,
      }}
    >
      {label}
    </span>
  )
}

function AgentExperimentQuota({ agentId, pe }: { agentId: string; pe: PlatformExperiment }) {
  const { data: quotas } = useSWR<AgentQuota[]>(
    ['pe-quotas', pe.id],
    () => fetchPlatformExperimentQuotas(pe.id),
    { refreshInterval: 30_000 },
  )
  const q = quotas?.find(q => q.agent_id === agentId)
  if (!q) return null
  const gRem = q.guaranteed_accelerator_hours - q.used_guaranteed_acch
  const bRem = q.burst_accelerator_hours - q.used_burst_acch
  const totalRem = Math.max(0, gRem) + Math.max(0, bRem)
  return (
    <tr style={{ background: 'var(--muted)' }}>
      <td style={{ paddingLeft: 28, fontSize: 12 }} className="text-link">{pe.name}</td>
      <td className="mono" style={{ fontSize: 11 }}>
        <Badge status="guaranteed" className="mr-1">G</Badge>
        {formatAccH(q.guaranteed_accelerator_hours)} AccH ({gRem >= 0 ? `${formatAccH(gRem)} left` : 'over'})
      </td>
      <td className="mono" style={{ fontSize: 11 }}>
        <Badge status="burst" className="mr-1">B</Badge>
        {formatAccH(q.burst_accelerator_hours)} AccH ({bRem >= 0 ? `${formatAccH(bRem)} left` : 'over'})
      </td>
      <td className="mono" style={{ fontSize: 11, color: totalRem > 0 ? semantic.success : semantic.danger }}>
        {formatAccH(totalRem)} AccH
      </td>
    </tr>
  )
}

function AgentQuotaRows({ agentId, experiments }: { agentId: string; experiments: PlatformExperiment[] }) {
  const active = experiments.filter(pe => pe.status === 'open' || pe.status === 'running')
  if (active.length === 0) return (
    <tr><td colSpan={4} className="text-dim" style={{ fontSize: 12, paddingLeft: 20 }}>No active platform experiments</td></tr>
  )
  return <>{active.map(pe => <AgentExperimentQuota key={pe.id} agentId={agentId} pe={pe} />)}</>
}

export default function AgentsPage() {
  const [expandedAgent, setExpandedAgent] = useState<string | null>(null)
  const [experimentFilter, setExperimentFilter] = useState('')
  const [page, setPage] = useState(0)

  const { data, error, isLoading, mutate } = useSWR(
    ['agent-balances', experimentFilter, page],
    () => fetchAgentBalancesPage(
      experimentFilter
        ? { limit: MAX_PAGE_SIZE, offset: 0 }
        : { limit: PAGE_SIZE, offset: page * PAGE_SIZE },
    ),
    { refreshInterval: 15_000, keepPreviousData: true },
  )
  const balances = data?.items
  const total = data?.total ?? 0

  const { data: experiments } = useSWR<PlatformExperiment[]>(
    'platform-experiments-all',
    () => fetchPlatformExperiments(),
    { refreshInterval: 30_000 },
  )

  const { data: donations } = useSWR<DonationRequest[]>(
    'donations-open',
    () => fetchDonations('open'),
    { refreshInterval: 15_000 },
  )

  // Filter agents by experiment (those who signed up)
  const filteredBalances = (() => {
    if (!experimentFilter || !experiments) return balances
    const pe = experiments.find(e => e.id === experimentFilter)
    if (!pe || !pe.signed_up_agents) return balances
    const agentSet = new Set(pe.signed_up_agents)
    return (balances ?? []).filter(b => agentSet.has(b.agent_id))
  })()

  const sortedExperiments = useMemo(
    () => [...(experiments ?? [])].sort((a, b) => b.created_at.localeCompare(a.created_at)),
    [experiments],
  )

  const [sorting, setSorting] = useState<SortingState>([{ id: 'performance_bonus', desc: true }])
  const agentColumns = useMemo(() => [
    { accessorKey: 'agent_id', header: 'Agent ID' },
    { accessorKey: 'top3_count', header: 'Bonus Eligibility' },
    { accessorKey: 'performance_bonus', header: 'Perf Score' },
  ], [])
  const agentTable = useReactTable({
    data: filteredBalances ?? [],
    columns: agentColumns,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
  })
  const sortedBalances = agentTable.getRowModel().rows.map(r => r.original)

  const selectedPE = experimentFilter ? (experiments ?? []).find(pe => pe.id === experimentFilter) ?? null : null
  const baseShare = selectedPE && selectedPE.signup_count > 0 ? selectedPE.budget_accelerator_hours / selectedPE.signup_count : 0

  const top3Count = (balances ?? []).filter(b => ((b as any).top3_count ?? 0) > 0).length
  const avgPerf = balances && balances.length > 0
    ? balances.reduce((s, b) => s + (typeof b.performance_bonus === 'number' ? b.performance_bonus : 0), 0) / balances.length
    : null

  return (
    <div>
      <PageHeader
        title="Research Agents"
        description="Researcher agent bonus eligibility and per-experiment quota allocation"
        actions={<Button size="sm" onClick={() => mutate()}>Refresh</Button>}
      />

      {/* KPI row — Top-3/Avg Perf tally the current page only (see jobs page's own KPI-strip
          precedent) when unfiltered; Registered Agents always reflects the true platform total. */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, minmax(0, 1fr))', gap: 10, marginBottom: 16 }}>
        <StatTile label="Registered Agents" value={total || '—'} />
        <StatTile label="Top-3 Eligible" value={top3Count} color={semantic.success} sub="+25% quota bonus" />
        <StatTile label="Avg Perf Bonus" value={avgPerf != null ? avgPerf.toFixed(3) : '—'} />
        <StatTile label="Open Donation Requests" value={donations?.length ?? 0} color={(donations?.length ?? 0) > 0 ? semantic.warning : undefined} />
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, alignItems: 'start' }}>
        {/* Experiment filter */}
        <Pod>
          <PodHeader>Filter</PodHeader>
          <PodContent style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
            <span className="text-muted">By experiment:</span>
            <SearchableSelect
              value={experimentFilter}
              onChange={v => { setExperimentFilter(v); setPage(0) }}
              allLabel="All agents"
              options={sortedExperiments.map(pe => ({ value: pe.id, label: `${pe.name} (${pe.status})` }))}
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
                  <tr><td>Base share</td><td className="mono" style={{ fontSize: 11.5 }}>{formatAccH(selectedPE.budget_accelerator_hours)} ÷ {selectedPE.signup_count || 0} = {formatAccH(baseShare)} AccH</td></tr>
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
          {filteredBalances && (
            <span className="text-dim" style={{ fontWeight: 400, marginLeft: 8 }}>
              ({experimentFilter ? filteredBalances.length : total})
            </span>
          )}
          {experimentFilter && balances && filteredBalances && filteredBalances.length !== balances.length && (
            <span className="text-link" style={{ fontWeight: 400, fontSize: 11, marginLeft: 6 }}>filtered from {balances.length}</span>
          )}
        </PodHeader>
        {experimentFilter && total > MAX_PAGE_SIZE && (
          <div className="text-dim" style={{ fontSize: 12, padding: '6px 12px', color: semantic.warning }}>
            Showing first {MAX_PAGE_SIZE} of {total} agents system-wide before filtering — some
            agents signed up for this experiment may be missing from this list. Narrow the filter
            or check the platform experiment&apos;s quota list for the full signup roster.
          </div>
        )}
        <PodContent scrollX>
          <table className="wa-table">
            <thead>
              <tr>
                {agentTable.getFlatHeaders().map(h => (
                  <th
                    key={h.id}
                    style={{
                      textAlign: h.id === 'top3_count' ? 'center' : h.id === 'performance_bonus' ? 'right' : undefined,
                      cursor: 'pointer', userSelect: 'none',
                    }}
                    onClick={h.column.getToggleSortingHandler()}
                  >
                    {String(h.column.columnDef.header)}
                    {h.column.getIsSorted() === 'desc' ? ' ▼' : h.column.getIsSorted() === 'asc' ? ' ▲' : ''}
                  </th>
                ))}
                <th></th>
              </tr>
            </thead>
            <tbody>
              {!filteredBalances || filteredBalances.length === 0 ? (
                <tr>
                  <td colSpan={4} style={{ padding: 0 }}>
                    <EmptyState>
                      {experimentFilter ? 'No agents signed up for this experiment.' : 'No agents registered.'}
                    </EmptyState>
                  </td>
                </tr>
              ) : sortedBalances.map(b => {
                const top3 = (b as any).top3_count ?? 0
                const hasTop3 = top3 > 0
                const isExpanded = expandedAgent === b.agent_id
                return (
                  <Fragment key={b.agent_id}>
                    <tr style={{ cursor: 'pointer' }} onClick={() => setExpandedAgent(isExpanded ? null : b.agent_id)}>
                      <td className="mono" style={{ fontWeight: 700 }}>{b.agent_id}</td>
                      <td style={{ textAlign: 'center' }}>
                        <BonusChip label="+25% Top-3" active={hasTop3} />
                      </td>
                      <td className="mono" style={{ textAlign: 'right' }}>
                        {typeof b.performance_bonus === 'number' ? b.performance_bonus.toFixed(3) : '—'}
                      </td>
                      <td className="text-dim" style={{ fontSize: 12 }}>{isExpanded ? '▲' : '▼'}</td>
                    </tr>
                    {isExpanded && experiments && (
                      <AgentQuotaRows agentId={b.agent_id} experiments={experiments} />
                    )}
                  </Fragment>
                )
              })}
            </tbody>
          </table>
          {!experimentFilter && <Pagination page={page} pageSize={PAGE_SIZE} total={total} onPageChange={setPage} />}
        </PodContent>
      </Pod>

      {/* Compute Donation Requests */}
      <Pod>
        <PodHeader>
          Compute Donation Requests
          {donations && donations.length > 0 && <span className="text-link" style={{ fontWeight: 400, marginLeft: 8 }}>({donations.length} open)</span>}
        </PodHeader>
        <PodContent>
          {!donations || donations.length === 0 ? (
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
                {donations.map(d => (
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
        </PodContent>
      </Pod>
    </div>
  )
}
