'use client'

import { Suspense, useEffect, useState } from 'react'
import { useRouter, useSearchParams } from 'next/navigation'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { Info } from 'lucide-react'
import { TextLink } from '@/components/ui/link'
import { fetchOrgClusters } from '@/lib/api/clusters'
import { cancelExperiment, fetchExperimentsPage, fetchExperimentStats } from '@/lib/api/jobs'
import { fetchPlatformExperiment } from '@/lib/api/platform-experiments'
import { Page } from '@/components/ui/page'
import { AgentSelect } from '@/components/agent-select'
import { PlatformExperimentName } from '@/components/platform-experiment-name'
import { PlatformExperimentSelect } from '@/components/platform-experiment-select'
import { Section } from '@/components/ui/section'
import { StatTile } from '@/components/ui/stat-tile'
import { Fetched } from '@/components/ui/fetched'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { EmptyState } from '@/components/ui/empty-state'
import { Dot } from '@/components/ui/dot'
import { Inline, Stack } from '@/components/ui/stack'
import { Code } from '@/components/ui/text'
import { FormField } from '@/components/ui/form-field'
import { LinkTr, RowAnchor, SortableTh, Table, num, quiet } from '@/components/ui/table'
import { Pagination } from '@/components/ui/pagination'
import { acceleratorLabel, formatAccH, relTime, shortId } from '@/lib/format'
import { budgetUnit, kindOf } from '@/lib/units'
import { statusTone } from '@/lib/colors'
import { evictionCode, evictionCodeLabel, evictionDetailLines, faultBreakdown, notAdmittedLabel } from '@/lib/eviction'
import { experimentPath, jobPath, projectPath, withQuery } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { listProject, useExperimentAccess } from '@/lib/experiment-actions'
import { variedEnvByHypothesis, variedEnvSettings } from '@/lib/hypothesis-progress'
import { BASELINE_JOB, JobHypothesis, measuresBaseline } from '@/components/job-hypothesis'
import { keys } from '@/lib/keys'
import { POLL } from '@/lib/poll'
import type { Experiment, ExperimentStatus } from '@/types/api'

// The statuses of a job that has not ended, which burst compute may still preempt.
const LIVE = new Set<ExperimentStatus>(['SUBMITTED', 'QUEUED', 'RUNNING'])
const PAGE_SIZE = 25

type SortKey = 'created_at' | 'status'

