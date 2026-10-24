'use client'

import { Fragment, Suspense, useMemo, useState } from 'react'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { TextLink } from '@/components/ui/link'
import { useReactTable, getCoreRowModel, getSortedRowModel, type SortingState } from '@tanstack/react-table'
import { fetchAgentsPage, fetchAgentStats } from '@/lib/api/agents'
import { fetchExperimentStats } from '@/lib/api/jobs'
import { fetchPlatformExperiment, fetchPlatformExperimentResults, fetchPlatformExperimentsPage } from '@/lib/api/platform-experiments'
import { fetchAgentQuota, fetchPlatformExperimentQuotas } from '@/lib/api/quotas'
import { ApiError, MAX_LIST_PAGE_SIZE } from '@/lib/api/request'
import { Page } from '@/components/ui/page'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { InfoTip } from '@/components/ui/info-tip'
import { Code, Text } from '@/components/ui/text'
import { ExperimentAgents } from '@/components/experiment-agents'
import { Team } from '@/components/team'
import { EmptyState } from '@/components/ui/empty-state'
import { Badge } from '@/components/ui/badge'
import { FormField } from '@/components/ui/form-field'
import { StatTile } from '@/components/ui/stat-tile'
import { Fetched } from '@/components/ui/fetched'
import { ColumnTh, Table, num } from '@/components/ui/table'
import { Loading, ErrorMessage, InlineError } from '@/components/ui/status-message'
import { PlatformExperimentSelect } from '@/components/platform-experiment-select'
import { Pagination } from '@/components/ui/pagination'
import { formatAccH, formatMetric } from '@/lib/format'
import { budgetUnit } from '@/lib/units'
import { closedWhilePlanned } from '@/lib/phase'
import { useRouter } from 'next/navigation'
import { experimentPath, jobPath, projectPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { keys } from '@/lib/keys'
import { POLL } from '@/lib/poll'
import type { PlatformExperiment } from '@/types/api'

const PAGE_SIZE = 25

function unitOf(pe: PlatformExperiment): string {
  return budgetUnit(pe.budget_kind)
}

// One agent's compute in one experiment. An agent that is not in the experiment has none, which
// the API answers 404 and this shows as no row.
function AgentExperimentQuota({ agentId, pe, colSpan }: { agentId: string; pe: PlatformExperiment; colSpan: number }) {
  const { data: q, error } = useSWR(
    keys.agentQuota(pe.id, agentId),
    () => fetchAgentQuota(pe.id, agentId),
    { refreshInterval: POLL.slow },
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
          <TextLink href={experimentPath(pe.id)} className="text-xs">{pe.name}</TextLink>
          <span>
            <Badge status="guaranteed" className="mr-1">Guaranteed</Badge>
            <Code>{formatAccH(q.used_guaranteed_acch)} / {formatAccH(q.guaranteed_accelerator_hours)}</Code> {unit} reserved
          </span>
          <span>
            <Badge status="burst" className="mr-1">Burst</Badge>
            <Code>{formatAccH(q.used_burst_acch)} / {formatAccH(q.burst_accelerator_hours)}</Code> {unit} reserved
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

function AgentQuotaRows({ agentId, experiments, error, colSpan }: { agentId: string; experiments: PlatformExperiment[] | undefined; error: Error | undefined; colSpan: number }) {
  if (error) return (
    <tr><td colSpan={colSpan} className="pl-7"><InlineError>Could not load the experiments it holds compute in: {error.message}</InlineError></td></tr>
  )
  if (!experiments) return <tr><td colSpan={colSpan} className="pl-7"><Loading rows={1} /></td></tr>
  if (experiments.length === 0) return (
    <tr><td colSpan={colSpan} className="pl-5 text-xs text-subtle">No running experiments</td></tr>
  )
  return <>{experiments.map(pe => <AgentExperimentQuota key={pe.id} agentId={agentId} pe={pe} colSpan={colSpan} />)}</>
}

/** The agents of the project, or of one experiment in it when `experimentID` names one. */
export function AgentsList({ experimentID }: { experimentID?: string }) {
  return (
    <Suspense fallback={<Loading />}>
      <AgentsListContent experimentFilter={experimentID ?? ''} />
    </Suspense>
  )
}

function AgentsListContent({ experimentFilter }: { experimentFilter: string }) {
  const { project } = useTenant()
  const router = useRouter()
  const [expandedAgent, setExpandedAgent] = useState<string | null>(null)
  const [page, setPage] = useState(0)

  const { data, error } = useSWR(
    keys.agents(project.id, experimentFilter, page),
    () => fetchAgentsPage({ project_id: project.id, platform_experiment_id: experimentFilter, limit: PAGE_SIZE, offset: page * PAGE_SIZE }),
  )
  const agents = data?.items
  const total = data?.total ?? 0

  const { data: stats, error: statsError } = useSWR(
    keys.agentStats(project.id, experimentFilter),
    () => fetchAgentStats({ project_id: project.id, platform_experiment_id: experimentFilter }),
  )

  // Quota exists from an experiment's start, so only running ones have a row to show.
  const { data: running, error: runningError } = useSWR<PlatformExperiment[]>(
    experimentFilter ? null : keys.runningExperiments(project.id),
    () => fetchPlatformExperimentsPage({ project_id: project.id, status: 'running', limit: MAX_LIST_PAGE_SIZE }).then(p => p.items),
  )

  const { data: selectedPE, error: selectedPEError } = useSWR(
    experimentFilter ? keys.experiment(experimentFilter) : null,
    () => fetchPlatformExperiment(experimentFilter),
  )
  // What the platform allocated each agent of the selected experiment.
  const { data: quotas, error: quotasError } = useSWR(
    experimentFilter ? keys.quotas(experimentFilter) : null,
    () => fetchPlatformExperimentQuotas(experimentFilter),
    { refreshInterval: POLL.slow },
  )

  // What each agent did in the selected experiment: its jobs, its best result and standing.
  const { data: jobStats, error: jobStatsError } = useSWR(
    experimentFilter ? keys.jobStatsByAgent(experimentFilter) : null,
    () => fetchExperimentStats({ project_id: project.id, platform_experiment_id: experimentFilter, group_by: 'agent' }),
  )
  const { data: results, error: resultsError } = useSWR(
    experimentFilter ? keys.results(experimentFilter) : null,
    () => fetchPlatformExperimentResults(experimentFilter),
    { refreshInterval: POLL.standing },
  )
  const ranking = results?.metrics[0]
  const shown = experimentFilter
    ? { experiments: selectedPE && [selectedPE], error: selectedPEError }
    : { experiments: running, error: runningError }
  const colSpan = experimentFilter ? 6 : 3

  const [sorting, setSorting] = useState<SortingState>([{ id: 'top3_count', desc: true }])
  const agentColumns = useMemo(() => [
    { accessorKey: 'name', header: 'Agent' },
    { accessorKey: 'top3_count', header: 'Top-3 bonus' },
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
    >
      <div className="grid grid-cols-2 gap-3">
        <StatTile label="Agents" value={<Fetched data={data} error={error}>{d => d.total}</Fetched>} />
        <StatTile
          label="With the top-3 bonus"
          value={<Fetched data={stats} error={statsError}>{s => s.top3_eligible}</Fetched>}
          tone="success"
          sub="25% more guaranteed compute"
        />
      </div>

      <Stack gap="sm">
        <FormField inline label="Experiment">
          <PlatformExperimentSelect
            value={experimentFilter}
            onChange={v => router.push(v ? experimentPath(v, 'agents') : projectPath(project.id, 'agents'))}
            allLabel="All experiments"
          />
        </FormField>
        <Text variant="note" className="max-w-4xl">
          Each agent gets an equal share of the budget its baseline leaves.
          <InfoTip>What an experiment&apos;s baseline leaves of its budget is split equally among its agents when it starts, the baseline agent apart, and each stage releases its part of it. An agent that placed in the top three of an earlier experiment is guaranteed 25% more. Beyond what is guaranteed, an agent may use up to twice that amount as burst compute, which can be preempted.</InfoTip>
        </Text>
      </Stack>

      {selectedPE && (
        <>
          {!closedWhilePlanned(selectedPE) && <Team pe={selectedPE} />}
          <ExperimentAgents pe={selectedPE} />
        </>
      )}

      {error && <ErrorMessage>Could not load the agents of {project.name}: {error.message}</ErrorMessage>}
      {!agents && !error && <Loading />}

      {agents && (
        <Section title={experimentFilter ? 'Compute and results' : 'Agents'} note={total} scrollX>
          <Table>
            <thead>
              <tr>
                {agentTable.getFlatHeaders().map(h => (
                  <ColumnTh
                    key={h.id}
                    header={h}
                    className={clsx(h.id === 'top3_count' && 'text-center')}
                  />
                ))}
                {experimentFilter && (
                  <>
                    <th className="text-right">Share{selectedPE ? `, ${unitOf(selectedPE)}` : ''}</th>
                    <th className="text-right">Best {ranking?.metric ?? 'metric'}</th>
                    <th>Jobs</th>
                  </>
                )}
                <th><span className="sr-only">Compute</span></th>
              </tr>
            </thead>
            <tbody>
              {agents.length === 0 ? (
                <tr>
                  <td colSpan={colSpan} className="p-0">
                    <EmptyState>
                      {experimentFilter ? 'No agents in this experiment.' : 'No agents yet.'}
                    </EmptyState>
                  </td>
                </tr>
              ) : sortedAgents.map(a => {
                const isExpanded = expandedAgent === a.id
                const none = <span className="text-subtle">—</span>
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
                      {experimentFilter && (
                        <>
                          <td className={num} title="Its part of the budget released so far">
                            <Fetched data={quotas} error={quotasError}>
                              {qs => {
                                const quota = qs.find(q => q.agent_id === a.id)
                                return quota ? formatAccH(quota.share_accelerator_hours) : none
                              }}
                            </Fetched>
                          </td>
                          <td className={num}>
                            <Fetched data={results} error={resultsError}>
                              {() => {
                                const standing = ranking?.standings.find(st => st.agent_id === a.id)
                                return standing?.experiment_id ? (
                                  <TextLink href={jobPath(experimentFilter, standing.experiment_id)} onClick={e => e.stopPropagation()}>
                                    #{standing.rank} · {formatMetric(standing.best)}
                                  </TextLink>
                                ) : none
                              }}
                            </Fetched>
                          </td>
                          <td className="text-2xs">
                            <Fetched data={jobStats} error={jobStatsError}>
                              {s => {
                                const jobs = s.groups?.[a.id]
                                return jobs
                                  ? Object.entries(jobs.by_status).map(([status, n]) => `${n} ${status.toLowerCase()}`).join(' · ')
                                  : <span className="text-xs text-subtle">none</span>
                              }}
                            </Fetched>
                          </td>
                        </>
                      )}
                      <td className="whitespace-nowrap text-right text-xs text-subtle">{isExpanded ? 'Hide compute ▲' : 'Compute ▼'}</td>
                    </tr>
                    {isExpanded && <AgentQuotaRows agentId={a.id} experiments={shown.experiments} error={shown.error} colSpan={colSpan} />}
                  </Fragment>
                )
              })}
            </tbody>
          </Table>
          <Pagination page={page} pageSize={PAGE_SIZE} total={total} onPageChange={setPage} />
        </Section>
      )}
    </Page>
  )
}
