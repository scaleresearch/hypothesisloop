'use client'

import { useEffect } from 'react'
import { useRouter } from 'next/navigation'
import useSWR from 'swr'
import { fetchPlatformExperimentsPage } from '@/lib/api'
import { graphHref } from '@/lib/hypothesis-graph'
import { useTenant } from '@/lib/tenant'
import { Page } from '@/components/ui/page'
import { EmptyState } from '@/components/ui/empty-state'
import { ErrorMessage, Loading } from '@/components/ui/status-message'

/** A graph belongs to one experiment, so the project's hypotheses open on the latest started. */
export default function ProjectHypothesesPage() {
  const { project } = useTenant()
  const router = useRouter()
  const { data: latest, error } = useSWR(
    ['platform-experiment-latest', project.id],
    () => fetchPlatformExperimentsPage({ project_id: project.id, sort: '-starts_at', limit: 1 }).then(p => p.items[0] ?? null),
  )

  useEffect(() => {
    if (latest) router.replace(graphHref(latest.id))
  }, [latest, router])

  return (
    <Page title="Hypotheses">
      {error && <ErrorMessage>Could not load the experiments of {project.name}: {error.message}</ErrorMessage>}
      {latest === null && <EmptyState>No experiments yet. A hypothesis graph grows inside one.</EmptyState>}
      {latest === undefined && !error && <Loading />}
    </Page>
  )
}
