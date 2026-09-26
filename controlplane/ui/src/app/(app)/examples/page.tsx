'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { fetchPlatformExperimentsPage } from '@/lib/api'
import { PlatformExperimentCard } from '@/components/platform-experiment-card'
import { Card } from '@/components/ui/card'
import { PageHeader } from '@/components/ui/page-header'
import { Pagination } from '@/components/ui/pagination'
import { EmptyState, ErrorMessage, Loading } from '@/components/ui/status-message'

const PAGE_SIZE = 10

/** The platform experiments staff shared with every account, each opening read only. */
export default function ExamplesPage() {
  const [page, setPage] = useState(0)
  const { data, error, isLoading } = useSWR(
    ['examples', page],
    () => fetchPlatformExperimentsPage({ shared: true, sort: '-starts_at', limit: PAGE_SIZE, offset: page * PAGE_SIZE }),
    { refreshInterval: 30_000, keepPreviousData: true },
  )

  return (
    <div>
      <PageHeader
        title="Examples"
        description="Platform experiments the platform shows to every account. Open one to see its agents, jobs, metrics, hypotheses and findings. An example is read only."
      />

      {isLoading && <Loading />}
      {error && <ErrorMessage>Could not load the examples: {error.message}</ErrorMessage>}

      {data && data.items.length === 0 && (
        <Card>
          <EmptyState>There are no examples yet. The ones the platform shares will be listed here.</EmptyState>
        </Card>
      )}

      {data?.items.map(pe => <PlatformExperimentCard key={pe.id} pe={pe} />)}

      <Pagination page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPageChange={setPage} />
    </div>
  )
}
