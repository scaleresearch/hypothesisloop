'use client'

import { Fragment, Suspense, useMemo, useState } from 'react'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { TextLink } from '@/components/ui/link'
import { useReactTable, getCoreRowModel, getSortedRowModel, type SortingState } from '@tanstack/react-table'
import {
  ApiError, fetchAgentQuota, fetchAgentsPage, fetchAgentStats, fetchPlatformExperiment, fetchPlatformExperimentsPage,
  fetchExperimentStats, fetchPlatformExperimentQuotas, fetchPlatformExperimentResults, MAX_LIST_PAGE_SIZE,
} from '@/lib/api'
import type { Draft } from '@/lib/planning'
import type { PlatformExperiment } from '@/types'
import { Page } from '@/components/ui/page'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { Code, Text } from '@/components/ui/text'
import { EmptyState } from '@/components/ui/empty-state'
import { Badge } from '@/components/ui/badge'
import { FormField } from '@/components/ui/form-field'
import { StatTile } from '@/components/ui/stat-tile'
import { ColumnTh, Table, num } from '@/components/ui/table'
import { Loading, ErrorMessage, InlineError } from '@/components/ui/status-message'
import { PlatformExperimentSelect } from '@/components/platform-experiment-select'
import { Pagination } from '@/components/ui/pagination'
import { formatAccH } from '@/lib/format'
import { budgetKind, budgetUnit } from '@/lib/units'
import { useQueryParam } from '@/lib/query-state'
import { useTenant } from '@/lib/tenant'

const PAGE_SIZE = 25

function unitOf(pe: PlatformExperiment): string {
  return budgetUnit(budgetKind((pe as Draft).proposal?.accelerator_type, []))
}

// One agent's compute in one experiment. An agent that is not in the experiment has none, which
// the API answers 404 and this shows as no row.
function AgentExperimentQuota({ agentId, pe, colSpan }: { agentId: string; pe: PlatformExperiment; colSpan: number }) {
  const { data: q, error } = useSWR(
    ['agent-quota', pe.id, agentId],
    () => fetchAgentQuota(pe.id, agentId),
    { refreshInterval: 30_000, shouldRetryOnError: false },
  )
  if (error instanceof ApiError && error.status === 404) return null
  if (error) return (
    <tr><td colSpan={colSpan} className="pl-7"><InlineError>Could not load its compute in {pe.name}: {error.message}</InlineError></td></tr>
  )
  if (!q) return null
  const unit = unitOf(pe)
  const room = q.remaining_guaranteed_accelerator_hours > 0 || q.remaining_burst_accelerator_hours > 0
  return (
    <tr className="bg-surface-inset">
      <td colSpan={colSpan} className="pl-7 text-2xs">
        <Inline gap="xl">
          <TextLink href={`/platform-experiments/${pe.id}`} className="text-xs">{pe.name}</TextLink>
          <span>
            <Badge status="guaranteed" className="mr-1">Guaranteed</Badge>
            <Code>{formatAccH(q.used_guaranteed_acch)} / {formatAccH(q.guaranteed_accelerator_hours)}</Code> {unit} used
          </span>
          <span>
            <Badge status="burst" className="mr-1">Burst</Badge>
            <Code>{formatAccH(q.used_burst_acch)} / {formatAccH(q.burst_accelerator_hours)}</Code> {unit} used
          </span>
          <span className={room ? 'text-success' : 'text-danger'} title="The largest job a submit is admitted with now: every job that has not ended holds its whole estimate">
            room for a job of <Code>{formatAccH(q.remaining_guaranteed_accelerator_hours)}</Code> guaranteed
            or <Code>{formatAccH(q.remaining_burst_accelerator_hours)}</Code> burst {unit}
          </span>
        </Inline>
      </td>
    </tr>
  )
}