// Every status a job can hold, in the order a job moves through them.
const STATUS_LABELS: Record<ExperimentStatus, string> = {
  QUEUED: 'Queued',
  SUBMITTED: 'Submitted',
  RUNNING: 'Running',
  COMPLETED: 'Completed',
  FAILED: 'Failed',
  EVICTED: 'Evicted',
  REJECTED: 'Rejected',
  CANCELLED: 'Cancelled',
}
const STATUSES = Object.entries(STATUS_LABELS).map(([status, label]) => ({ status: status as ExperimentStatus, label }))

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
  // The jobs of a platform experiment of another project are asked for by it alone, which is
  // known once the experiment has been read.
  const access = useExperimentAccess(peFilter || undefined)
  const scope = listProject(project, access)
  const scopeKey = scope.project_id ?? 'shared'
  const statusFilter = searchParams.get('status') ?? ''
  const [confirming, setConfirming] = useState<string | null>(null)
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
  const { data: clusters, error: clustersError } = useSWR(keys.orgClusters(org.id), () => fetchOrgClusters(org.id), { refreshInterval: POLL.reports })
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

  const { data, error, mutate } = useSWR(
    access.known ? keys.jobs(scopeKey, agentFilter, peFilter, statusFilter, page, sort) : null,
    () => fetchExperimentsPage({
      ...scope,
      agent_id: agentFilter || undefined,
      platform_experiment_id: peFilter || undefined,
      status: statusFilter || undefined,
      limit: PAGE_SIZE,
      offset: page * PAGE_SIZE,
      sort,
    }),
  )

  async function cancel(id: string) {
    await cancelExperiment(id)
    await mutate()
  }

  const visible = data?.items ?? []
  const cancels = visible.some(job => job.actions.includes('cancel'))
  const total = data?.total ?? 0
  const variedByHypothesis = variedEnvByHypothesis(visible)

  // The tiles count every job the agent and experiment filters match, whatever its status, so
  // each tile says how many jobs choosing it lists.
  const { data: stats, error: statsError } = useSWR(
    access.known ? keys.jobListStats(scopeKey, agentFilter, peFilter) : null,
    () => fetchExperimentStats({
      ...scope,
      agent_id: agentFilter || undefined,
      platform_experiment_id: peFilter || undefined,
    }),
  )

  const evictionReasons = Object.entries(stats?.evictions_by_reason ?? {}).sort((a, b) => b[1] - a[1])
  const faultSummary = faultBreakdown(stats?.evictions_by_class ?? {})
  const shownLabel = STATUSES.find(s => s.status === statusFilter)?.label

  return (
    <Page
      title="Jobs"
    >
      {/* One tile per status: its count, and the filter that lists those jobs. */}
      <div className="grid grid-cols-tiles-sm gap-3">
        <StatTile label="All" value={<Fetched data={stats} error={statsError}>{s => s.total}</Fetched>} href={statusHref('')} active={statusFilter === ''} />
        {STATUSES.map(({ status, label }) => (
          <StatTile
            key={status}
            label={label}
            value={<Fetched data={stats} error={statsError}>{s => s.by_status[status] ?? 0}</Fetched>}
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

      {error && <ErrorMessage>Could not load the jobs: {error.message}</ErrorMessage>}
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

      {!data && !error && <Loading />}
      {data && (
        <Section title={shownLabel ? `${shownLabel} jobs` : 'Jobs'} note={total} scrollX>
          {/* One line a row. The short columns are fixed; experiment and hypothesis share what is
              left and truncate, their whole text on hover. Agent, accelerator, cost and submitted
              step aside on a narrow window, and stay in the job's own tooltip. An experiment
              filter makes the experiment the same on every row, so its column goes. */}
          <Table className="min-w-[41rem] table-fixed">
            <thead>
              <tr>
                <th className="w-[5.5rem]">Job</th>
                {!peFilter && <th className="w-[16%]">Experiment</th>}
                <th title="What the job tests. Hover a row for its branch and the settings that differ between this page's jobs of it">Hypothesis</th>
                <th className="hidden w-32 2xl:table-cell">Agent</th>
                <SortableTh className="w-36" sorted={sortKey === 'status' && sortDir} onSort={() => toggleSort('status')}>Status</SortableTh>
                <th className="hidden w-32 2xl:table-cell">Accelerator</th>
                <th className="w-24 text-right" title="The last value the job reported on the ranking metric, which it is ranked by">Last metric</th>
                <th className="hidden w-32 text-right xl:table-cell">Est. cost</th>
                <SortableTh className="hidden w-28 xl:table-cell" sorted={sortKey === 'created_at' && sortDir} onSort={() => toggleSort('created_at')}>Submitted</SortableTh>
                {cancels && <th className="w-[5.5rem]"><span className="sr-only">Actions</span></th>}
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
              ) : visible.map(job => (
                <JobRow
                  key={job.id}
                  job={job}
                  showExperiment={!peFilter}
                  varied={variedEnvSettings(job, variedByHypothesis)}
                  connected={job.cluster_name ? connectedByCluster.get(job.cluster_name) : undefined}
                  onCancel={cancels ? () => setConfirming(job.id) : undefined}
                />
              ))}
            </tbody>
          </Table>
          <Pagination page={page} pageSize={PAGE_SIZE} total={total} onPageChange={setPage} />
        </Section>
      )}

      <ConfirmDialog
        open={confirming !== null}
        title="Cancel this job?"
        confirmLabel="Cancel job"
        cancelLabel="Keep job"
        onConfirm={() => cancel(confirming!)}
        onClose={() => setConfirming(null)}
      >
        The job stops and the compute it holds is released.
      </ConfirmDialog>
    </Page>
  )
}

