'use client'

import { Suspense, useEffect, useState } from 'react'
import { useRouter, useSearchParams } from 'next/navigation'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { TextLink } from '@/components/ui/link'
import { fetchExperimentsPage, fetchExperimentStats, cancelExperiment, fetchOrgClusters } from '@/lib/api'
import type { Experiment } from '@/types'
import { Page } from '@/components/ui/page'
import { AgentSelect } from '@/components/agent-select'
import { PlatformExperimentName } from '@/components/platform-experiment-name'
import { PlatformExperimentSelect } from '@/components/platform-experiment-select'
import { Section } from '@/components/ui/section'
import { StatTile } from '@/components/ui/stat-tile'
import { Badge, NoSummary } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { EmptyState } from '@/components/ui/empty-state'
import { Dot } from '@/components/ui/dot'
import { Inline, Stack } from '@/components/ui/stack'
import { Code, Text } from '@/components/ui/text'
import { FormField } from '@/components/ui/form-field'
import { LinkTr, SortableTh, Table, num, quiet } from '@/components/ui/table'
import { Pagination } from '@/components/ui/pagination'
import { acceleratorLabel, formatAccH, relTime, shortId } from '@/lib/format'
import { budgetUnit, kindOf } from '@/lib/units'
import { statusTone } from '@/lib/colors'
import { evictionCodeLabel, faultBreakdown, notAdmittedLabel } from '@/lib/eviction'
import { JobReason } from '@/components/job-reason'
import { experimentPath, jobPath, projectPath, withQuery } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { listProject, useExperimentAccess } from '@/lib/read-only'
import { variedEnvByHypothesis, variedEnvSettings } from '@/lib/hypothesis-progress'
import { Branch } from '@/components/code-ref'
import { JobHypothesis } from '@/components/job-hypothesis'

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

/** The jobs of the project, or of one experiment in it when `experimentID` names one. */
export function JobsList({ experimentID }: { experimentID?: string }) {
  return (
    <Suspense fallback={<Loading />}>
      <JobsListContent peFilter={experimentID ?? ''} />
    </Suspense>
  )
}

