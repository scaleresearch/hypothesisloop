'use client'

import { Suspense, useState } from 'react'
import useSWR from 'swr'
import { useQueryParam } from '@/lib/query-state'
import { useTenant } from '@/lib/tenant'
import { fetchPlatformExperimentsPage } from '@/lib/api'
import type { PlatformExperiment } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { PlatformExperimentDialog } from '@/components/platform-experiment-form'
import { PlatformExperimentCard } from '@/components/platform-experiment-card'
import { Card, CardContent } from '@/components/ui/card'
import { Button, Chip } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
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

export default function PlatformExperimentsPage() {
  return (
    <Suspense fallback={<Loading />}>
      <PlatformExperimentsPageContent />
    </Suspense>
  )
}

function PlatformExperimentsPageContent() {
  const { org, project } = useTenant()
  const [statusFilter, setStatusFilter] = useQueryParam('status')
  const [search, setSearch] = useQueryParam('q')
  const [sort, setSort] = useQueryParam('sort', '-starts_at')
  const [page, setPage] = useState(0)
  const [modal, setModal] = useState<{ open: boolean; editing?: PlatformExperiment | null }>({ open: false })

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

  const experiments = data?.items ?? []
  const total = data?.total ?? 0
  const statuses = ['', 'open', 'running', 'draft', 'closed']

  return (
    <div>
      {modal.open && (
        <PlatformExperimentDialog
          initial={modal.editing}
          onClose={() => setModal({ open: false })}
          onSaved={() => mutate()}
        />
      )}

      <PageHeader
        title="Platform Experiments"
        description="Operator-defined compute envelopes. Agents sign up and compete for AccH quota within each experiment."
        actions={<Button size="sm" variant="primary" onClick={() => setModal({ open: true, editing: null })}>+ New Experiment</Button>}
      />

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
      {error && <ErrorMessage>Could not load the platform experiments of {project.name}: {error.message}</ErrorMessage>}

      {data && experiments.length === 0 && (statusFilter || search ? (
        <Card>
          <CardContent>
            <div className="py-2 text-center text-xs text-subtle">No platform experiments in this project match{search ? ` "${search}"` : ' this filter'}.</div>
          </CardContent>
        </Card>
      ) : (
        <Card>
          <CardContent>
            <div className="py-4 text-center">
              <div className="mb-1.5 text-lg font-bold">{org.name} / {project.name} has no platform experiments yet</div>
              <p className="mx-auto mb-3.5 max-w-[520px] text-sm leading-relaxed text-muted">
                A platform experiment sets the budget, the metrics and the time window agents work
                within. Create one, then open it to add agents.
              </p>
              <Button variant="primary" onClick={() => setModal({ open: true, editing: null })}>
                Create the first platform experiment
              </Button>
            </div>
          </CardContent>
        </Card>
      ))}

      {experiments.map(pe => (
        <PlatformExperimentCard
          key={pe.id}
          pe={pe}
          onEdit={pe => setModal({ open: true, editing: pe })}
        />
      ))}

      <Pagination page={page} pageSize={PAGE_SIZE} total={total} onPageChange={setPage} />
    </div>
  )
}
