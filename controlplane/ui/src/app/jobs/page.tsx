'use client'

import { Suspense, useMemo, useState } from 'react'
import useSWR from 'swr'
import Link from 'next/link'
import { useRouter } from 'next/navigation'
import { fetchExperimentsPage, fetchExperimentStats, cancelExperiment, fetchClusters } from '@/lib/api'
import type { Experiment, ClustersResponse } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { AgentSelect } from '@/components/agent-select'
import { PlatformExperimentName } from '@/components/platform-experiment-name'
import { PlatformExperimentSelect } from '@/components/platform-experiment-select'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { StatTile } from '@/components/ui/stat-tile'
import { Badge } from '@/components/ui/badge'
import { Button, Chip } from '@/components/ui/button'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { Pagination } from '@/components/ui/pagination'
import { semantic } from '@/lib/colors'
import { acceleratorLabel, formatAccH, relTime } from '@/lib/format'
import { notAdmittedLabel } from '@/lib/eviction'
import { useQueryParam } from '@/lib/query-state'

const CANCELABLE = new Set(['SUBMITTED', 'QUEUED', 'ADMITTED', 'RUNNING'])
const PAGE_SIZE = 25
const ELLIPSIS: React.CSSProperties = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }

type SortKey = 'created_at' | 'priority_score' | 'status'

function SortHeader({ label, sortKey, active, dir, onClick, width }: {
  label: string
  sortKey: SortKey
  active: boolean
  dir: 'asc' | 'desc'
  onClick: (k: SortKey) => void
  width: number
}) {
  return (
    <th style={{ width, cursor: 'pointer', userSelect: 'none' }} onClick={() => onClick(sortKey)}>
      {label} {active ? (dir === 'desc' ? '▼' : '▲') : ''}
    </th>
  )
}

export default function JobsPage() {
  return (
    <Suspense fallback={<Loading />}>
      <JobsPageContent />
    </Suspense>
  )
}

