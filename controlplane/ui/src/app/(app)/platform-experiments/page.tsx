'use client'

import { Suspense, useRef, useState } from 'react'
import useSWR from 'swr'
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
import { Card, CardContent } from '@/components/ui/card'
import { Chip } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { TextLink } from '@/components/ui/link'
import { Select } from '@/components/ui/select'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { Pagination } from '@/components/ui/pagination'
import { Segmented } from '@/components/ui/tabs'

const PAGE_SIZE = 10

const SORT_OPTIONS = [
  { value: '-starts_at', label: 'Newest first' },
  { value: 'starts_at', label: 'Oldest first' },
  { value: 'name', label: 'Name (A–Z)' },
  { value: '-name', label: 'Name (Z–A)' },
]

const PLAN = 'Plan it for me'
const CONFIGURE = 'Configure it myself'

/** The project's running jobs and agents, each only while there are some. */
function LiveStrip() {
  const { project } = useTenant()
  // Both counts are server-side totals, not the length of a fetched page: see
  // fetchExperimentStats. The agent read asks for a single row purely to carry X-Total-Count.
  const { data: stats, error: statsError } = useSWR(['experiment-stats', project.id], () => fetchExperimentStats({ project_id: project.id }), { refreshInterval: 15_000 })
  const { data: agents, error: agentsError } = useSWR(['agent-count', project.id], () => fetchAgentsPage({ project_id: project.id, limit: 1 }), { refreshInterval: 30_000 })

  // A status no job holds is absent from the answer.
  const running = stats?.by_status?.RUNNING ?? 0
  const items = [
    statsError ? { key: 'jobs', text: 'Jobs running unknown', color: semantic.danger, title: statsError.message }
      : running > 0 && { key: 'jobs', text: `${running} ${running === 1 ? 'job' : 'jobs'} running`, color: semantic.success, title: undefined },
    agentsError ? { key: 'agents', text: 'Agents unknown', color: semantic.danger, title: agentsError.message }
      : (agents?.total ?? 0) > 0 && { key: 'agents', text: `${agents!.total} ${agents!.total === 1 ? 'agent' : 'agents'}`, color: semantic.neutral, title: undefined },
  ].filter(item => !!item)
  if (items.length === 0) return null

  return (
    <div className="-mt-2 mb-4 flex flex-wrap gap-4 text-xs tabular-nums text-muted">
      {items.map(item => (
        <span key={item.key} title={item.title} className="flex items-center gap-1.5">
          <Dot color={item.color} />
          {item.text}
        </span>
      ))}
    </div>
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
        <CardContent className="grid gap-3">
          <Segmented
            label="How to start"
            options={[{ value: 'plan', label: PLAN }, { value: 'configure', label: CONFIGURE }]}
            value={configuring ? 'configure' : 'plan'}
            onChange={way => way === 'configure' ? setDialog({ editing: null }) : planning.current?.querySelector('textarea')?.focus()}
          />
          <div ref={planning}>
            <GoalInput projectID={project.id} onCreated={open} />
          </div>
          {clusters && clustersInUse(clusters).online === 0 && (
            <p className="text-xs text-muted">
              No compute is online yet. <TextLink inline href="/cluster">Compute resources</TextLink>
            </p>
          )}
        </CardContent>
      </Card>

      <div className="mb-3 flex flex-wrap items-center gap-2">
        <div className="flex flex-wrap gap-1">
          {statuses.map(s => (
            <Chip key={s} active={statusFilter === s} onClick={() => { setStatusFilter(s); setPage(0) }}>
              {s || 'All'}
            </Chip>
          ))}
        </div>

        <Input
          fieldSize="sm"
          className="w-40"
          aria-label="Search"
          value={search}
          onChange={e => { setSearch(e.target.value); setPage(0) }}
          placeholder="Search"
        />

        <Select fieldSize="sm" aria-label="Sort" value={sort} onChange={v => { setSort(v); setPage(0) }} options={SORT_OPTIONS} />
      </div>

      {isLoading && <Loading />}
      {error && <ErrorMessage>Could not load the experiments of {project.name}: {error.message}</ErrorMessage>}

      {data && experiments.length === 0 && (
        <p className="text-sm text-muted">
          {statusFilter || search
            ? `No experiments match${search ? ` "${search}"` : ' this filter'}.`
            : 'No experiments yet.'}
        </p>
      )}

      {experiments.map(pe => <PlatformExperimentCard key={pe.id} pe={pe} onEdit={editing => setDialog({ editing })} />)}

      <Pagination page={page} pageSize={PAGE_SIZE} total={total} onPageChange={setPage} />
    </div>
  )
}