function AgentQuotaRows({ agentId, experiments, colSpan }: { agentId: string; experiments: PlatformExperiment[]; colSpan: number }) {
  if (experiments.length === 0) return (
    <tr><td colSpan={colSpan} className="pl-5 text-xs text-subtle">No running experiments</td></tr>
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
  const { project } = useTenant()
  const [expandedAgent, setExpandedAgent] = useState<string | null>(null)
  const [experimentFilter, setExperimentFilter] = useQueryParam('platform_experiment_id')
  const [page, setPage] = useState(0)

  const { data, error, isLoading } = useSWR(
    ['agents-page', project.id, experimentFilter, page],
    () => fetchAgentsPage({ project_id: project.id, platform_experiment_id: experimentFilter, limit: PAGE_SIZE, offset: page * PAGE_SIZE }),
    { refreshInterval: 15_000, keepPreviousData: true },
  )
  const agents = data?.items
  const total = data?.total ?? 0

  const { data: stats } = useSWR(
    ['agent-stats', project.id, experimentFilter],
    () => fetchAgentStats({ project_id: project.id, platform_experiment_id: experimentFilter }),
    { refreshInterval: 15_000, keepPreviousData: true },
  )

  // Quota exists from an experiment's start, so only running ones have a row to show.
  const { data: running } = useSWR<PlatformExperiment[]>(
    ['platform-experiments-running', project.id],
    () => fetchPlatformExperimentsPage({ project_id: project.id, status: 'running', limit: MAX_LIST_PAGE_SIZE }).then(p => p.items),
    { refreshInterval: 30_000 },
  )

  const { data: selectedPE } = useSWR(
    experimentFilter ? ['pe', experimentFilter] : null,
    () => fetchPlatformExperiment(experimentFilter),
  )
  // What the platform allocated each agent of the selected experiment.
  const { data: quotas } = useSWR(
    experimentFilter ? ['pe-quotas', experimentFilter] : null,
    () => fetchPlatformExperimentQuotas(experimentFilter),
    { refreshInterval: 30_000 },
  )

  // What each agent did in the selected experiment: its jobs, its best result and standing, and
  // how many of its completed jobs it left without a summary.
  const { data: jobStats } = useSWR(
    experimentFilter ? ['pe-stats-by-agent', experimentFilter] : null,
    () => fetchExperimentStats({ project_id: project.id, platform_experiment_id: experimentFilter, group_by: 'agent' }),
    { refreshInterval: 15_000 },
  )
  const { data: summariesDue } = useSWR(
    experimentFilter ? ['pe-summaries-due-by-agent', experimentFilter] : null,
    () => fetchExperimentStats({ project_id: project.id, platform_experiment_id: experimentFilter, group_by: 'agent', needs_summary: true }),
    { refreshInterval: 15_000 },
  )
  const { data: results } = useSWR(
    experimentFilter ? ['pe-results', experimentFilter] : null,
    () => fetchPlatformExperimentResults(experimentFilter),
    { refreshInterval: 15_000 },
  )
  const ranking = results?.metrics[0]
  const colSpan = experimentFilter ? 8 : 4

  const [sorting, setSorting] = useState<SortingState>([{ id: 'performance_score', desc: true }])
  const agentColumns = useMemo(() => [
    { accessorKey: 'name', header: 'Agent' },
    { accessorKey: 'top3_count', header: 'Top-3 bonus' },
    { accessorKey: 'performance_score', header: 'Performance score' },
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

  return (
    <Page
      title="Research Agents"
      description="The agents of this project, how they have placed, and the compute each holds in running experiments."
    >
      <div className="grid grid-cols-3 gap-3">
        <StatTile label="Agents" value={total || '—'} />
        <StatTile label="With the top-3 bonus" value={stats?.top3_eligible ?? '—'} tone="success" sub="25% more guaranteed compute" />
        <StatTile label="Average performance score" value={stats?.total ? stats.avg_performance_score.toFixed(3) : '—'} />
      </div>

      <Stack gap="sm">
        <FormField inline label="Experiment">
          <PlatformExperimentSelect
            value={experimentFilter}
            onChange={v => { setExperimentFilter(v); setPage(0) }}
            allLabel="All experiments"
          />
        </FormField>
        <Text variant="note" className="max-w-4xl">
          What an experiment&apos;s baseline leaves of its budget is split equally among its agents when it starts, the baseline agent apart,
          and each stage releases its part of it. An agent that placed in the top three of an earlier experiment is guaranteed 25% more. Beyond what is guaranteed, an agent may use up to
          twice that amount as burst compute, which can be preempted.
        </Text>
      </Stack>

      {isLoading && <Loading />}
      {error && <ErrorMessage>Could not load the agents of {project.name}: {error.message}</ErrorMessage>}

      <Section title="Agents" note={agents ? total : undefined} scrollX>
          <Table>
            <thead>
              <tr>
                {agentTable.getFlatHeaders().map(h => (
                  <ColumnTh
                    key={h.id}
                    header={h}
                    className={clsx(h.id === 'top3_count' && 'text-center', h.id === 'performance_score' && 'text-right')}
                  />
                ))}
                {experimentFilter && (
                  <>
                    <th className="text-right">Share{selectedPE ? `, ${unitOf(selectedPE)}` : ''}</th>
                    <th className="text-right">Best {ranking?.metric ?? 'metric'}</th>
                    <th>Jobs</th>
                    <th>Summaries filed</th>
                  </>
                )}
                <th><span className="sr-only">Compute</span></th>
              </tr>
            </thead>
            <tbody>
              {!agents || agents.length === 0 ? (
                <tr>
                  <td colSpan={colSpan} className="p-0">
                    <EmptyState>
                      {experimentFilter ? 'No agents in this experiment.' : 'No agents yet. An agent appears here once it is added to an experiment.'}
                    </EmptyState>
                  </td>
                </tr>
              ) : sortedAgents.map(a => {
                const isExpanded = expandedAgent === a.id
                const standing = ranking?.standings.find(st => st.agent_id === a.id)
                const quota = quotas?.find(q => q.agent_id === a.id)
                const jobs = jobStats?.groups?.[a.id]
                const completed = jobs?.by_status.COMPLETED ?? 0
                const due = summariesDue?.groups?.[a.id]?.total ?? 0
                const shown = selectedPE ? [selectedPE] : running
                return (
                  <Fragment key={a.id}>
                    <tr
                      className="cursor-pointer"
                      tabIndex={0}
                      aria-expanded={isExpanded}
                      onClick={() => setExpandedAgent(isExpanded ? null : a.id)}
                      onKeyDown={e => { if (e.key === 'Enter' && e.target === e.currentTarget) setExpandedAgent(isExpanded ? null : a.id) }}
                    >
                      <td className="text-xs font-medium" title={a.id}>{a.name}</td>
                      <td className="text-center">
                        {a.top3_count > 0 ? <Badge tone="success">+25%</Badge> : <span className="text-xs text-subtle">—</span>}
                      </td>
                      <td className={num}>{a.performance_score.toFixed(3)}</td>
                      {experimentFilter && (
                        <>
                          <td className={num} title="Its part of the budget released so far">
                            {quota ? formatAccH(quota.share_accelerator_hours) : <span className="text-subtle">—</span>}
                          </td>
                          <td className={num}>
                            {standing?.experiment_id ? (
                              <TextLink href={`/jobs/${standing.experiment_id}`} onClick={e => e.stopPropagation()}>
                                #{standing.rank} · {standing.best.toFixed(4)}
                              </TextLink>
                            ) : <span className="text-subtle">—</span>}
                          </td>
                          <td className="text-2xs">
                            {jobs
                              ? Object.entries(jobs.by_status).map(([status, n]) => `${n} ${status.toLowerCase()}`).join(' · ')
                              : <span className="text-xs text-subtle">none</span>}
                          </td>
                          <td className={clsx('text-2xs', due > 0 && 'text-warning')}>
                            {summariesDue ? `${completed - due} of ${completed} completed` : '—'}
                          </td>
                        </>
                      )}
                      <td className="whitespace-nowrap text-right text-xs text-subtle">{isExpanded ? 'Hide compute ▲' : 'Compute ▼'}</td>
                    </tr>
                    {isExpanded && shown && <AgentQuotaRows agentId={a.id} experiments={shown} colSpan={colSpan} />}
                  </Fragment>
                )
              })}
            </tbody>
          </Table>
          <Pagination page={page} pageSize={PAGE_SIZE} total={total} onPageChange={setPage} />
      </Section>
    </Page>
  )
}