function JobsPageContent() {
  const router = useRouter()
  const [agentFilter, setAgentFilter] = useState('')
  const [peFilter, setPEFilter] = useQueryParam('platform_experiment_id')
  const [statusFilter, setStatusFilter] = useQueryParam('status')
  const [cancelling, setCancelling] = useState<string | null>(null)
  const [page, setPage] = useState(0)
  const [sortKey, setSortKey] = useState<SortKey>('created_at')
  const [sortDir, setSortDir] = useState<'asc' | 'desc'>('desc')

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

  // The tiles count the whole filtered set, server-side — the same filters this page's list
  // read uses, minus its paging. Tallying the visible rows instead would report the page's
  // shape under a label that reads as the platform's.
  const { data: stats } = useSWR(
    ['job-stats', agentFilter, peFilter, statusFilter],
    () => fetchExperimentStats({
      agent: agentFilter || undefined,
      platform_experiment_id: peFilter || undefined,
      status: statusFilter || undefined,
    }),
    { refreshInterval: 8_000, keepPreviousData: true },
  )
  const byStatus = stats?.by_status ?? {}
  const running   = byStatus.RUNNING ?? 0
  const queued    = (byStatus.SUBMITTED ?? 0) + (byStatus.QUEUED ?? 0) + (byStatus.ADMITTED ?? 0)
  const evicted   = byStatus.EVICTED ?? 0
  const completed = byStatus.COMPLETED ?? 0

  return (
    <div>
      <PageHeader
        title="Jobs"
        description="Agent-submitted workload runs within Platform Experiments — status, capacity tier, accelerator cost in AccH"
      />

      {/* KPI strip */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 10, marginBottom: 16 }}>
        <StatTile label="Running" value={running} color={semantic.success} />
        <StatTile label="Queued / Pending" value={queued} color={semantic.warning} />
        <StatTile label="Completed" value={completed} color={semantic.accent} />
        <StatTile label="Evicted" value={evicted} color={semantic.danger} />
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
          <AgentSelect value={agentFilter} onChange={v => { setAgentFilter(v); setPage(0) }} />
        </div>

        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <span className="uppercase-label">Platform Experiment</span>
          <PlatformExperimentSelect
            value={peFilter}
            onChange={v => { setPEFilter(v); setPage(0) }}
            allLabel="All experiments"
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
          {/* Fixed widths for the short columns; experiment and hypothesis share what is left. */}
          <table className="wa-table" style={{ tableLayout: 'fixed', minWidth: 940 }}>
            <thead>
              <tr>
                <th style={{ width: 84 }}>Job ID</th>
                <th style={{ width: 150 }}>Agent</th>
                <th>Experiment</th>
                <SortHeader label="Status" sortKey="status" active={sortKey === 'status'} dir={sortDir} onClick={toggleSort} width={140} />
                <th style={{ width: 130 }}>Accelerator</th>
                <th style={{ width: 92, textAlign: 'right' }}>Est. Cost</th>
                <th>Hypothesis</th>
                <SortHeader label="Submitted" sortKey="created_at" active={sortKey === 'created_at'} dir={sortDir} onClick={toggleSort} width={104} />
                <th style={{ width: 80 }}></th>
              </tr>
            </thead>
            <tbody>
              {visible.length === 0 ? (
                <tr>
                  <td colSpan={9}>
                    <div className="empty-state">
                      No jobs found{statusFilter ? ` for status "${statusFilter}"` : ''}
                      {agentFilter ? ` from agent "${agentFilter}"` : ''}
                      {peFilter && <> in <PlatformExperimentName id={peFilter} /></>}.
                    </div>
                  </td>
                </tr>
              ) : visible.map((job: Experiment) => {
                const j = job
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
                    <td className="mono text-dim" style={{ fontSize: 11, whiteSpace: 'nowrap' }}>{job.id.slice(0, 8)}…</td>
                    <td className="mono" style={ELLIPSIS} title={job.agent_id}>{job.agent_id}</td>
                    <td className="mono" style={{ fontSize: 11, overflowWrap: 'anywhere' }}>
                      {j.platform_experiment_id ? (
                        <Link
                          href={`/platform-experiments/${j.platform_experiment_id}`}
                          className="text-link"
                          onClick={e => e.stopPropagation()}
                        >
                          <PlatformExperimentName id={j.platform_experiment_id} />
                        </Link>
                      ) : '—'}
                    </td>
                    <td>
                      <Badge status={status}>{status}</Badge>
                      {status === 'QUEUED' && j.not_admitted_reason && (
                        <div className="text-muted mono" style={{ fontSize: 10, marginTop: 3 }}>
                          {notAdmittedLabel(j.not_admitted_reason)}
                        </div>
                      )}
                      {(j.capacity_tier || j.cluster_name) && (
                        <div className="text-muted mono" style={{ fontSize: 10, marginTop: 3, whiteSpace: 'nowrap' }}>
                          {j.capacity_tier}
                          {j.capacity_tier && j.cluster_name && ' · '}
                          {j.cluster_name && (
                            <>
                              <span style={{
                                width: 6, height: 6, borderRadius: '50%', display: 'inline-block', marginRight: 4,
                                background: connectedByCluster.get(j.cluster_name) ? semantic.success : semantic.danger,
                              }} title={connectedByCluster.get(j.cluster_name) ? 'Cluster connected' : 'Cluster disconnected'} />
                              {j.cluster_name}
                            </>
                          )}
                        </div>
                      )}
                    </td>
                    <td className="mono" style={{ ...ELLIPSIS, fontSize: 11 }} title={j.accelerator_type}>
                      {j.accelerator_count}× {acceleratorLabel(j.accelerator_type)}
                    </td>
                    <td className="mono" style={{ textAlign: 'right', fontSize: 11, whiteSpace: 'nowrap' }}>
                      {cost != null ? `${cost} AccH` : '—'}
                    </td>
                    <td
                      className={j.hypothesis_id ? 'accent' : undefined}
                      style={{ ...ELLIPSIS, fontSize: 12, cursor: j.hypothesis_id ? 'pointer' : undefined }}
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
