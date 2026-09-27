'use client'

import { Suspense, useEffect, useState } from 'react'
import { useSearchParams } from 'next/navigation'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { TextLink } from '@/components/ui/link'
import { fetchExperimentsPage, fetchExperimentStats, cancelExperiment, fetchOrgClusters } from '@/lib/api'
import type { Experiment } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { AgentSelect } from '@/components/agent-select'
import { PlatformExperimentName } from '@/components/platform-experiment-name'
import { PlatformExperimentSelect } from '@/components/platform-experiment-select'
import { Card, CardHeader, CardContent, CardTitle } from '@/components/ui/card'
import { StatTile } from '@/components/ui/stat-tile'
import { Badge, NoSummary } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { FormField } from '@/components/ui/form-field'
import { LinkTr, SortableTh, Table, num, quiet } from '@/components/ui/table'
import { Pagination } from '@/components/ui/pagination'
import { acceleratorLabel, formatAccH, relTime } from '@/lib/format'
import { budgetUnit, kindOf } from '@/lib/units'
import { statusTone } from '@/lib/colors'
import { evictionCodeLabel, evictionLabel, faultBreakdown, notAdmittedLabel } from '@/lib/eviction'
import { useQueryParam } from '@/lib/query-state'
import { useTenant } from '@/lib/tenant'
import { listProject, useExperimentAccess } from '@/lib/read-only'
import { envSettings, variedEnvByHypothesis } from '@/lib/hypothesis-progress'

const CANCELABLE = new Set(['SUBMITTED', 'QUEUED', 'RUNNING'])
const PAGE_SIZE = 25

type SortKey = 'created_at' | 'status'

// Every status a job can hold, in the order a job moves through them.
const STATUSES = [
  { status: 'QUEUED', label: 'Queued' },
  { status: 'SUBMITTED', label: 'Submitted' },
  { status: 'RUNNING', label: 'Running' },
  { status: 'COMPLETED', label: 'Completed' },
  { status: 'FAILED', label: 'Failed' },
  { status: 'EVICTED', label: 'Evicted' },
  { status: 'REJECTED', label: 'Rejected' },
  { status: 'CANCELLED', label: 'Cancelled' },
]

export default function JobsPage() {
  return (
    <Suspense fallback={<Loading />}>
      <JobsPageContent />
    </Suspense>
  )
}

