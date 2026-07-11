'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { useRouter } from 'next/navigation'
import { fetchExperiments, fetchAgents, cancelExperiment, fetchClusters } from '@/lib/api'
import type { Experiment, Agent, ClustersResponse } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { StatTile } from '@/components/ui/stat-tile'
import { Badge, TierBadge } from '@/components/ui/badge'
import { Button, Chip } from '@/components/ui/button'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { semantic } from '@/lib/colors'
import { formatT4h } from '@/lib/format'

function relTime(iso: string) {
  const diffMs = Date.now() - new Date(iso).getTime()
  const s = Math.round(diffMs / 1000)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.round(s / 60)}m ago`
  if (s < 86400) return `${Math.round(s / 3600)}h ago`
  return new Date(iso).toLocaleDateString()
}

const CANCELABLE = new Set(['SUBMITTED', 'QUEUED', 'ADMITTED', 'RUNNING'])

export default function JobsPage() {
  const router = useRouter()
  const [agentFilter, setAgentFilter] = useState('')
  const [statusFilter, setStatusFilter] = useState('')
  const [cancelling, setCancelling] = useState<string | null>(null)

  const { data: agents } = useSWR<Agent[]>('agents', fetchAgents)
  const { data: clusters } = useSWR<ClustersResponse>('clusters', fetchClusters, { refreshInterval: 10_000 })
  const connectedByCluster = new Map((clusters?.clusters ?? []).map(c => [c.cluster_name, c.connected]))

  const { data: jobs, error, isLoading, mutate } = useSWR<Experiment[]>(
    ['jobs', agentFilter],
    () => fetchExperiments({ agent_id: agentFilter || undefined, limit: 200 }),
    { refreshInterval: 8_000 },
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

  const list = jobs ?? []

  const running   = list.filter(j => j.status === 'RUNNING' as any).length
  const queued    = list.filter(j => ['SUBMITTED', 'QUEUED', 'ADMITTED'].includes(j.status as any)).length
  const evicted   = list.filter(j => j.status === 'EVICTED' as any).length
  const completed = list.filter(j => j.status === 'COMPLETED' as any).length

  const visible = statusFilter
    ? list.filter(j => (statusFilter === 'QUEUED' ? ['SUBMITTED', 'QUEUED', 'ADMITTED'] : [statusFilter]).includes(j.status as any))
    : list

  return (
    <div>
      <PageHeader
        title="Jobs"
        description="Agent-submitted workload runs within Platform Experiments — status, capacity tier, GPU cost in T4h"
        actions={<Button size="sm" onClick={() => mutate()}>Refresh</Button>}
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
            <Chip active={statusFilter === ''} onClick={() => setStatusFilter('')}>All</Chip>
            <Chip active={statusFilter === 'RUNNING'} onClick={() => setStatusFilter('RUNNING')}>Running</Chip>
            <Chip active={statusFilter === 'QUEUED'} onClick={() => setStatusFilter('QUEUED')}>Queued</Chip>
            <Chip active={statusFilter === 'COMPLETED'} onClick={() => setStatusFilter('COMPLETED')}>Completed</Chip>
            <Chip active={statusFilter === 'EVICTED'} onClick={() => setStatusFilter('EVICTED')}>Evicted</Chip>
            <Chip active={statusFilter === 'FAILED'} onClick={() => setStatusFilter('FAILED')}>Failed</Chip>
          </div>
        </div>

        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <span className="uppercase-label">Agent</span>
          <select
            value={agentFilter}
            onChange={e => setAgentFilter(e.target.value)}
            className="mono"
            style={{
              fontSize: 12,
              padding: '5px 10px',
              border: '1px solid rgba(255,255,255,.16)',
              borderRadius: 6,
              background: 'var(--surface-2)',
              color: 'var(--foreground)',
            }}
          >
            <option value="">All agents</option>
            {(agents ?? []).map(a => (
              <option key={a.id} value={a.id}>{a.name ?? a.id}</option>
            ))}
          </select>
          {agentFilter && (
            <Button variant="link" onClick={() => setAgentFilter('')}>Clear</Button>
          )}
        </div>
      </div>

      {isLoading && <Loading />}
      {error && <ErrorMessage>Cannot reach registry service.</ErrorMessage>}

      <Pod>
        <PodHeader>
          Job List
          {visible.length > 0 && <span className="text-muted" style={{ fontWeight: 400, marginLeft: 8 }}>({visible.length})</span>}
        </PodHeader>
        <PodContent scrollX>
          <table className="wa-table">
            <thead>
              <tr>
                <th>Job ID</th>
                <th>Agent</th>
                <th>Experiment</th>
                <th>Status</th>
                <th>Cluster</th>
                <th>Tier</th>
                <th>GPU</th>
                <th style={{ textAlign: 'right' }}>Est. Cost</th>
                <th style={{ textAlign: 'right' }}>Final Metric</th>
                <th>Hypothesis</th>
                <th>Submitted</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {visible.length === 0 ? (
                <tr>
                  <td colSpan={12}>
                    <div className="empty-state">No jobs found{statusFilter ? ` for status "${statusFilter}"` : ''}.</div>
                  </td>
                </tr>
              ) : visible.map((job: Experiment) => {
                const j = job as any
                const status = j.status ?? 'UNKNOWN'
                const cost = j.estimated_cost_t4h != null ? formatT4h(j.estimated_cost_t4h) : null
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
                    <td className="mono text-dim" style={{ fontSize: 11 }}>
                      {j.platform_experiment_id ? j.platform_experiment_id.slice(0, 12) + '…' : '—'}
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
                    <td className="mono" style={{ fontSize: 11 }}>{j.gpu_count}× {j.gpu_type}</td>
                    <td className="mono" style={{ textAlign: 'right', fontSize: 11 }}>
                      {cost != null ? `${cost} T4h` : '—'}
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
        </PodContent>
      </Pod>
    </div>
  )
}
