'use client'

import { Suspense, useState } from 'react'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { Link } from '@/components/ui/link'
import { fetchExperimentsPage, fetchExperimentStats, fetchUnsummarizedJobIDs, cancelExperiment, fetchOrgClusters } from '@/lib/api'
import type { Experiment } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { AgentSelect } from '@/components/agent-select'
import { PlatformExperimentName } from '@/components/platform-experiment-name'
import { PlatformExperimentSelect } from '@/components/platform-experiment-select'
import { Card, CardHeader, CardContent } from '@/components/ui/card'
import { StatTile } from '@/components/ui/stat-tile'
import { Badge, NoSummary } from '@/components/ui/badge'
import { Button, Chip } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { SortableTh, Table } from '@/components/ui/table'
import { Eyebrow } from '@/components/ui/text'
import { Pagination } from '@/components/ui/pagination'
import { acceleratorLabel, formatAccH, relTime } from '@/lib/format'
import { budgetUnit, kindOf } from '@/lib/units'
import { notAdmittedLabel } from '@/lib/eviction'
import { useProjectRouter, useQueryParam } from '@/lib/query-state'
import { useTenant } from '@/lib/tenant'
import { listProject, useExperimentAccess } from '@/lib/read-only'
import { envSettings, variedEnvByHypothesis } from '@/lib/hypothesis-progress'

const CANCELABLE = new Set(['SUBMITTED', 'QUEUED', 'RUNNING'])
const PAGE_SIZE = 25

type SortKey = 'created_at' | 'priority_score' | 'status'

export default function JobsPage() {
  return (
    <Suspense fallback={<Loading />}>
      <JobsPageContent />
    </Suspense>
  )
}

