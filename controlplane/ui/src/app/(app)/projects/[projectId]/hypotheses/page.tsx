'use client'

import { Suspense } from 'react'
import type { ReactNode } from 'react'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { ArrowUpRight } from 'lucide-react'
import { HYPOTHESIS_STATUSES, fetchHypothesisStats, fetchPlatformExperimentResults, fetchPlatformExperimentsPage } from '@/lib/api'
import { statusColor } from '@/lib/colors'
import { useQueryParam } from '@/lib/query-state'
import { experimentPath, projectPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { PlatformExperimentStatus } from '@/types'
import type { HypothesisStats } from '@/lib/api'
import type { PlatformExperiment } from '@/types'
import { ExperimentHypotheses, HYPOTHESES_VIEW } from '@/components/hypotheses-workspace'
import { Badge } from '@/components/ui/badge'
import { ButtonLink } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { TextLink } from '@/components/ui/link'
import { Page } from '@/components/ui/page'
import { Segmented } from '@/components/ui/segmented'
import { Inline, Stack } from '@/components/ui/stack'
import { Heading, Text } from '@/components/ui/text'
import { ErrorMessage, Loading } from '@/components/ui/status-message'

// The experiments a project holds are few; past this many the newest are listed and the rest named.
const EXPERIMENTS_LISTED = 50
const VIEWS = [{ value: 'outline', label: 'Outline' }, { value: 'graph', label: 'Graph' }] as const

/**
 * The experiment with the most recent activity: an open or running one, the latest started first
 * as the list comes, else the one that closed last.
 */
function mostRecentlyActive(experiments: PlatformExperiment[]): PlatformExperiment {
  const live = experiments.find(pe => pe.status === PlatformExperimentStatus.OPEN || pe.status === PlatformExperimentStatus.RUNNING)
  if (live) return live
  return experiments.reduce((last, pe) => (pe.closed_at ?? '') > (last.closed_at ?? '') ? pe : last)
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
 * by status and its best result against its baseline; beside them the chosen one's ideas as an
 * outline or a graph. The URL keeps the experiment and the view.
 */
function ProjectHypotheses() {
  const { project } = useTenant()
  const [chosen, setChosen] = useQueryParam('experiment')
  const [view, setView] = useQueryParam('view', HYPOTHESES_VIEW)
  const { data, error } = useSWR(
    ['platform-experiments', project.id, 'hypotheses-scope'],
    () => fetchPlatformExperimentsPage({ project_id: project.id, sort: '-starts_at', limit: EXPERIMENTS_LISTED }),
    { refreshInterval: 15_000, keepPreviousData: true },
  )
  const experiments = (data?.items ?? []).filter(pe => pe.status !== PlatformExperimentStatus.DRAFT)
  const selected = experiments.length === 0 ? undefined : chosen ? experiments.find(pe => pe.id === chosen) : mostRecentlyActive(experiments)

  return (
    <Page
      fill={!!selected}
      title="Hypotheses"
      actions={experiments.length > 0 && <Segmented label="View" size="sm" options={[...VIEWS]} value={view} onChange={setView} />}
    >
      {error && <ErrorMessage>Could not load the experiments of {project.name}: {error.message}</ErrorMessage>}
      {!data && !error && <Loading />}
      {data && experiments.length === 0 && (
        <EmptyState
          framed
          action={<ButtonLink href={projectPath(project.id, 'experiments')} variant="primary">Create an experiment</ButtonLink>}
        >
          No experiments yet.
        </EmptyState>
      )}
      {experiments.length > 0 && (
        <div className="grid flex-fill gap-5 xl:grid-cols-scope-rail">
          <ScopeRail experiments={experiments} total={data!.total} selected={selected?.id} onSelect={setChosen} />
          <Stack gap="lg" className="min-w-0">
            {selected ? (
              <>
                <ExperimentHeader pe={selected} />
                <ExperimentHypotheses key={selected.id} peID={selected.id} embedded />
              </>
            ) : (
              <ErrorMessage>Experiment {chosen} is not among the experiments of {project.name} listed here. Choose one beside.</ErrorMessage>
            )}
          </Stack>
        </div>
      )}
    </Page>
  )
}

function ScopeRail({ experiments, total, selected, onSelect }: {
  experiments: PlatformExperiment[]
  total: number
  selected?: string
  onSelect: (id: string) => void
}) {
  return (
    <nav aria-label="Experiments" className="min-w-0">
      <ul className="flex list-none gap-2 overflow-x-auto pb-1 xl:flex-col xl:overflow-visible xl:pb-0">
        {experiments.map(pe => (
          <li key={pe.id} className="w-60 flex-none xl:w-auto">
            <ExperimentScope pe={pe} selected={selected === pe.id} onClick={() => onSelect(pe.id)} />
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