function JobsListContent({ peFilter }: { peFilter: string }) {
  const { org, project } = useTenant()
  const router = useRouter()
  const searchParams = useSearchParams()
  const [agentFilter, setAgentFilter] = useState('')
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

  // The jobs of one experiment are a page of their own; the status they are narrowed to is a filter.
  const listPath = (experimentID: string) => experimentID ? experimentPath(experimentID, 'jobs') : projectPath(project.id, 'jobs')
  const statusHref = (status: string) => withQuery(listPath(peFilter), { status })

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
    <Page
      title="Jobs"
      description="What agents ran for their experiments, with status and the compute each used. Choose a status to list its jobs."
    >
      {/* One tile per status: its count, and the filter that lists those jobs. */}
      <div className="grid grid-cols-tiles-sm gap-3">
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

      <Inline gap="lg">
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
            onChange={v => router.push(withQuery(listPath(v), { status: statusFilter }))}
            allLabel="All experiments"
          />
        </FormField>
      </Inline>

      {isLoading && <Loading />}
      {error && <ErrorMessage>Could not load the jobs: {error.message}</ErrorMessage>}
      {cancelError && <ErrorMessage>{cancelError}</ErrorMessage>}
      {clustersError && (
        <ErrorMessage>
          Could not load the clusters of {org.name}, so no job shows whether its cluster is connected: {clustersError.message}
        </ErrorMessage>
      )}

      {statusFilter === 'EVICTED' && evictionReasons.length > 0 && (
        <Section title="Why jobs were evicted" note={faultSummary}>
          <Stack as="ul" gap="xs" className="max-w-2xl text-xs">
            {evictionReasons.map(([code, n]) => (
              <li key={code} className="flex items-baseline justify-between gap-4">
                <span>{evictionCodeLabel(code)}</span>
                <Code className="tabular-nums text-muted">{n}</Code>
              </li>
            ))}
          </Stack>
        </Section>
      )}

      <Section title={shownLabel ? `${shownLabel} jobs` : 'Jobs'} note={data ? total : undefined} scrollX>
          {/* Fixed widths for the short columns; experiment and hypothesis share what is left. An
              experiment filter makes the experiment column the same on every row, so it goes. */}
          <Table className="min-w-5xl table-fixed">
            <thead>
              <tr>
                <th className="w-24">Job ID</th>
                <th className="w-36">Agent</th>
                {!peFilter && <th>Experiment</th>}
                <SortableTh className="w-36" sorted={sortKey === 'status' && sortDir} onSort={() => toggleSort('status')}>Status</SortableTh>
                <th className="w-32">Accelerator</th>
                <th className="w-28 text-right" title="The last value the job reported on the ranking metric, which it is ranked by">Last metric</th>
                <th className="w-24 text-right">Est. cost</th>
                <th title="Under each hypothesis: the branch the job ran on, and the settings that differ between this page's jobs of it">Hypothesis · branch · varied settings</th>
                <SortableTh className="w-28" sorted={sortKey === 'created_at' && sortDir} onSort={() => toggleSort('created_at')}>Submitted</SortableTh>
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
                  <LinkTr key={job.id} href={jobPath(j.platform_experiment_id, job.id)}>
                    <td className={quiet}>{shortId(job.id)}</td>
                    <td className="truncate text-xs" title={job.agent_id}>{job.agent_name}</td>
                    {!peFilter && (
                      <td className="break-words text-xs">
                        <TextLink href={experimentPath(j.platform_experiment_id)} onClick={e => e.stopPropagation()}>
                          <PlatformExperimentName id={j.platform_experiment_id} />
                        </TextLink>
                      </td>
                    )}
                    <td>
                      <Badge status={status} />
                      {j.summary_due && <div><NoSummary /></div>}
                      {status === 'QUEUED' && j.not_admitted_reason && (
                        <Text as="div" variant="meta">
                          {notAdmittedLabel(j.not_admitted_reason, j.not_admitted_message)}
                        </Text>
                      )}
                      {j.eviction_reason && (
                        <Text as="div" variant="meta"><JobReason reason={j.eviction_reason} /></Text>
                      )}
                      {j.capacity_tier === 'burst' && CANCELABLE.has(status) && (
                        <Text as="div" variant="meta">Burst compute, can be preempted</Text>
                      )}
                      {j.cluster_name && (
                        <Text as="div" variant="meta" mono className="whitespace-nowrap">
                          {connectedByCluster.has(j.cluster_name) && (
                            <span title={connectedByCluster.get(j.cluster_name) ? 'Cluster connected' : 'Cluster disconnected'}>
                              <Dot tone={connectedByCluster.get(j.cluster_name) ? 'success' : 'danger'} size="sm" className="mr-1" />
                              <span className="sr-only">{connectedByCluster.get(j.cluster_name) ? 'Connected: ' : 'Disconnected: '}</span>
                            </span>
                          )}
                          {j.cluster_name}
                        </Text>
                      )}
                    </td>
                    <td className="truncate font-mono text-2xs" title={j.accelerator_type}>
                      {j.accelerator_count}× {acceleratorLabel(j.accelerator_type)}
                    </td>
                    <td className={num}>
                      {j.metric ? j.metric.value.toFixed(4) : '—'}
                      {j.metric && <Text as="div" variant="meta" className="truncate" title={j.metric.name}>{j.metric.name}</Text>}
                    </td>
                    <td className={clsx(num, 'whitespace-nowrap')}>
                      {cost != null ? `${cost} ${budgetUnit(kindOf(j.accelerator_type))}` : '—'}
                    </td>
                    <td className="truncate text-xs" title={j.hypothesis}>
                      <JobHypothesis job={j} />
                      <Text as="div" variant="meta" mono className="whitespace-normal">
                        {j.branch && <><Branch branch={j.branch} codeRef={j.code_ref} platformExperimentID={j.platform_experiment_id} />{' '}</>}
                        {variedEnvSettings(j, variedByHypothesis)}
                      </Text>
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
      </Section>

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
    </Page>
  )
}