function JobsPageContent() {
  const { org, project } = useTenant()
  const router = useProjectRouter()
  const [agentFilter, setAgentFilter] = useState('')
  const [peFilter, setPEFilter] = useQueryParam('platform_experiment_id')
  // The jobs of a shared platform experiment of another org are asked for by it alone and
  // cannot be cancelled. Which it is is known once the experiment has been read.
  const access = useExperimentAccess(peFilter || undefined)
  const scope = listProject(project, access)
  const scopeKey = scope.project_id ?? 'shared'
  const [statusFilter, setStatusFilter] = useQueryParam('status')
  const [cancelling, setCancelling] = useState<string | null>(null)
  const [confirming, setConfirming] = useState<string | null>(null)
  const [cancelError, setCancelError] = useState('')
  const [page, setPage] = useState(0)
  const [sortKey, setSortKey] = useState<SortKey>('created_at')
  const [sortDir, setSortDir] = useState<'asc' | 'desc'>('desc')

  // Every cluster a job of this org can be on, its own and the ones the platform shares. A job
  // on a cluster no longer listed, such as one that stopped being shared, shows its name alone.
  // Until that read answers, and when it fails, no job shows a connection dot.
  const { data: clusters, error: clustersError } = useSWR(['org-clusters', org.id], () => fetchOrgClusters(org.id), { refreshInterval: 10_000 })
  const connectedByCluster = new Map(clustersError ? [] : clusters?.map(c => [c.name, c.connected]))

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
    access.known ? ['jobs', scopeKey, agentFilter, peFilter, statusFilter, page, sort] : null,
    () => fetchExperimentsPage({
      ...scope,
      agent: agentFilter || undefined,
      platform_experiment_id: peFilter || undefined,
      status: statusFilter || undefined,
      limit: PAGE_SIZE,
      offset: page * PAGE_SIZE,
      sort,
    }),
    { refreshInterval: 8_000, keepPreviousData: true },
  )

  const { data: unsummarized } = useSWR(
    access.known ? ['jobs-unsummarized', scopeKey, agentFilter, peFilter] : null,
    () => fetchUnsummarizedJobIDs({ ...scope, agent: agentFilter || undefined, platform_experiment_id: peFilter || undefined }),
    { refreshInterval: 8_000, keepPreviousData: true },
  )

  async function handleCancel(id: string) {
    setConfirming(null)
    setCancelling(id)
    setCancelError('')
    try {
      await cancelExperiment(id)
      mutate()
    } catch (err: any) {
      setCancelError('Cancel failed: ' + (err?.message ?? String(err)))
    } finally {
      setCancelling(null)
    }
  }

  const visible = data?.items ?? []
  const total = data?.total ?? 0
  const variedByHypothesis = variedEnvByHypothesis(visible)

  // The tiles count the whole filtered set, server-side — the same filters this page's list
  // read uses, minus its paging. Tallying the visible rows instead would report the page's
  // shape under a label that reads as the platform's.
  const { data: stats } = useSWR(
    access.known ? ['job-stats', scopeKey, agentFilter, peFilter, statusFilter] : null,
    () => fetchExperimentStats({
      ...scope,
      agent: agentFilter || undefined,
      platform_experiment_id: peFilter || undefined,
      status: statusFilter || undefined,
    }),
    { refreshInterval: 8_000, keepPreviousData: true },
  )

  const byStatus = stats?.by_status ?? {}
  const running   = byStatus.RUNNING ?? 0
  const queued    = (byStatus.SUBMITTED ?? 0) + (byStatus.QUEUED ?? 0)
  const evicted   = byStatus.EVICTED ?? 0
  const completed = byStatus.COMPLETED ?? 0

  return (
    <div>
      <PageHeader
        title="Jobs"
        description="What agents ran for their experiments, with status and the compute each used."
      />

      {/* KPI strip */}
      <div className="mb-4 grid grid-cols-4 gap-2.5">
        <StatTile label="Running" value={running} tone="success" />
        <StatTile label="Queued / Pending" value={queued} tone="warning" />
        <StatTile label="Completed" value={completed} tone="accent" />
        <StatTile label="Evicted" value={evicted} tone="danger" />
      </div>

      {/* Filters */}
      <div className="mb-4 flex flex-wrap items-center gap-4">
        <div className="flex items-center gap-2">
          <Eyebrow>Status</Eyebrow>
          <div className="flex gap-1.5">
            <Chip active={statusFilter === ''} onClick={() => { setStatusFilter(''); setPage(0) }}>All</Chip>
            <Chip active={statusFilter === 'RUNNING'} onClick={() => { setStatusFilter('RUNNING'); setPage(0) }}>Running</Chip>
            <Chip active={statusFilter === 'QUEUED'} onClick={() => { setStatusFilter('QUEUED'); setPage(0) }}>Queued</Chip>
            <Chip active={statusFilter === 'COMPLETED'} onClick={() => { setStatusFilter('COMPLETED'); setPage(0) }}>Completed</Chip>
            <Chip active={statusFilter === 'EVICTED'} onClick={() => { setStatusFilter('EVICTED'); setPage(0) }}>Evicted</Chip>
            <Chip active={statusFilter === 'FAILED'} onClick={() => { setStatusFilter('FAILED'); setPage(0) }}>Failed</Chip>
          </div>
        </div>

        <div className="flex items-center gap-2">
          <Eyebrow>Agent</Eyebrow>
          <AgentSelect
            value={agentFilter}
            onChange={v => { setAgentFilter(v); setPage(0) }}
            platformExperimentID={scope.project_id ? undefined : peFilter}
          />
        </div>

        <div className="flex items-center gap-2">
          <Eyebrow>Platform Experiment</Eyebrow>
          <PlatformExperimentSelect
            value={peFilter}
            onChange={v => { setPEFilter(v); setPage(0) }}
            allLabel="All experiments"
          />
        </div>
      </div>

      {isLoading && <Loading />}
      {error && <ErrorMessage>Cannot reach registry service.</ErrorMessage>}
      {cancelError && <ErrorMessage className="mb-4">{cancelError}</ErrorMessage>}
      {clustersError && (
        <ErrorMessage>
          Could not load the clusters of {org.name}, so no job shows whether its cluster is connected: {clustersError.message}
        </ErrorMessage>
      )}

      <Card>
        <CardHeader>
          Job List
          {total > 0 && <span className="ml-2 text-xs font-normal text-muted">({total})</span>}
        </CardHeader>
        <CardContent scrollX>
          {/* Fixed widths for the short columns; experiment and hypothesis share what is left. An
              experiment filter makes the experiment column the same on every row, so it goes. */}
          <Table className="min-w-[1040px] table-fixed">
            <thead>
              <tr>
                <th className="w-[84px]">Job ID</th>
                <th className="w-[150px]">Agent</th>
                {!peFilter && <th>Experiment</th>}
                <SortableTh className="w-[140px]" sorted={sortKey === 'status' && sortDir} onSort={() => toggleSort('status')}>Status</SortableTh>
                <th className="w-[130px]">Accelerator</th>
                <th className="w-[120px] text-right">Best metric</th>
                <th className="w-[92px] text-right">Est. Cost</th>
                <th title="Under each hypothesis: the settings that differ between this page's jobs of it">Hypothesis · varied settings</th>
                <SortableTh className="w-[104px]" sorted={sortKey === 'created_at' && sortDir} onSort={() => toggleSort('created_at')}>Submitted</SortableTh>
                <th className="w-20"></th>
              </tr>
            </thead>
            <tbody>
              {visible.length === 0 ? (
                <tr>
                  <td colSpan={10}>
                    <EmptyState>
                      No jobs found{statusFilter ? ` for status "${statusFilter}"` : ''}
                      {agentFilter ? ` from agent "${agentFilter}"` : ''}
                      {peFilter && <> in <PlatformExperimentName id={peFilter} /></>}.
                    </EmptyState>
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
                    className="cursor-pointer"
                    onClick={() => router.push(`/jobs/${job.id}`)}
                    onKeyDown={e => { if (e.key === 'Enter') router.push(`/jobs/${job.id}`) }}
                  >
                    <td className="whitespace-nowrap font-mono text-2xs text-subtle">{job.id.slice(0, 8)}…</td>
                    <td className="truncate font-mono text-xs" title={job.agent_id}>{job.agent_id}</td>
                    {!peFilter && (
                      <td className="font-mono text-2xs [overflow-wrap:anywhere]">
                        {j.platform_experiment_id ? (
                          <Link
                            href={`/platform-experiments/${j.platform_experiment_id}`}
                            className="text-accent hover:underline"
                            onClick={e => e.stopPropagation()}
                          >
                            <PlatformExperimentName id={j.platform_experiment_id} />
                          </Link>
                        ) : '—'}
                      </td>
                    )}
                    <td>
                      <Badge status={status}>{status}</Badge>
                      {unsummarized?.has(job.id) && <div className="mt-0.5"><NoSummary /></div>}
                      {status === 'QUEUED' && j.not_admitted_reason && (
                        <div className="mt-0.5 font-mono text-2xs text-muted">
                          {notAdmittedLabel(j.not_admitted_reason)}
                        </div>
                      )}
                      {(j.capacity_tier || j.cluster_name) && (
                        <div className="mt-0.5 whitespace-nowrap font-mono text-2xs text-muted">
                          {j.capacity_tier}
                          {j.capacity_tier && j.cluster_name && ' · '}
                          {j.cluster_name && (
                            <>
                              {connectedByCluster.has(j.cluster_name) && (
                                <span
                                  className={clsx('mr-1 inline-block h-1.5 w-1.5 rounded-full', connectedByCluster.get(j.cluster_name) ? 'bg-success' : 'bg-danger')}
                                  title={connectedByCluster.get(j.cluster_name) ? 'Cluster connected' : 'Cluster disconnected'}
                                />
                              )}
                              {j.cluster_name}
                            </>
                          )}
                        </div>
                      )}
                    </td>
                    <td className="truncate font-mono text-2xs" title={j.accelerator_type}>
                      {j.accelerator_count}× {acceleratorLabel(j.accelerator_type)}
                    </td>
                    <td className="text-right font-mono text-xs">
                      {j.metric ? j.metric.best.toFixed(4) : '—'}
                      {j.metric && <div className="mt-0.5 truncate text-2xs text-muted" title={j.metric.name}>{j.metric.name}</div>}
                    </td>
                    <td className="whitespace-nowrap text-right font-mono text-2xs">
                      {cost != null ? `${cost} ${budgetUnit(kindOf(j.accelerator_type))}` : '—'}
                    </td>
                    <td
                      className={clsx('truncate text-xs', j.hypothesis_id && 'cursor-pointer text-accent')}
                      title={j.hypothesis}
                      onClick={e => {
                        if (!j.hypothesis_id) return
                        e.stopPropagation()
                        router.push(`/hypotheses/${j.hypothesis_id}`)
                      }}
                    >
                      {j.hypothesis}
                      <div className="mt-0.5 whitespace-normal font-mono text-2xs text-muted">
                        {envSettings(j, variedByHypothesis.get(j.hypothesis_id) ?? [])}
                      </div>
                    </td>
                    <td className="whitespace-nowrap font-mono text-2xs text-subtle">
                      {relTime(j.created_at)}
                    </td>
                    <td onClick={e => e.stopPropagation()}>
                      {!access.readOnly && CANCELABLE.has(status) && (
                        <Button
                          variant="danger"
                          size="sm"
                          disabled={cancelling === job.id}
                          onClick={() => setConfirming(job.id)}
                          className="whitespace-nowrap"
                        >
                          {cancelling === job.id ? '…' : 'Cancel'}
                        </Button>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </Table>
          <Pagination page={page} pageSize={PAGE_SIZE} total={total} onPageChange={setPage} />
        </CardContent>
      </Card>

      <ConfirmDialog
        open={confirming !== null}
        title="Cancel this job?"
        confirmLabel="Cancel job"
        cancelLabel="Keep job"
        onConfirm={() => handleCancel(confirming!)}
        onCancel={() => setConfirming(null)}
      >
        The job stops and the compute it holds is released.
      </ConfirmDialog>
    </div>
  )
}