interface JobRowProps {
  job: Experiment
  showExperiment: boolean
  /** The settings that differ between this page's jobs of its hypothesis. */
  varied: string
  /** Whether its cluster's agent is connected; unknown until the clusters are read. */
  connected: boolean | undefined
  onCancel?: () => void
}

function JobRow({ job, showExperiment, varied, connected, onCancel }: JobRowProps) {
  const status = job.status ?? 'UNKNOWN'
  const accelerator = `${job.accelerator_count}× ${acceleratorLabel(job.accelerator_type)}`
  const cost = job.estimated_cost_acch != null ? `${formatAccH(job.estimated_cost_acch)} ${budgetUnit(kindOf(job.accelerator_type))}` : '—'
  const about = [
    status === 'QUEUED' && job.not_admitted_reason && notAdmittedLabel(job.not_admitted_reason, job.not_admitted_message),
    job.eviction_reason && [evictionCodeLabel(evictionCode(job.eviction_reason)), evictionDetailLines(job.eviction_reason)[0]].filter(Boolean).join(': '),
    job.capacity_tier === 'burst' && LIVE.has(job.status) && 'Burst compute, can be preempted',
    job.cluster_name && `On ${job.cluster_name}${connected === undefined ? '' : connected ? ', connected' : ', disconnected'}`,
  ].filter(Boolean).join('\n')
  const tested = [measuresBaseline(job) ? BASELINE_JOB : job.hypothesis, job.branch && `branch ${job.branch}`, varied].filter(Boolean).join('\n')

  return (
    <LinkTr href={jobPath(job.platform_experiment_id, job.id)}>
      <td className={quiet} title={`${job.id}\n${job.agent_name} · ${accelerator} · est. ${cost} · submitted ${new Date(job.created_at).toLocaleString()}`}>
        <RowAnchor>{shortId(job.id)}</RowAnchor>
      </td>
      {showExperiment && <td className="truncate text-xs"><ExperimentLink id={job.platform_experiment_id} /></td>}
      <td className="truncate text-xs" title={tested}>
        <JobHypothesis job={job} />
        {varied && <span className="ml-2 font-mono text-2xs text-subtle">{varied}</span>}
      </td>
      <td className="hidden truncate text-xs 2xl:table-cell" title={job.agent_id}>{job.agent_name}</td>
      <td className="truncate" title={about || undefined}>
        <Inline gap="xs" nowrap>
          <Badge status={status} />
          {connected === false && (
            <span title="Its cluster is disconnected">
              <Dot tone="danger" size="sm" />
              <span className="sr-only">Cluster disconnected</span>
            </span>
          )}
          {about && <Info aria-label={about} role="img" size={12} className="flex-none text-subtle" />}
        </Inline>
      </td>
      <td className="hidden truncate font-mono text-2xs 2xl:table-cell" title={job.accelerator_type}>{accelerator}</td>
      <td className={clsx(num, 'truncate')} title={job.metric?.name}>{job.metric ? job.metric.value.toFixed(4) : '—'}</td>
      <td className={clsx(num, 'hidden truncate xl:table-cell')} title={cost}>{cost}</td>
      <td className={clsx(quiet, 'hidden xl:table-cell')} title={new Date(job.created_at).toLocaleString()}>{relTime(job.created_at)}</td>
      {onCancel && (
        <td className="text-right">
          {job.actions.includes('cancel') && <Button variant="danger" size="sm" onClick={onCancel}>Cancel</Button>}
        </td>
      )}
    </LinkTr>
  )
}

/** An experiment by its name, linked to it; its id only while the name is read or when it cannot be. */
function ExperimentLink({ id }: { id: string }) {
  const { data, error } = useSWR(keys.experiment(id), () => fetchPlatformExperiment(id))
  if (!data) return <span className="font-mono text-2xs text-subtle" title={error ? `Could not load its name: ${error.message}` : id}>{error ? id : '…'}</span>
  return <TextLink href={experimentPath(id)} title={data.name}>{data.name}</TextLink>
}
