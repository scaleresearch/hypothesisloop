'use client'

import { Suspense, useId, useRef, useState } from 'react'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { clustersInUse } from '@/lib/clusters'
import { semantic } from '@/lib/colors'
import { useProjectRouter, useQueryParam } from '@/lib/query-state'
import { useTenant } from '@/lib/tenant'
import { fetchAgentsPage, fetchExperimentStats, fetchOrgClusters, fetchPlatformExperimentsPage } from '@/lib/api'
import type { PlatformExperiment } from '@/types'
import { GoalInput } from '@/components/goal-input'
import { PageHeader } from '@/components/ui/page-header'
import { PlatformExperimentDialog } from '@/components/platform-experiment-form'
import { PlatformExperimentCard } from '@/components/platform-experiment-card'
import { Dot } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Chip } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { TextLink } from '@/components/ui/link'
import { Select } from '@/components/ui/select'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { Pagination } from '@/components/ui/pagination'

const PAGE_SIZE = 10

const SORT_OPTIONS = [
  { value: '-starts_at', label: 'Newest first' },
  { value: 'starts_at', label: 'Oldest first' },
  { value: 'name', label: 'Name (A–Z)' },
  { value: '-name', label: 'Name (Z–A)' },
]

function LiveStrip() {
  const { project } = useTenant()
  // Both counts are server-side totals, not the length of a fetched page — see
  // fetchExperimentStats. The agent read asks for a single row purely to carry X-Total-Count.
  const { data: stats, error: statsError } = useSWR(['experiment-stats', project.id], () => fetchExperimentStats({ project_id: project.id }), { refreshInterval: 15_000 })
  const { data: agents, error: agentsError } = useSWR(['agent-count', project.id], () => fetchAgentsPage({ project_id: project.id, limit: 1 }), { refreshInterval: 30_000 })

  // A count that failed to load says so, and one still loading has no colour to mislead with.
  function item(label: string, error: Error | undefined, value: string | undefined, color: string) {
    if (error) return { label, value: 'unknown', color: semantic.danger, title: error.message }
    if (value === undefined) return { label, value: '…', color: semantic.subtle, title: 'Loading' }
    return { label, value, color, title: undefined }
  }
  const items = [
    // A status no job holds is absent from the answer.
    item('Jobs running', statsError, stats && String(stats.by_status?.RUNNING ?? 0), semantic.success),
    item('Agents', agentsError, agents && String(agents.total), semantic.neutral),
  ]

  return (
    <div className="mb-6 flex flex-wrap gap-5">
      {items.map(item => (
        <div key={item.label} title={item.title} className="flex items-center gap-2">
          <Dot color={item.color} />
          <span className="font-mono text-sm font-medium text-foreground">{item.value}</span>
          <span className="text-xs text-subtle">{item.label}</span>
        </div>
      ))}
    </div>
  )
}

/** One of the two ways to a new experiment, side by side and alike. */
function WayIn({ pressed, title, line, onClick }: { pressed: boolean; title: string; line: string; onClick: () => void }) {
  const id = useId()
  return (
    <button
      type="button"
      aria-pressed={pressed}
      aria-labelledby={`${id}-title`}
      aria-describedby={`${id}-line`}
      onClick={onClick}
      className={clsx(
        'grid gap-0.5 rounded border px-3 py-2 text-left transition-colors',
        pressed ? 'border-primary bg-primary/10' : 'border-border-control hover:bg-foreground/5',
      )}
    >
      <span id={`${id}-title`} className="text-sm font-semibold text-foreground">{title}</span>
      <span id={`${id}-line`} className="text-xs text-muted">{line}</span>
    </button>
  )
}

export default function PlatformExperimentsPage() {
  return (
    <Suspense fallback={<Loading />}>
      <PlatformExperimentsPageContent />
    </Suspense>
  )
}

/**
 * The experiments of the selected project and two ways to a new one: a sentence the platform
 * plans, or the full form, which opens in a dialog as it does for an edit. The experiments the
 * platform shares are listed too, read only.
 */
