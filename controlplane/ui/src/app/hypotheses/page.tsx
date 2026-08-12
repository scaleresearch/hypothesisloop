'use client'

import Link from 'next/link'
import { Suspense } from 'react'
import { useRouter, useSearchParams } from 'next/navigation'
import useSWR from 'swr'
import { useMemo, useState } from 'react'
import { useReactTable, getCoreRowModel, getSortedRowModel, type SortingState } from '@tanstack/react-table'
import { fetchAllHypotheses, fetchPlatformExperiments, fetchAgents } from '@/lib/api'
import type { Hypothesis, HypothesisStatus, PlatformExperiment, Agent } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { SearchableSelect } from '@/components/ui/searchable-select'

const STATUS_FILTERS: Array<{ value: HypothesisStatus | ''; label: string }> = [
  { value: '', label: 'All' },
  { value: 'open', label: 'Open' },
  { value: 'confirmed', label: 'Confirmed' },
  { value: 'inconclusive', label: 'Inconclusive' },
]

function relTime(iso: string) {
  const diffMs = Date.now() - new Date(iso).getTime()
  const s = Math.round(diffMs / 1000)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.round(s / 60)}m ago`
  if (s < 86400) return `${Math.round(s / 3600)}h ago`
  return new Date(iso).toLocaleDateString()
}

export default function HypothesesPage() {
  return (
    <Suspense fallback={<Loading />}>
      <HypothesesPageContent />
    </Suspense>
  )
}

function HypothesesPageContent() {
  const router = useRouter()
  const searchParams = useSearchParams()
  const peID = searchParams.get('pe') ?? ''
  const [agentFilter, setAgentFilter] = useState('')
  const [statusFilter, setStatusFilter] = useState<HypothesisStatus | ''>('')

  const { data: agents } = useSWR<Agent[]>('agents', fetchAgents)

  // Every hypothesis belongs to exactly one platform experiment's shared idea pool — there is
  // no unscoped registry on the backend. To show a combined view here we fetch every platform
  // experiment, then that hypothesis pool per platform experiment, and merge — the "Platform
  // Experiment" column plus the filter below is what makes that structure visible instead of
  // flattening it away.
  const { data: platformExperiments } = useSWR<PlatformExperiment[]>(
    'platform-experiments-all',
    () => fetchPlatformExperiments(),
    { refreshInterval: 30_000 },
  )
  const peByID = new Map((platformExperiments ?? []).map(pe => [pe.id, pe]))
  const peIDs = (platformExperiments ?? []).map(pe => pe.id)

  const { data: hypotheses, error, isLoading, mutate } = useSWR<Hypothesis[]>(
    peIDs.length > 0 ? ['hypotheses-all', peIDs.join(',')] : null,
    () => fetchAllHypotheses(peIDs),
    { refreshInterval: 15_000 },
  )

  const filtered = (() => {
    let all = hypotheses ?? []
    if (peID) all = all.filter(h => h.platform_experiment_id === peID)
    if (agentFilter) all = all.filter(h => h.agent_id === agentFilter)
    if (statusFilter) all = all.filter(h => (h.status ?? 'open') === statusFilter)
    return all
  })()

  function setPEFilter(next: string) {
    if (next) router.push(`/hypotheses?pe=${next}`)
    else router.push('/hypotheses')
  }

  const sortedPEs = useMemo(
    () => [...(platformExperiments ?? [])].sort((a, b) => b.created_at.localeCompare(a.created_at)),
    [platformExperiments],
  )
  const sortedAgentOptions = useMemo(
    () => [...(agents ?? [])].sort((a, b) => b.created_at.localeCompare(a.created_at)),
    [agents],
  )

  const [sorting, setSorting] = useState<SortingState>([{ id: 'created_at', desc: true }])
  const hypothesisColumns = useMemo(() => [
    { accessorKey: 'text', header: 'Text' },
    { accessorKey: 'status', header: 'Status' },
    { accessorKey: 'agent_id', header: 'Registered by' },
    { accessorKey: 'created_at', header: 'Created' },
  ], [])
  const hypothesisTable = useReactTable({
    data: filtered,
    columns: hypothesisColumns,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
  })
  const list = hypothesisTable.getRowModel().rows.map(r => r.original)

  return (
    <div>
      <PageHeader
        title="Hypotheses"
        description="Registered research claims, scoped per platform experiment — every platform experiment accumulates its own shared idea pool, and every job must reference a hypothesis registered under the same one it's submitted into. Registering text equivalent to an existing hypothesis within the same platform experiment returns the existing row instead of creating a duplicate."
        actions={<Button size="sm" onClick={() => mutate()}>Refresh</Button>}
      />

      <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 12, flexWrap: 'wrap' }}>
        <span className="uppercase-label">Platform Experiment</span>
        <SearchableSelect
          value={peID}
          onChange={setPEFilter}
          allLabel={`All (${(hypotheses ?? []).length})`}
          options={sortedPEs.map(pe => ({ value: pe.id, label: pe.name }))}
        />
        {peID && <Link href="/hypotheses" className="text-link" style={{ fontSize: 12 }}>Clear filter</Link>}

        <span className="uppercase-label" style={{ marginLeft: 8 }}>Agent</span>
        <SearchableSelect
          value={agentFilter}
          onChange={setAgentFilter}
          allLabel="All agents"
          options={sortedAgentOptions.map(a => ({ value: a.id, label: a.name ?? a.id }))}
        />
        {agentFilter && (
          <Button variant="link" onClick={() => setAgentFilter('')}>Clear</Button>
        )}

        <span className="uppercase-label" style={{ marginLeft: 8 }}>Status</span>
        <SearchableSelect
          value={statusFilter}
          onChange={v => setStatusFilter(v as HypothesisStatus | '')}
          allLabel="All"
          options={STATUS_FILTERS.filter(f => f.value !== '').map(f => ({ value: f.value, label: f.label }))}
        />
      </div>

      {isLoading && <Loading />}
      {error && <ErrorMessage>Cannot reach registry service.</ErrorMessage>}

      <Pod>
        <PodHeader>
          Hypothesis Registry
          {list.length > 0 && <span className="text-muted" style={{ fontWeight: 400, marginLeft: 8 }}>({list.length})</span>}
        </PodHeader>
        <PodContent scrollX>
          <table className="wa-table">
            <thead>
              <tr>
                <th>ID</th>
                {(['text', 'status'] as const).map(colId => {
                  const h = hypothesisTable.getColumn(colId)!
                  return (
                    <th key={colId} style={{ cursor: 'pointer', userSelect: 'none' }} onClick={h.getToggleSortingHandler()}>
                      {String(h.columnDef.header)}
                      {h.getIsSorted() === 'desc' ? ' ▼' : h.getIsSorted() === 'asc' ? ' ▲' : ''}
                    </th>
                  )
                })}
                <th>Platform Experiment</th>
                {(['agent_id', 'created_at'] as const).map(colId => {
                  const h = hypothesisTable.getColumn(colId)!
                  return (
                    <th key={colId} style={{ cursor: 'pointer', userSelect: 'none' }} onClick={h.getToggleSortingHandler()}>
                      {String(h.columnDef.header)}
                      {h.getIsSorted() === 'desc' ? ' ▼' : h.getIsSorted() === 'asc' ? ' ▲' : ''}
                    </th>
                  )
                })}
              </tr>
            </thead>
            <tbody>
              {list.length === 0 ? (
                <tr>
                  <td colSpan={6}>
                    <div className="empty-state">
                      No hypotheses found
                      {peID ? ` in "${peByID.get(peID)?.name ?? peID}"` : ''}
                      {agentFilter ? ` from agent "${agentFilter}"` : ''}.
                    </div>
                  </td>
                </tr>
              ) : list.map(h => {
                const pe = peByID.get(h.platform_experiment_id)
                return (
                  <tr
                    key={h.id}
                    tabIndex={0}
                    role="link"
                    style={{ cursor: 'pointer' }}
                    onClick={() => router.push(`/hypotheses/${h.id}`)}
                    onKeyDown={e => { if (e.key === 'Enter') router.push(`/hypotheses/${h.id}`) }}
                  >
                    <td className="mono text-dim" style={{ fontSize: 11 }}>{h.id.slice(0, 8)}…</td>
                    <td style={{ maxWidth: 420, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={h.text}>
                      {h.text}
                    </td>
                    <td><Badge status={h.status ?? 'open'}>{h.status ?? 'open'}</Badge></td>
                    <td style={{ fontSize: 12 }}>
                      <Link
                        href={`/platform-experiments/${h.platform_experiment_id}`}
                        className="text-link"
                        onClick={e => e.stopPropagation()}
                      >
                        {pe?.name ?? h.platform_experiment_id.slice(0, 8) + '…'}
                      </Link>
                    </td>
                    <td className="mono">{h.agent_id}</td>
                    <td className="mono text-dim" style={{ fontSize: 11, whiteSpace: 'nowrap' }}>{relTime(h.created_at)}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </PodContent>
      </Pod>
    </div>
  )
}