function JobsPageContent() {
  const { org, project } = useTenant()
  const searchParams = useSearchParams()
  const [agentFilter, setAgentFilter] = useState('')
  const [peFilter, setPEFilter] = useQueryParam('platform_experiment_id')
  // The jobs of a shared platform experiment of another org are asked for by it alone and
  // cannot be cancelled. Which it is is known once the experiment has been read.
  const access = useExperimentAccess(peFilter || undefined)
  const scope = listProject(project, access)
  const scopeKey = scope.project_id ?? 'shared'
  const statusFilter = searchParams.get('status') ?? ''
  const [cancelling, setCancelling] = useState<string | null>(null)
  const [confirming, setConfirming] = useState<string | null>(null)
  const [cancelError, setCancelError] = useState('')
  const [page, setPage] = useState(0)
  const [sortKey, setSortKey] = useState<SortKey>('created_at')
  const [sortDir, setSortDir] = useState<'asc' | 'desc'>('desc')

  useEffect(() => setPage(0), [statusFilter])

  // Where a status tile leads: this list with the other filters kept.
  function statusHref(status: string) {
    const params = new URLSearchParams(searchParams.toString())
    if (status) params.set('status', status)
    else params.delete('status')
    const query = params.toString()
    return query ? `/jobs?${query}` : '/jobs'
  }

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
      agent_id: agentFilter || undefined,
      platform_experiment_id: peFilter || undefined,
      status: statusFilter || undefined,
      limit: PAGE_SIZE,
      offset: page * PAGE_SIZE,
      sort,
    }),
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

  // The tiles count every job the agent and experiment filters match, whatever its status, so
  // each tile says how many jobs choosing it lists.
  const { data: stats } = useSWR(
    access.known ? ['job-stats', scopeKey, agentFilter, peFilter] : null,
    () => fetchExperimentStats({
      ...scope,
      agent_id: agentFilter || undefined,
      platform_experiment_id: peFilter || undefined,
    }),
    { refreshInterval: 8_000, keepPreviousData: true },
  )

  const byStatus = stats?.by_status ?? {}
  const evictionReasons = Object.entries(stats?.evictions_by_reason ?? {}).sort((a, b) => b[1] - a[1])
  const faultSummary = faultBreakdown(stats?.evictions_by_class ?? {})
  const shownLabel = STATUSES.find(s => s.status === statusFilter)?.label

  return (
    <div>
      <PageHeader
        title="Jobs"
        description="What agents ran for their experiments, with status and the compute each used. Choose a status to list its jobs."
      />

      {/* One tile per status: its count, and the filter that lists those jobs. */}
      <div className="mb-4 grid grid-cols-[repeat(auto-fit,minmax(118px,1fr))] gap-2.5">
        <StatTile label="All" value={stats?.total ?? '—'} href={statusHref('')} active={statusFilter === ''} />
        {STATUSES.map(({ status, label }) => (
          <StatTile
            key={status}
            label={label}
            value={stats ? byStatus[status] ?? 0 : '—'}
            tone={statusTone(status)}
            href={statusHref(status)}
            active={statusFilter === status}
          />
        ))}
      </div>

      <div className="mb-4 flex flex-wrap items-center gap-4">
        <FormField inline label="Agent">
          <AgentSelect
            value={agentFilter}
            onChange={v => { setAgentFilter(v); setPage(0) }}
            platformExperimentID={scope.project_id ? undefined : peFilter}
          />
        </FormField>
        <FormField inline label="Experiment">
          <PlatformExperimentSelect
            value={peFilter}
            onChange={v => { setPEFilter(v); setPage(0) }}
            allLabel="All experiments"
          />
        </FormField>
      </div>

      {isLoading && <Loading />}
      {error && <ErrorMessage className="mb-4">Could not load the jobs: {error.message}</ErrorMessage>}
      {cancelError && <ErrorMessage className="mb-4">{cancelError}</ErrorMessage>}
      {clustersError && (
        <ErrorMessage>
          Could not load the clusters of {org.name}, so no job shows whether its cluster is connected: {clustersError.message}
        </ErrorMessage>
      )}

      {statusFilter === 'EVICTED' && evictionReasons.length > 0 && (
        <Card>
          <CardHeader><CardTitle note={faultSummary}>Why jobs were evicted</CardTitle></CardHeader>
          <CardContent>
            <ul className="grid max-w-[640px] gap-1 text-xs">
              {evictionReasons.map(([code, n]) => (
                <li key={code} className="flex items-baseline justify-between gap-4">
                  <span>{evictionCodeLabel(code)}</span>
                  <span className="font-mono tabular-nums text-muted">{n}</span>
                </li>
              ))}
            </ul>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader><CardTitle note={data ? total : undefined}>{shownLabel ? `${shownLabel} jobs` : 'Jobs'}</CardTitle></CardHeader>
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
                <th className="w-[92px] text-right">Est. cost</th>
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
                      No {shownLabel ? `${shownLabel.toLowerCase()} jobs` : 'jobs'}{agentFilter && ' of this agent'}
                      {peFilter && <> in <PlatformExperimentName id={peFilter} /></>}.
                    </EmptyState>
                  </td>
                </tr>
              ) : visible.map((job: Experiment) => {
                const j = job
                const status = j.status ?? 'UNKNOWN'
                const cost = j.estimated_cost_acch != null ? formatAccH(j.estimated_cost_acch) : null
                return (
                  <LinkTr key={job.id} href={`/jobs/${job.id}`}>
                    <td className={quiet}>{job.id.slice(0, 8)}…</td>
                    <td className="truncate text-xs" title={job.agent_id}>{job.agent_name}</td>
                    {!peFilter && (
                      <td className="text-xs [overflow-wrap:anywhere]">
                        <TextLink href={`/platform-experiments/${j.platform_experiment_id}`} onClick={e => e.stopPropagation()}>
                          <PlatformExperimentName id={j.platform_experiment_id} />
                        </TextLink>
                      </td>
                    )}
                    <td>
                      <Badge status={status} />
                      {j.summary_due && <div className="mt-0.5"><NoSummary /></div>}
                      {status === 'QUEUED' && j.not_admitted_reason && (
                        <div className="mt-0.5 text-2xs text-muted">
                          {notAdmittedLabel(j.not_admitted_reason)}
                        </div>
                      )}
                      {status === 'EVICTED' && j.eviction_reason && (
                        <div className="mt-0.5 text-2xs text-muted">{evictionLabel(j.eviction_reason)}</div>
                      )}
                      {j.capacity_tier === 'burst' && CANCELABLE.has(status) && (
                        <div className="mt-0.5 text-2xs text-muted">Burst compute, can be preempted</div>
                      )}
                      {j.cluster_name && (
                        <div className="mt-0.5 whitespace-nowrap font-mono text-2xs text-muted">
                          {connectedByCluster.has(j.cluster_name) && (
                            <>
                              <span
                                className={clsx('mr-1 inline-block h-1.5 w-1.5 rounded-full', connectedByCluster.get(j.cluster_name) ? 'bg-success' : 'bg-danger')}
                                title={connectedByCluster.get(j.cluster_name) ? 'Cluster connected' : 'Cluster disconnected'}
                              />
                              <span className="sr-only">{connectedByCluster.get(j.cluster_name) ? 'Connected: ' : 'Disconnected: '}</span>
                            </>
                          )}
                          {j.cluster_name}
                        </div>
                      )}
                    </td>
                    <td className="truncate font-mono text-2xs" title={j.accelerator_type}>
                      {j.accelerator_count}× {acceleratorLabel(j.accelerator_type)}
                    </td>
                    <td className={num}>
                      {j.metric ? j.metric.value.toFixed(4) : '—'}
                      {j.metric && <div className="mt-0.5 truncate text-2xs text-muted" title={j.metric.name}>{j.metric.name}</div>}
                    </td>
                    <td className={clsx(num, 'whitespace-nowrap')}>
                      {cost != null ? `${cost} ${budgetUnit(kindOf(j.accelerator_type))}` : '—'}
                    </td>
                    <td className="truncate text-xs" title={j.hypothesis}>
                      <TextLink href={`/hypotheses/${j.hypothesis_id}`} onClick={e => e.stopPropagation()}>{j.hypothesis}</TextLink>
                      <div className="mt-0.5 whitespace-normal font-mono text-2xs text-muted">
                        {envSettings(j, variedByHypothesis.get(j.hypothesis_id) ?? [])}
                      </div>
                    </td>
                    <td className={quiet}>{relTime(j.created_at)}</td>
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
                  </LinkTr>
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