function PlatformExperimentsPageContent() {
  const { org, project } = useTenant()
  const router = useProjectRouter()
  const [statusFilter, setStatusFilter] = useQueryParam('status')
  const [search, setSearch] = useQueryParam('q')
  const [sort, setSort] = useQueryParam('sort', '-starts_at')
  const [page, setPage] = useState(0)
  const [dialog, setDialog] = useState<{ editing: PlatformExperiment | null } | null>(null)
  const configuring = dialog !== null && dialog.editing === null
  const planning = useRef<HTMLDivElement>(null)

  const { data, error, isLoading, mutate } = useSWR(
    ['platform-experiments', project.id, statusFilter, search, sort, page],
    () => fetchPlatformExperimentsPage({
      project_id: project.id,
      status: statusFilter || undefined,
      q: search || undefined,
      sort,
      limit: PAGE_SIZE,
      offset: page * PAGE_SIZE,
    }),
    { refreshInterval: 10_000, keepPreviousData: true },
  )
  // The sidebar reads the same key, so this is one request for both.
  const { data: clusters } = useSWR(['org-clusters', org.id], () => fetchOrgClusters(org.id), { refreshInterval: 10_000 })

  // A new experiment opens on its own page: a draft to be planned, or one to add agents to.
  const open = (pe: PlatformExperiment) => router.push(`/platform-experiments/${pe.id}`)
  const experiments = data?.items ?? []
  const total = data?.total ?? 0
  const statuses = ['', 'open', 'running', 'draft', 'closed']

  return (
    <div>
      {dialog && (
        <PlatformExperimentDialog
          initial={dialog.editing}
          onClose={() => setDialog(null)}
          // An edit stays here and rereads the list. A new experiment, or one the form also ran, opens.
          onSaved={(saved, run) => (dialog.editing && !run ? void mutate() : open(saved))}
        />
      )}

      <PageHeader title="Experiments" />
      <LiveStrip />

      <Card className="mb-6 max-w-[720px]">
        <CardHeader><CardTitle>New experiment in {org.name} / {project.name}</CardTitle></CardHeader>
        <CardContent className="grid gap-4">
          {clusters && clustersInUse(clusters).online === 0 && (
            <p className="text-sm text-warning">
              No compute is online yet. <TextLink inline href="/cluster">Compute resources</TextLink>
            </p>
          )}
          <div role="group" aria-label="How to start" className="grid gap-2 sm:grid-cols-2">
            <WayIn pressed={!configuring} title="Let us plan it" line="Say what to find out. We propose, you approve." onClick={() => planning.current?.querySelector('textarea')?.focus()} />
            <WayIn pressed={configuring} title="Configure it yourself" line="Set the metrics, budget and agents in a form." onClick={() => setDialog({ editing: null })} />
          </div>
          <div ref={planning}>
            <GoalInput projectID={project.id} onCreated={open} />
          </div>
        </CardContent>
      </Card>

      <div className="mb-4 flex flex-wrap items-center gap-3">
        <div className="flex flex-wrap gap-1.5">
          {statuses.map(s => (
            <Chip key={s} active={statusFilter === s} onClick={() => { setStatusFilter(s); setPage(0) }}>
              {s || 'All'}
            </Chip>
          ))}
        </div>

        <Input
          fieldSize="sm"
          mono
          className="min-w-[220px]"
          value={search}
          onChange={e => { setSearch(e.target.value); setPage(0) }}
          placeholder="Search name or description…"
        />

        <Select fieldSize="sm" aria-label="Sort" prefix="Sort" value={sort} onChange={v => { setSort(v); setPage(0) }} options={SORT_OPTIONS} />
      </div>

      {isLoading && <Loading />}
      {error && <ErrorMessage>Could not load the experiments of {project.name}: {error.message}</ErrorMessage>}

      {data && experiments.length === 0 && (
        <p className="text-sm text-muted">
          {statusFilter || search
            ? `No experiments in this project match${search ? ` "${search}"` : ' this filter'}.`
            : 'None yet. Write what you want to find out above and the platform proposes the first.'}
        </p>
      )}

      {experiments.map(pe => <PlatformExperimentCard key={pe.id} pe={pe} onEdit={editing => setDialog({ editing })} />)}

      <Pagination page={page} pageSize={PAGE_SIZE} total={total} onPageChange={setPage} />
    </div>
  )
}
