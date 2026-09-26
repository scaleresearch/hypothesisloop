'use client'

import { Suspense, useState } from 'react'
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
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button, Chip } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { TextLink } from '@/components/ui/link'
import { Select } from '@/components/ui/select'
import { Callout, Loading, ErrorMessage } from '@/components/ui/status-message'
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
    item('Agents', agentsError, agents && String(agents.total), semantic.accent),
  ]

  return (
    <div className="mb-6 flex flex-wrap gap-5">
      {items.map(item => (
        <div key={item.label} title={item.title} className="flex items-center gap-2">
          <Dot color={item.color} />
          <span className="font-mono text-sm font-bold text-foreground">{item.value}</span>
          <span className="text-xs text-subtle">{item.label}</span>
        </div>
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
 * The experiments of the selected project and the way to a new one: a sentence saying what to
 * find out, which the platform plans. The full form, for who wants to state the metrics, the
 * budget and the agents themselves, is behind Advanced and opens in a dialog, as it does for an
 * edit. The experiments the platform shares are listed too, read only.
 */
function PlatformExperimentsPageContent() {
  const { org, project } = useTenant()
  const router = useProjectRouter()
  const [statusFilter, setStatusFilter] = useQueryParam('status')
  const [search, setSearch] = useQueryParam('q')
  const [sort, setSort] = useQueryParam('sort', '-starts_at')
  const [page, setPage] = useState(0)
  const [dialog, setDialog] = useState<{ editing: PlatformExperiment | null } | null>(null)

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

      <PageHeader
        title="Experiments"
        description="Write what you want to find out. The platform plans the experiment, and agents run it on your compute."
      />
      <LiveStrip />

      <Card className="mb-6 max-w-[720px]">
        <CardHeader><CardTitle>New experiment in {org.name} / {project.name}</CardTitle></CardHeader>
        <CardContent className="grid gap-3">
          {clusters && clustersInUse(clusters).online === 0 && (
            <Callout tone="warning" className="mb-0">
              No cluster this org can use is online, so a plan&apos;s baseline job would wait. <TextLink href="/cluster">Compute resources</TextLink> shows what is connected.
            </Callout>
          )}
          <GoalInput projectID={project.id} onCreated={open} />
          <div>
            <Button variant="link" onClick={() => setDialog({ editing: null })}>Advanced</Button>
            <span className="ml-2 text-xs text-subtle">Set the metrics, the budget, the repository and the agents yourself.</span>
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
