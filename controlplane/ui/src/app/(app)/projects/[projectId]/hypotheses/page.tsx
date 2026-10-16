'use client'

import { Suspense, useState } from 'react'
import useSWR from 'swr'
import { fetchHypothesesPage, fetchHypothesisStats, fetchPlatformExperimentsPage, HYPOTHESIS_STATUSES } from '@/lib/api'
import { graphHref } from '@/lib/hypothesis-graph'
import { useQueryParam } from '@/lib/query-state'
import { listProject, useExperimentAccess } from '@/lib/read-only'
import { hypothesisPath, projectPath } from '@/lib/routes'
import { relTime } from '@/lib/format'
import { useTenant } from '@/lib/tenant'
import type { HypothesisListItem, HypothesisStatus, PlatformExperiment } from '@/types'
import { actorLabel } from '@/components/actor'
import { PlatformExperimentName } from '@/components/platform-experiment-name'
import { PlatformExperimentSelect } from '@/components/platform-experiment-select'
import { Badge } from '@/components/ui/badge'
import { ButtonLink } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { FormField } from '@/components/ui/form-field'
import { Input } from '@/components/ui/input'
import { TextLink } from '@/components/ui/link'
import { Page } from '@/components/ui/page'
import { Pagination } from '@/components/ui/pagination'
import { Section } from '@/components/ui/section'
import { Segmented } from '@/components/ui/segmented'
import { Inline } from '@/components/ui/stack'
import { LinkTr, Table, num, quiet } from '@/components/ui/table'
import { ErrorMessage, Loading } from '@/components/ui/status-message'

const EXPERIMENTS_PAGE_SIZE = 10
const HYPOTHESES_PAGE_SIZE = 25

const capitalised = (word: string) => word.charAt(0).toUpperCase() + word.slice(1)

/** The owner of a hypothesis by name: its agent, or the person who registered one no agent owns. */
function ownerLabel(h: Pick<HypothesisListItem, 'agent_id' | 'actor'>): string {
  const own = h.actor.kind === 'agent' && h.actor.id === h.agent_id && h.actor.name
  return actorLabel(h.actor, h.agent_id, own || h.agent_id)
}

function lastActivity(h: HypothesisListItem): string {
  return h.status_changed_at && h.status_changed_at > h.created_at ? h.status_changed_at : h.created_at
}

export default function ProjectHypothesesPage() {
  return (
    <Suspense fallback={<Loading />}>
      <ProjectHypotheses />
    </Suspense>
  )
}

/**
 * The hypotheses of every experiment in the project: each experiment with its count by status and
 * a way to its graph, then every hypothesis in one table, paged and filtered on the server.
 */
function ProjectHypotheses() {
  const { project } = useTenant()
  const [page, setPage] = useState(0)
  const { data, error } = useSWR(
    ['platform-experiments', project.id, 'hypotheses-overview', page],
    () => fetchPlatformExperimentsPage({ project_id: project.id, sort: '-starts_at', limit: EXPERIMENTS_PAGE_SIZE, offset: page * EXPERIMENTS_PAGE_SIZE }),
    { refreshInterval: 15_000, keepPreviousData: true },
  )

  return (
    <Page title="Hypotheses" description={`The claims agents and people test in the experiments of ${project.name}. Each grows in its experiment's graph.`}>
      {error && <ErrorMessage>Could not load the experiments of {project.name}: {error.message}</ErrorMessage>}
      {!data && !error && <Loading />}
      {data && data.total === 0 && (
        <EmptyState
          framed
          action={<ButtonLink href={projectPath(project.id, 'experiments')} variant="primary">Create an experiment</ButtonLink>}
        >
          No experiments yet. Hypotheses grow inside one: start an experiment and its agents register them as they work.
        </EmptyState>
      )}
      {data && data.total > 0 && (
        <>
          <Section title="Experiments" note={data.total} scrollX>
            <Table className="min-w-3xl">
              <thead>
                <tr>
                  <th>Experiment</th>
                  <th className="w-24">Status</th>
                  <th className={`w-20 ${num}`}>Total</th>
                  {HYPOTHESIS_STATUSES.map(s => <th key={s} className={`w-24 ${num}`}>{capitalised(s)}</th>)}
                  <th className="w-16"></th>
                </tr>
              </thead>
              <tbody>
                {data.items.map(pe => <ExperimentRow key={pe.id} pe={pe} />)}
              </tbody>
            </Table>
            <Pagination page={page} pageSize={EXPERIMENTS_PAGE_SIZE} total={data.total} onPageChange={setPage} />
          </Section>
          <HypothesesTable />
        </>
      )}
    </Page>
  )
}

