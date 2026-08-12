'use client'

import { useMemo, useState } from 'react'
import useSWR from 'swr'
import Link from 'next/link'
import { useRouter } from 'next/navigation'
import { fetchExperimentsPage, fetchAgents, cancelExperiment, fetchClusters, fetchPlatformExperiments } from '@/lib/api'
import type { Experiment, Agent, ClustersResponse, PlatformExperiment } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { StatTile } from '@/components/ui/stat-tile'
import { Badge, TierBadge } from '@/components/ui/badge'
import { Button, Chip } from '@/components/ui/button'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { Pagination } from '@/components/ui/pagination'
import { SearchableSelect } from '@/components/ui/searchable-select'
import { semantic } from '@/lib/colors'
import { formatAccH } from '@/lib/format'

function relTime(iso: string) {
  const diffMs = Date.now() - new Date(iso).getTime()
  const s = Math.round(diffMs / 1000)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.round(s / 60)}m ago`
  if (s < 86400) return `${Math.round(s / 3600)}h ago`
  return new Date(iso).toLocaleDateString()
}

const CANCELABLE = new Set(['SUBMITTED', 'QUEUED', 'ADMITTED', 'RUNNING'])
const PAGE_SIZE = 25

type SortKey = 'created_at' | 'priority_score' | 'status'

function SortHeader({ label, sortKey, active, dir, onClick, align }: {
  label: string
  sortKey: SortKey
  active: boolean
  dir: 'asc' | 'desc'
  onClick: (k: SortKey) => void
  align?: 'right'
}) {
  return (
    <th style={{ textAlign: align, cursor: 'pointer', userSelect: 'none' }} onClick={() => onClick(sortKey)}>
      {label} {active ? (dir === 'desc' ? '▼' : '▲') : ''}
    </th>
  )
}

export default function JobsPage() {
  const router = useRouter()
  const [agentFilter, setAgentFilter] = useState('')
  const [peFilter, setPEFilter] = useState('')
  const [statusFilter, setStatusFilter] = useState('')
  const [cancelling, setCancelling] = useState<string | null>(null)
  const [page, setPage] = useState(0)
  const [sortKey, setSortKey] = useState<SortKey>('created_at')
  const [sortDir, setSortDir] = useState<'asc' | 'desc'>('desc')

  const { data: agents } = useSWR<Agent[]>('agents', fetchAgents)
  const sortedAgents = useMemo(
    () => [...(agents ?? [])].sort((a, b) => b.created_at.localeCompare(a.created_at)),
    [agents],
  )
  const { data: platformExperiments } = useSWR<PlatformExperiment[]>('platform-experiments-all', () => fetchPlatformExperiments())
  const sortedPEs = useMemo(
    () => [...(platformExperiments ?? [])].sort((a, b) => b.created_at.localeCompare(a.created_at)),
    [platformExperiments],
  )
  const peByID = new Map((platformExperiments ?? []).map(pe => [pe.id, pe]))
  const { data: clusters } = useSWR<ClustersResponse>('clusters', fetchClusters, { refreshInterval: 10_000 })
  const connectedByCluster = new Map((clusters?.clusters ?? []).map(c => [c.cluster_name, c.connected]))

  function toggleSort(key: SortKey) {
    setPage(0)
    if (key === sortKey) {
      setSortDir(d => (d === 'desc' ? 'asc' : 'desc'))
    } else {
      setSortKey(key)
      setSortDir('desc')
    }
  }

  const sort = `${sortDir === 'desc' ? '-' : ''}${sortKey}`

  const { data, error, isLoading, mutate } = useSWR(
    ['jobs', agentFilter, peFilter, statusFilter, page, sort],
    () => fetchExperimentsPage({
      agent: agentFilter || undefined,
      platform_experiment_id: peFilter || undefined,
      status: statusFilter || undefined,
      limit: PAGE_SIZE,
      offset: page * PAGE_SIZE,
      sort,
    }),
    { refreshInterval: 8_000, keepPreviousData: true },
  )

  async function handleCancel(e: React.MouseEvent, id: string) {
    e.stopPropagation()
    if (!confirm('Cancel this job?')) return
    setCancelling(id)
    try {
      await cancelExperiment(id)
      mutate()
    } catch (err: any) {
      alert('Cancel failed: ' + (err?.message ?? String(err)))
    } finally {
      setCancelling(null)
    }
  }

  const visible = data?.items ?? []
  const total = data?.total ?? 0

  // KPI strip counts the current page only when server-paginated data is all we have loaded —
  // acceptable here since these are "what's on screen" tiles, not claimed platform totals.
  const running   = visible.filter(j => j.status === 'RUNNING' as any).length
  const queued    = visible.filter(j => ['SUBMITTED', 'QUEUED', 'ADMITTED'].includes(j.status as any)).length
  const evicted   = visible.filter(j => j.status === 'EVICTED' as any).length
  const completed = visible.filter(j => j.status === 'COMPLETED' as any).length

  return (
    <div>
      <PageHeader
        title="Jobs"
        description="Agent-submitted workload runs within Platform Experiments — status, capacity tier, accelerator cost in AccH"
        actions={<Button size="sm" onClick={() => mutate()}>Refresh</Button>}
      />

      {/* KPI strip */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 10, marginBottom: 16 }}>
        <StatTile label="Running (page)" value={running} color={semantic.success} />
        <StatTile label="Queued / Pending (page)" value={queued} color={semantic.warning} />
        <StatTile label="Completed (page)" value={completed} color={semantic.accent} />
        <StatTile label="Evicted (page)" value={evicted} color={semantic.danger} />
      </div>

      {/* Filters */}
      <div style={{ marginBottom: 16, display: 'flex', alignItems: 'center', gap: 18, flexWrap: 'wrap' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <span className="uppercase-label">Status</span>
          <div style={{ display: 'flex', gap: 6 }}>
            <Chip active={statusFilter === ''} onClick={() => { setStatusFilter(''); setPage(0) }}>All</Chip>
            <Chip active={statusFilter === 'RUNNING'} onClick={() => { setStatusFilter('RUNNING'); setPage(0) }}>Running</Chip>
            <Chip active={statusFilter === 'QUEUED'} onClick={() => { setStatusFilter('QUEUED'); setPage(0) }}>Queued</Chip>
            <Chip active={statusFilter === 'COMPLETED'} onClick={() => { setStatusFilter('COMPLETED'); setPage(0) }}>Completed</Chip>
            <Chip active={statusFilter === 'EVICTED'} onClick={() => { setStatusFilter('EVICTED'); setPage(0) }}>Evicted</Chip>
            <Chip active={statusFilter === 'FAILED'} onClick={() => { setStatusFilter('FAILED'); setPage(0) }}>Failed</Chip>
          </div>
        </div>

        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <span className="uppercase-label">Agent</span>
          <SearchableSelect
            value={agentFilter}
            onChange={v => { setAgentFilter(v); setPage(0) }}
            allLabel="All agents"
            options={sortedAgents.map(a => ({ value: a.id, label: a.name ?? a.id }))}
          />
        </div>

        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <span className="uppercase-label">Platform Experiment</span>
          <SearchableSelect
            value={peFilter}
            onChange={v => { setPEFilter(v); setPage(0) }}
            allLabel="All experiments"
            options={sortedPEs.map(pe => ({ value: pe.id, label: pe.name }))}
          />
        </div>
      </div>

      {isLoading && <Loading />}
      {error && <ErrorMessage>Cannot reach registry service.</ErrorMessage>}

      <Pod>
        <PodHeader>
          Job List
          {total > 0 && <span className="text-muted" style={{ fontWeight: 400, marginLeft: 8 }}>({total})</span>}
        </PodHeader>
        <PodContent scrollX>
          <table className="wa-table">
            <thead>
              <tr>
                <th>Job ID</th>
                <th>Agent</th>
                <th>Experiment</th>
                <SortHeader label="Status" sortKey="status" active={sortKey === 'status'} dir={sortDir} onClick={toggleSort} />
                <th>Cluster</th>
                <th>Tier</th>
                <th>Accelerator</th>
                <th style={{ textAlign: 'right' }}>Est. Cost</th>
                <th style={{ textAlign: 'right' }}>Final Metric</th>
                <th>Hypothesis</th>
                <SortHeader label="Submitted" sortKey="created_at" active={sortKey === 'created_at'} dir={sortDir} onClick={toggleSort} />
                <th></th>
              </tr>
            </thead>
            <tbody>
              {visible.length === 0 ? (
                <tr>
                  <td colSpan={12}>
                    <div className="empty-state">
                      No jobs found{statusFilter ? ` for status "${statusFilter}"` : ''}
                      {agentFilter ? ` from agent "${agentFilter}"` : ''}
                      {peFilter ? ` in "${peByID.get(peFilter)?.name ?? peFilter}"` : ''}.
                    </div>
                  </td>
                </tr>
              ) : visible.map((job: Experiment) => {
                const j = job as any
                const status = j.status ?? 'UNKNOWN'
                const cost = j.estimated_cost_acch != null ? formatAccH(j.estimated_cost_acch) : null
                return (
                  <tr
                    key={job.id}
                    tabIndex={0}
                    role="link"
                    style={{ cursor: 'pointer' }}
                    onClick={() => router.push(`/jobs/${job.id}`)}
                    onKeyDown={e => { if (e.key === 'Enter') router.push(`/jobs/${job.id}`) }}
                  >
                    <td className="mono text-dim" style={{ fontSize: 11 }}>{job.id.slice(0, 8)}…</td>
                    <td className="mono">{job.agent_id}</td>
                    <td className="mono" style={{ fontSize: 11 }}>
                      {j.platform_experiment_id ? (
                        <Link
                          href={`/platform-experiments/${j.platform_experiment_id}`}
                          className="text-link"
                          onClick={e => e.stopPropagation()}
                        >
                          {peByID.get(j.platform_experiment_id)?.name ?? j.platform_experiment_id.slice(0, 12) + '…'}
                        </Link>
                      ) : '—'}
                    </td>
                    <td>
					  <Badge status={status}>{status}</Badge>
                      {status === 'QUEUED' && j.not_admitted_reason && (
                        <div className="text-muted mono" style={{ fontSize: 10, marginTop: 3 }}>
                          {j.not_admitted_reason}
                        </div>
                      )}
                    </td>
                    <td className="mono" style={{ fontSize: 11 }}>
                      {j.cluster_name ? (
                        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                          <span style={{
                            width: 7, height: 7, borderRadius: '50%', display: 'inline-block',
                            background: connectedByCluster.get(j.cluster_name) ? semantic.success : semantic.danger,
                            boxShadow: connectedByCluster.get(j.cluster_name)
                              ? '0 0 6px rgba(74, 222, 128, 0.6)' : '0 0 6px rgba(242, 89, 107, 0.5)',
                          }} title={connectedByCluster.get(j.cluster_name) ? 'Connected' : 'Disconnected'} />
                          {j.cluster_name}
                        </span>
                      ) : '—'}
                    </td>
                    <td><TierBadge tier={j.capacity_tier} /></td>
                    <td className="mono" style={{ fontSize: 11 }}>{j.accelerator_count}× {j.accelerator_type}</td>
                    <td className="mono" style={{ textAlign: 'right', fontSize: 11 }}>
                      {cost != null ? `${cost} AccH` : '—'}
                    </td>
                    <td className="mono" style={{ textAlign: 'right' }}>
                      <span className={j.final_metric != null ? 'accent' : 'text-muted'}>
                        {j.final_metric != null ? j.final_metric.toFixed(4) : '—'}
                      </span>
                    </td>
                    <td
                      className={j.hypothesis_id ? 'accent' : undefined}
                      style={{ maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', fontSize: 12, cursor: j.hypothesis_id ? 'pointer' : undefined }}
                      title={j.hypothesis}
                      onClick={e => {
                        if (!j.hypothesis_id) return
                        e.stopPropagation()
                        router.push(`/hypotheses/${j.hypothesis_id}`)
                      }}
                    >
                      {j.hypothesis}
                    </td>
                    <td className="mono text-dim" style={{ fontSize: 11, whiteSpace: 'nowrap' }}>
                      {relTime(j.created_at)}
                    </td>
                    <td onClick={e => e.stopPropagation()}>
                      {CANCELABLE.has(status) && (
                        <Button
                          variant="danger"
                          size="sm"
                          disabled={cancelling === job.id}
                          onClick={e => handleCancel(e, job.id)}
                          style={{ whiteSpace: 'nowrap' }}
                        >
                          {cancelling === job.id ? '…' : 'Cancel'}
                        </Button>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
          <Pagination page={page} pageSize={PAGE_SIZE} total={total} onPageChange={setPage} />
        </PodContent>
      </Pod>
    </div>
  )
}
