'use client'

import { Suspense, useState } from 'react'
import type { ReactNode } from 'react'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { ArrowUpRight } from 'lucide-react'
import {
  HYPOTHESIS_STATUSES, fetchHypothesesPage, fetchHypothesisStats, fetchPlatformExperimentResults, fetchPlatformExperimentsPage,
} from '@/lib/api'
import { statusColor } from '@/lib/colors'
import { relTime } from '@/lib/format'
import { useQueryParam } from '@/lib/query-state'
import { experimentPath, hypothesisPath, projectPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { PlatformExperimentStatus } from '@/types'
import type { HypothesisStats } from '@/lib/api'
import type { HypothesisGraphNode, HypothesisStatus, PlatformExperiment } from '@/types'
import { actorLabel } from '@/components/actor'
import { ExperimentHypotheses } from '@/components/hypotheses-workspace'
import { Badge } from '@/components/ui/badge'
import { ButtonLink, Chip } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { Input } from '@/components/ui/input'
import { TextLink } from '@/components/ui/link'
import { Page } from '@/components/ui/page'
import { Pagination } from '@/components/ui/pagination'
import { Segmented } from '@/components/ui/segmented'
import { Inline, Stack } from '@/components/ui/stack'
import { LinkTr, RowAnchor, Table, num, quiet } from '@/components/ui/table'
import { Eyebrow, Heading, Text } from '@/components/ui/text'
import { ErrorMessage, Loading } from '@/components/ui/status-message'

// The experiments a project holds are few; past this many the newest are listed and the rest named.
const EXPERIMENTS_LISTED = 50
const HYPOTHESES_PAGE_SIZE = 25
const VIEWS = [{ value: 'list', label: 'List' }, { value: 'graph', label: 'Graph' }, { value: 'outline', label: 'Outline' }] as const

const capitalised = (word: string) => word.charAt(0).toUpperCase() + word.slice(1)

/** The owner of a hypothesis by name: its agent, or the person who registered one no agent owns. */
function ownerLabel(h: Pick<HypothesisGraphNode, 'agent_id' | 'actor'>): string {
  const own = h.actor.kind === 'agent' && h.actor.id === h.agent_id && h.actor.name
  return actorLabel(h.actor, h.agent_id, own || h.agent_id)
}

function lastActivity(h: HypothesisGraphNode): string {
  return h.status_changed_at && h.status_changed_at > h.created_at ? h.status_changed_at : h.created_at
}

interface Standing {
  metric: string
  direction: 'maximize' | 'minimize'
  best?: number
  baseline?: number
}

/** The experiment's primary metric with its best result and the baseline's, from its standings. */
function useStanding(pe: PlatformExperiment | undefined): Standing | undefined {
  const asked = pe && pe.status !== PlatformExperimentStatus.DRAFT
  const { data } = useSWR(asked ? ['pe-results', pe.id] : null, () => fetchPlatformExperimentResults(pe!.id), { refreshInterval: 15_000 })
  const primary = data?.metrics[0]
  if (!primary) return undefined
  return { metric: primary.metric, direction: primary.direction, best: primary.standings[0]?.best, baseline: primary.baseline }
}

function useStats(pe: PlatformExperiment) {
  return useSWR(['hypothesis-stats', pe.id], () => fetchHypothesisStats(pe.id), { refreshInterval: 15_000 })
}

/** How far value is from the baseline, signed so that better reads as positive in the metric's direction. */
function Delta({ value, standing }: { value: number; standing?: Standing }) {
  if (standing?.baseline === undefined) return null
  const diff = value - standing.baseline
  const better = standing.direction === 'maximize' ? diff > 0 : diff < 0
  const tone = diff === 0 ? 'text-subtle' : better ? 'text-success' : 'text-danger'
  return (
    <span className={clsx('font-mono text-2xs tabular-nums', tone)} title={`Baseline ${standing.metric} ${standing.baseline.toFixed(4)}`}>
      {diff >= 0 ? '+' : '−'}{Math.abs(diff).toFixed(4)}
    </span>
  )
}

/** The hypotheses of an experiment by status, as one bar whose segments are each status's share. */
function StatusBar({ stats }: { stats?: HypothesisStats }) {
  const total = stats?.total ?? 0
  return (
    <div className="flex h-1.5 w-full overflow-hidden rounded-full bg-surface-inset" aria-hidden="true">
      {total > 0 && HYPOTHESIS_STATUSES.map(s => {
        const n = stats?.by_status[s] ?? 0
        return n > 0 ? <span key={s} style={{ width: `${(n / total) * 100}%`, background: statusColor(s) }} /> : null
      })}
    </div>
  )
}

function StatusCounts({ stats }: { stats?: HypothesisStats }) {
  return (
    <Inline gap="md" className="text-xs text-muted">
      {HYPOTHESIS_STATUSES.map(s => (
        <span key={s} className="inline-flex items-center gap-1.5 tabular-nums">
          <span className="inline-block size-2 rounded-full" style={{ background: statusColor(s) }} />
          {stats?.by_status[s] ?? 0} {s}
        </span>
      ))}
    </Inline>
  )
}

function BestLine({ standing }: { standing?: Standing }) {
  if (!standing) return null
  if (standing.best === undefined) {
    return (
      <Text as="span" variant="meta" tone="subtle">
        {standing.baseline !== undefined ? `baseline ${standing.metric} ${standing.baseline.toFixed(4)}, no result yet` : 'no result yet'}
      </Text>
    )
  }
  return (
    <span className="inline-flex flex-wrap items-baseline gap-x-1.5 text-xs text-muted">
      best <span className="font-mono tabular-nums text-foreground">{standing.best.toFixed(4)}</span>
      <Delta value={standing.best} standing={standing} />
      {standing.baseline !== undefined && <span>vs baseline <span className="font-mono tabular-nums">{standing.baseline.toFixed(4)}</span></span>}
    </span>
  )
}

export default function ProjectHypothesesPage() {
  return (
    <Suspense fallback={<Loading />}>
      <ProjectHypotheses />
    </Suspense>
  )
}

/**
 * The project's home for hypotheses: its experiments down the side, each with its state, its ideas
 * by status and its best result against its baseline, picking the scope; beside them the scope's
 * ideas as a list, or one experiment's as its graph or outline. The URL keeps scope and view.
 */
function ProjectHypotheses() {
  const { project } = useTenant()
  const [scope, setScope] = useQueryParam('experiment')
  const [view, setView] = useQueryParam('view', 'list')
  const { data, error } = useSWR(
    ['platform-experiments', project.id, 'hypotheses-scope'],
    () => fetchPlatformExperimentsPage({ project_id: project.id, sort: '-starts_at', limit: EXPERIMENTS_LISTED }),
    { refreshInterval: 15_000, keepPreviousData: true },
  )
  const experiments = (data?.items ?? []).filter(pe => pe.status !== PlatformExperimentStatus.DRAFT)
  const scoped = experiments.find(pe => pe.id === scope)
  const workspace = view !== 'list' && scoped

  return (
    <Page
      fill={!!workspace}
      title="Hypotheses"
      description={`What agents and people claim and test across the experiments of ${project.name}. Pick an experiment to see only its ideas, or its graph.`}
      actions={experiments.length > 0 && <Segmented label="View" size="sm" options={[...VIEWS]} value={view} onChange={setView} />}
    >
      {error && <ErrorMessage>Could not load the experiments of {project.name}: {error.message}</ErrorMessage>}
      {!data && !error && <Loading />}
      {data && experiments.length === 0 && (
        <EmptyState
          framed
          action={<ButtonLink href={projectPath(project.id, 'experiments')} variant="primary">Create an experiment</ButtonLink>}
        >
          No experiments yet. Hypotheses grow inside one: start an experiment and its agents register them as they work.
        </EmptyState>
      )}
      {experiments.length > 0 && (
        <div className="grid flex-fill gap-5 xl:grid-cols-scope-rail">
          <ScopeRail
            experiments={experiments}
            total={data!.total}
            scope={scoped?.id ?? ''}
            onScope={id => setScope(id)}
          />
          <Stack gap="lg" className="min-w-0">
            {scoped && <ExperimentHeader pe={scoped} />}
            {view === 'list' && <HypothesesList experiments={experiments} scoped={scoped} />}
            {view !== 'list' && !scoped && <PickExperiment experiments={experiments} onScope={setScope} />}
            {workspace && <ExperimentHypotheses key={scoped.id} peID={scoped.id} defaultView="list" embedded />}
          </Stack>
        </div>
      )}
    </Page>
  )
}

function ScopeRail({ experiments, total, scope, onScope }: {
  experiments: PlatformExperiment[]
  total: number
  scope: string
  onScope: (id: string) => void
}) {
  return (
    <nav aria-label="Experiments" className="min-w-0">
      <ul className="flex list-none gap-2 overflow-x-auto pb-1 xl:flex-col xl:overflow-visible xl:pb-0">
        <li className="w-60 flex-none xl:w-auto">
          <ScopeButton selected={scope === ''} onClick={() => onScope('')}>
            <Inline gap="sm" nowrap>
              <span className="min-w-0 flex-1 truncate font-medium">All experiments</span>
              <Text as="span" variant="meta" tone="subtle" className="tabular-nums">{experiments.length}</Text>
            </Inline>
            <Text as="span" variant="meta" tone="subtle">Every idea of the project, newest activity first</Text>
          </ScopeButton>
        </li>
        {experiments.map(pe => (
          <li key={pe.id} className="w-60 flex-none xl:w-auto">
            <ExperimentScope pe={pe} selected={scope === pe.id} onClick={() => onScope(pe.id)} />
          </li>
        ))}
      </ul>
      {total > experiments.length && (
        <Text variant="hint" className="mt-2">The newest {experiments.length} of {total} experiments.</Text>
      )}
    </nav>
  )
}

function ScopeButton({ selected, onClick, children }: { selected: boolean; onClick: () => void; children: ReactNode }) {
  return (
    <button
      type="button"
      aria-current={selected || undefined}
      onClick={onClick}
      className={clsx(
        'flex w-full flex-col gap-1.5 rounded-lg border px-3 py-2.5 text-left text-sm transition-colors',
        selected ? 'border-accent bg-selected' : 'border-border bg-surface hover:bg-hover',
      )}
    >
      {children}
    </button>
  )
}

function ExperimentScope({ pe, selected, onClick }: { pe: PlatformExperiment; selected: boolean; onClick: () => void }) {
  const { data: stats, error } = useStats(pe)
  const standing = useStanding(pe)
  return (
    <ScopeButton selected={selected} onClick={onClick}>
      <Inline gap="sm" nowrap>
        <span className="min-w-0 flex-1 truncate font-medium" title={pe.name}>{pe.name}</span>
        <Badge status={pe.status} className="flex-none">{pe.status}</Badge>
      </Inline>
      <StatusBar stats={stats} />
      <Inline gap="sm" nowrap className="text-xs text-muted">
        <span className="tabular-nums" title={error?.message}>
          {error ? '?' : stats ? `${stats.total} ${stats.total === 1 ? 'idea' : 'ideas'}` : '…'}
        </span>
        {standing?.best !== undefined && (
          <span className="ml-auto inline-flex items-baseline gap-1.5">
            <span className="font-mono tabular-nums text-foreground" title={`Best ${standing.metric}`}>{standing.best.toFixed(4)}</span>
            <Delta value={standing.best} standing={standing} />
          </span>
        )}
      </Inline>
    </ScopeButton>
  )
}

function ExperimentHeader({ pe }: { pe: PlatformExperiment }) {
  const { data: stats } = useStats(pe)
  const standing = useStanding(pe)
  return (
    <Card className="px-5 py-4">
      <Stack gap="sm">
        <Inline gap="md">
          <Heading level={2} className="min-w-0 break-words">{pe.name}</Heading>
          <Badge status={pe.status}>{pe.status}</Badge>
          {pe.shared && <Badge tone="neutral">shared</Badge>}
          <TextLink href={experimentPath(pe.id)} className="ml-auto inline-flex items-center gap-1 text-sm">
            Open experiment <ArrowUpRight size={14} />
          </TextLink>
        </Inline>
        <Inline gap="lg">
          <Text as="span" variant="meta" className="tabular-nums">{stats ? `${stats.total} ideas` : '…'}</Text>
          <StatusCounts stats={stats} />
          <span className="ml-auto"><BestLine standing={standing} /></span>
        </Inline>
        <StatusBar stats={stats} />
      </Stack>
    </Card>
  )
}

function PickExperiment({ experiments, onScope }: { experiments: PlatformExperiment[]; onScope: (id: string) => void }) {
  return (
    <EmptyState framed>
      <Stack gap="md" className="items-center">
        <span>A graph belongs to one experiment. Pick the one to draw:</span>
        <Inline gap="sm" className="justify-center">
          {experiments.map(pe => <Chip key={pe.id} onClick={() => onScope(pe.id)}>{pe.name}</Chip>)}
        </Inline>
      </Stack>
    </EmptyState>
  )
}

function HypothesesList({ experiments, scoped }: { experiments: PlatformExperiment[]; scoped?: PlatformExperiment }) {
  const { project } = useTenant()
  const [status, setStatus] = useQueryParam('status')
  const [search, setSearch] = useQueryParam('q')
  const [page, setPage] = useState(0)
  const byID = new Map(experiments.map(pe => [pe.id, pe]))

  const { data, error, isLoading } = useSWR(
    ['hypotheses', project.id, scoped?.id ?? '', status, search, page],
    () => fetchHypothesesPage({
      // A shared experiment of another org lies outside the project, so it is asked for by itself.
      project_id: scoped && scoped.project_id !== project.id ? undefined : project.id,
      platform_experiment_id: scoped?.id,
      status: (status || undefined) as HypothesisStatus | undefined,
      q: search || undefined,
      limit: HYPOTHESES_PAGE_SIZE,
      offset: page * HYPOTHESES_PAGE_SIZE,
    }),
    { refreshInterval: 15_000 },
  )
  const rows = data?.items ?? []
  const filtered = !!(status || search)

  return (
    <Card>
      <div className="border-b px-5 py-3">
        <Inline gap="md">
          <Segmented
            label="Status"
            size="sm"
            options={['', ...HYPOTHESIS_STATUSES].map(s => ({ value: s, label: s ? capitalised(s) : 'All' }))}
            value={status}
            onChange={s => { setStatus(s); setPage(0) }}
          />
          {data && <Text as="span" variant="hint" className="tabular-nums">{data.total} {data.total === 1 ? 'idea' : 'ideas'}</Text>}
          <Input
            fieldSize="sm"
            className="ml-auto w-64"
            aria-label="Search hypotheses"
            value={search}
            onChange={e => { setSearch(e.target.value); setPage(0) }}
            placeholder={scoped ? `Search the claims of ${scoped.name}` : 'Search every claim'}
          />
        </Inline>
      </div>
      {error && <ErrorMessage className="m-5">Could not load the hypotheses: {error.message}</ErrorMessage>}
      {isLoading && <Loading />}
      {data && (
        <div className="overflow-x-auto">
          <Table className="min-w-xl table-fixed">
            <thead>
              <tr>
                <th>Claim</th>
                <th className="w-28">Status</th>
                <th className={`w-28 ${num}`}>Best result</th>
                <th className="hidden w-36 xl:table-cell">Proposed by</th>
                <th className="w-24">Activity</th>
              </tr>
            </thead>
            <tbody>
              {rows.length === 0 ? (
                <tr>
                  <td colSpan={5}>
                    <EmptyState>
                      {filtered
                        ? `No hypotheses match${search ? ` "${search}"` : ' these filters'}${scoped ? ` in ${scoped.name}` : ''}.`
                        : scoped
                          ? `No hypotheses in ${scoped.name} yet. Its agents register them as they start working, and you can add one from its graph.`
                          : 'No hypotheses yet. Agents register them as they start working.'}
                    </EmptyState>
                  </td>
                </tr>
              ) : rows.map(h => (
                <HypothesisRow key={h.id} h={h} pe={byID.get(h.platform_experiment_id)} showExperiment={!scoped} />
              ))}
            </tbody>
          </Table>
        </div>
      )}
      <Pagination page={page} pageSize={HYPOTHESES_PAGE_SIZE} total={data?.total ?? 0} onPageChange={setPage} />
    </Card>
  )
}

function HypothesisRow({ h, pe, showExperiment }: { h: HypothesisGraphNode; pe?: PlatformExperiment; showExperiment: boolean }) {
  const standing = useStanding(pe)
  const best = h.best_job?.metric
  return (
    <LinkTr href={hypothesisPath(h.platform_experiment_id, h.id)}>
      <td className="break-words">
        <Stack gap="2xs">
          <RowAnchor className="line-clamp-2" title={h.text}>{h.text}</RowAnchor>
          {/* Where it lives, and below the widest screens who proposed it, whose column is left out there. */}
          <Inline gap="xs" nowrap className={clsx('text-xs text-muted', !showExperiment && 'xl:hidden')}>
            {showExperiment && (
              <>
                <Eyebrow className="flex-none">In</Eyebrow>
                <span className="min-w-0 truncate">{pe?.name ?? h.platform_experiment_id}</span>
                {pe && pe.status !== PlatformExperimentStatus.RUNNING && <span className="flex-none text-subtle">· {pe.status}</span>}
              </>
            )}
            <span className="min-w-0 truncate text-subtle xl:hidden">{showExperiment && '· '}by {ownerLabel(h)}</span>
          </Inline>
        </Stack>
      </td>
      <td><Badge status={h.status}>{h.status}</Badge></td>
      <td className={num} title={best ? `Best ${best.name} of its jobs` : 'No job of it reported a result yet'}>
        {best ? (
          <Stack gap="2xs" className="items-end">
            <span className="text-foreground">{best.value.toFixed(4)}</span>
            <Delta value={best.value} standing={standing} />
          </Stack>
        ) : <span className="text-subtle">–</span>}
      </td>
      <td className="hidden truncate text-xs xl:table-cell" title={ownerLabel(h)}>{ownerLabel(h)}</td>
      <td className={quiet}>{relTime(lastActivity(h))}</td>
    </LinkTr>
  )
}