function ExperimentRow({ pe }: { pe: PlatformExperiment }) {
  const { data: stats, error } = useSWR(['hypothesis-stats', pe.id], () => fetchHypothesisStats(pe.id), { refreshInterval: 15_000 })
  const count = (n: number | undefined) => error ? '?' : stats ? n ?? 0 : '…'
  return (
    <tr>
      <td className="break-words">
        <TextLink href={graphHref(pe.id)}>{pe.name}</TextLink>
        {pe.shared && <Badge tone="neutral" className="ml-2">shared</Badge>}
      </td>
      <td><Badge status={pe.status}>{pe.status}</Badge></td>
      <td className={num} title={error?.message}>{count(stats?.total)}</td>
      {HYPOTHESIS_STATUSES.map(s => <td key={s} className={num}>{count(stats?.by_status[s])}</td>)}
      <td><TextLink href={graphHref(pe.id)} aria-label={`Graph of ${pe.name}`}>Graph</TextLink></td>
    </tr>
  )
}

function HypothesesTable() {
  const { project } = useTenant()
  const [experiment, setExperiment] = useQueryParam('experiment')
  const [status, setStatus] = useQueryParam('status')
  const [search, setSearch] = useQueryParam('q')
  const [page, setPage] = useState(0)
  // A shared experiment of another org lies outside the project, so it is asked for by itself.
  const access = useExperimentAccess(experiment || undefined)
  const scope = listProject(project, access)

  const { data, error, isLoading } = useSWR(
    access.known ? ['hypotheses', scope.project_id ?? 'shared', experiment, status, search, page] : null,
    () => fetchHypothesesPage({
      ...scope,
      platform_experiment_id: experiment || undefined,
      status: (status || undefined) as HypothesisStatus | undefined,
      q: search || undefined,
      limit: HYPOTHESES_PAGE_SIZE,
      offset: page * HYPOTHESES_PAGE_SIZE,
    }),
    { refreshInterval: 15_000, keepPreviousData: true },
  )
  const rows = data?.items ?? []
  const filtered = !!(experiment || status || search)

  return (
    <Section title="All hypotheses" note={data?.total} scrollX>
      <div className="border-b px-5 py-3">
        <Inline gap="md">
          <FormField inline label="Experiment">
            <PlatformExperimentSelect value={experiment} onChange={v => { setExperiment(v); setPage(0) }} allLabel="All experiments" />
          </FormField>
          <Segmented
            label="Status"
            size="sm"
            options={['', ...HYPOTHESIS_STATUSES].map(s => ({ value: s, label: s ? capitalised(s) : 'All' }))}
            value={status}
            onChange={s => { setStatus(s); setPage(0) }}
          />
          <Input
            fieldSize="sm"
            className="ml-auto w-56"
            aria-label="Search hypotheses"
            value={search}
            onChange={e => { setSearch(e.target.value); setPage(0) }}
            placeholder="Search claims"
          />
        </Inline>
      </div>
      {error && <ErrorMessage className="m-5">Could not load the hypotheses: {error.message}</ErrorMessage>}
      {isLoading && <Loading />}
      {data && (
        <Table className="min-w-4xl table-fixed">
          <thead>
            <tr>
              <th>Claim</th>
              {!experiment && <th className="w-56">Experiment</th>}
              <th className="w-28">Status</th>
              <th className="w-44">Owner</th>
              <th className="w-28">Last activity</th>
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 ? (
              <tr>
                <td colSpan={experiment ? 4 : 5}>
                  <EmptyState>
                    {filtered ? `No hypotheses match${search ? ` "${search}"` : ' these filters'}.` : 'No hypotheses yet. Agents register them as they start working.'}
                  </EmptyState>
                </td>
              </tr>
            ) : rows.map(h => (
              <LinkTr key={h.id} href={hypothesisPath(h.platform_experiment_id, h.id)}>
                <td className="break-words">{h.text}</td>
                {!experiment && (
                  <td className="break-words text-xs"><PlatformExperimentName id={h.platform_experiment_id} /></td>
                )}
                <td><Badge status={h.status}>{h.status}</Badge></td>
                <td className="truncate text-xs" title={ownerLabel(h)}>{ownerLabel(h)}</td>
                <td className={quiet}>{relTime(lastActivity(h))}</td>
              </LinkTr>
            ))}
          </tbody>
        </Table>
      )}
      <Pagination page={page} pageSize={HYPOTHESES_PAGE_SIZE} total={data?.total ?? 0} onPageChange={setPage} />
    </Section>
  )
}
