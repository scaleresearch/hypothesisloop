'use client'

import { Suspense, useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import useSWR from 'swr'
import { clustersInUse } from '@/lib/clusters'
import { usePathname, useRouter, useSearchParams } from 'next/navigation'
import { useQueryParam } from '@/lib/query-state'
import { experimentPath, orgPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { fetchOrgClusters } from '@/lib/api/clusters'
import { fetchExperimentDraft } from '@/lib/api/drafts'
import { fetchExperimentStats } from '@/lib/api/jobs'
import { fetchPlatformExperimentsPage } from '@/lib/api/platform-experiments'
import { ExperimentDrafts } from '@/components/experiment-drafts'
import { GoalInput } from '@/components/goal-input'
import { Page } from '@/components/ui/page'
import { PlatformExperimentDialog } from '@/components/platform-experiment-form'
import { PlatformExperimentCard } from '@/components/platform-experiment-card'
import { Dot } from '@/components/ui/dot'
import { EmptyState } from '@/components/ui/empty-state'
import { Inline, Stack } from '@/components/ui/stack'
import { Heading, Text } from '@/components/ui/text'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { TextLink } from '@/components/ui/link'
import { Select } from '@/components/ui/select'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { Pagination } from '@/components/ui/pagination'
import { Segmented } from '@/components/ui/segmented'
import { errorMessage } from '@/lib/errors'
import { keys } from '@/lib/keys'
import { POLL } from '@/lib/poll'
import type { ExperimentDraft, PlatformExperiment } from '@/types/api'

const PAGE_SIZE = 10

const SORT_OPTIONS = [
  { value: '-starts_at', label: 'Newest first' },
  { value: 'starts_at', label: 'Oldest first' },
  { value: 'name', label: 'Name (A–Z)' },
  { value: '-name', label: 'Name (Z–A)' },
]

/** The project's running jobs, only while there are some, as the subtitle of the page. */
function useLiveCounts(): ReactNode {
  const { project } = useTenant()
  const { data: stats, error: statsError } = useSWR(keys.projectJobStats(project.id), () => fetchExperimentStats({ project_id: project.id }))

  // A status no job holds is absent from the answer.
  const running = stats?.by_status?.RUNNING ?? 0
  const items = [
    statsError ? { key: 'jobs', text: 'Jobs running unknown', tone: 'danger' as const, title: statsError.message }
      : running > 0 && { key: 'jobs', text: `${running} ${running === 1 ? 'job' : 'jobs'} running`, tone: 'success' as const, title: undefined },
  ].filter(item => !!item)
  if (items.length === 0) return null

  return (
    <Inline gap="xl" className="tabular-nums">
      {items.map(item => (
        <span key={item.key} title={item.title} className="flex items-center gap-2">
          <Dot tone={item.tone} />
          {item.text}
        </span>
      ))}
    </Inline>
  )
}

const capitalised = (word: string) => word.charAt(0).toUpperCase() + word.slice(1)

export default function PlatformExperimentsPage() {
  return (
    <Suspense fallback={<Loading />}>
      <PlatformExperimentsPageContent />
    </Suspense>
  )
}

/**
 * The experiments of the selected project and two ways to a new one: a sentence the platform
 * plans, or the full form, which opens in a dialog as it does for an edit. A draft of the form
 * opens from ?draft, which is also where GitHub's return brings the person back to. The
 * experiments the platform shares are listed too, read only.
 */
function PlatformExperimentsPageContent() {
  const { org, project } = useTenant()
  const router = useRouter()
  const [statusFilter, setStatusFilter] = useQueryParam('status')
  const [search, setSearch] = useQueryParam('q')
  const [sort, setSort] = useQueryParam('sort', '-starts_at')
  const [page, setPage] = useState(0)
  const [dialog, setDialog] = useState<{ editing: PlatformExperiment | null; draft?: ExperimentDraft } | null>(null)

  // ?draft opens that draft once and leaves the address.
  const pathname = usePathname()
  const searchParams = useSearchParams()
  const draftID = searchParams.get('draft')
  const [draftError, setDraftError] = useState('')
  useEffect(() => {
    if (!draftID) return
    let live = true
    fetchExperimentDraft(project.id, draftID).then(
      draft => { if (live) setDialog({ editing: null, draft }) },
      (e: unknown) => { if (live) setDraftError(errorMessage(e)) },
    )
    const rest = new URLSearchParams(searchParams.toString())
    rest.delete('draft')
    router.replace(rest.size ? `${pathname}?${rest}` : pathname)
    return () => { live = false }
  }, [draftID, project.id, pathname, router, searchParams])

  const { data, error, mutate } = useSWR(
    keys.experiments(project.id, statusFilter, search, sort, page),
    () => fetchPlatformExperimentsPage({
      project_id: project.id,
      status: statusFilter || undefined,
      q: search || undefined,
      sort,
      limit: PAGE_SIZE,
      offset: page * PAGE_SIZE,
    }),
    // Read again for what each one has spent.
    { refreshInterval: POLL.reports },
  )
  // The sidebar reads the same key, so this is one request for both.
  const { data: clusters } = useSWR(keys.orgClusters(org.id), () => fetchOrgClusters(org.id), { refreshInterval: POLL.reports })

  // A new experiment opens on its own page: a draft to be planned, or one to add agents to.
  const open = (pe: PlatformExperiment) => router.push(experimentPath(pe.id))
  const experiments = data?.items ?? []
  const total = data?.total ?? 0
  const statuses = ['', 'open', 'running', 'draft', 'closed']
  const live = useLiveCounts()
  // A project with no experiment yet has nothing to filter, so the page is the goal box.
  const first = !!data && total === 0 && !statusFilter && !search

  return (
    <Page
      title="Experiments"
      description={live}
      actions={<Button onClick={() => setDialog({ editing: null })}>Set up manually</Button>}
    >
      {draftError && <ErrorMessage>Could not open the draft: {draftError}</ErrorMessage>}
      {dialog && (
        <PlatformExperimentDialog
          initial={dialog.editing}
          draft={dialog.draft}
          onClose={() => setDialog(null)}
          // An edit stays here and rereads the list. A new experiment opens.
          onSaved={(saved, created) => (dialog.editing && !created ? void mutate() : open(saved))}
        />
      )}

      {first && (
        <Heading level={2}>Start your first experiment</Heading>
      )}
      {/* The box draws no edge of its own; the card is its edge, and darkens while the box has focus. */}
      <Card className="mb-7 transition-colors focus-within:border-border-control">
        <GoalInput large={first} projectID={project.id} onCreated={open} onConfigure={draft => setDialog({ editing: null, draft })} />
        {clusters && clustersInUse(clusters).online === 0 && (
          <Text variant="note" className="border-t px-6 py-3">
            No compute is online yet. <TextLink inline href={orgPath(org.id, 'clusters')}>Go to Compute</TextLink>
          </Text>
        )}
      </Card>

      {!first && <Inline gap="md">
        <Segmented
          label="Status"
          size="sm"
          options={statuses.map(s => ({ value: s, label: s ? capitalised(s) : 'All' }))}
          value={statusFilter}
          onChange={s => { setStatusFilter(s); setPage(0) }}
        />
        <Input
          fieldSize="sm"
          className="ml-auto w-56"
          aria-label="Search"
          value={search}
          onChange={e => { setSearch(e.target.value); setPage(0) }}
          placeholder="Search"
        />
        <Select fieldSize="sm" aria-label="Sort" value={sort} onChange={v => { setSort(v); setPage(0) }} options={SORT_OPTIONS} />
      </Inline>}

      {(statusFilter === '' || statusFilter === 'draft') && !search && <ExperimentDrafts onOpen={draft => setDialog({ editing: null, draft })} />}

      {error && <ErrorMessage>Could not load the experiments of {project.name}: {error.message}</ErrorMessage>}
      {!data && !error && <Loading />}

      {data && experiments.length === 0 && !first && (
        <EmptyState framed>
          {statusFilter || search
            ? `No experiments match${search ? ` "${search}"` : ' this filter'}.`
            : 'No experiments yet.'}
        </EmptyState>
      )}

      {experiments.length > 0 && (
        <Stack gap="sm">
          {experiments.map(pe => <PlatformExperimentCard key={pe.id} pe={pe} onEdit={editing => setDialog({ editing })} />)}
        </Stack>
      )}

      <Pagination page={page} pageSize={PAGE_SIZE} total={total} onPageChange={setPage} />
    </Page>
  )
}
