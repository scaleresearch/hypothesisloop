'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { clustersInUse } from '@/lib/clusters'
import { fetchOrgClusters, fetchExperimentStats, fetchAgentsPage, fetchPlatformExperimentsPage } from '@/lib/api'
import { useProjectRouter } from '@/lib/query-state'
import { useTenant } from '@/lib/tenant'
import type { PlatformExperiment } from '@/types'
import { semantic } from '@/lib/colors'
import { GoalInput } from '@/components/goal-input'
import { PlatformExperimentCard } from '@/components/platform-experiment-card'
import { PlatformExperimentDialog, PlatformExperimentForm } from '@/components/platform-experiment-form'
import { Dot } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader } from '@/components/ui/card'
import { Link } from '@/components/ui/link'
import { ErrorMessage, Loading } from '@/components/ui/status-message'
import { Eyebrow } from '@/components/ui/text'

// How many of the newest platform experiments the home page lists.
const LISTED = 5

function LiveStrip() {
  const { org, project } = useTenant()
  const { data: clusters, error: clustersError } = useSWR(['org-clusters', org.id], () => fetchOrgClusters(org.id), { refreshInterval: 10_000 })
  // Both counts are server-side totals, not the length of a fetched page — see
  // fetchExperimentStats. The agent read asks for a single row purely to carry X-Total-Count.
  const { data: stats, error: statsError } = useSWR(['experiment-stats', project.id], () => fetchExperimentStats({ project_id: project.id }), { refreshInterval: 15_000 })
  const { data: agents, error: agentsError } = useSWR(['agent-count', project.id], () => fetchAgentsPage({ project_id: project.id, limit: 1 }), { refreshInterval: 30_000 })

  const inUse = clusters && clustersInUse(clusters)

  // A count that failed to load says so, and one still loading has no colour to mislead with.
  function item(label: string, error: Error | undefined, value: string | undefined, color: string) {
    if (error) return { label, value: 'unknown', color: semantic.danger, title: error.message }
    if (value === undefined) return { label, value: '…', color: semantic.subtle, title: 'Loading' }
    return { label, value, color, title: undefined }
  }
  const items = [
    // What jobs can run on, the platform's clusters with the org's own: a person with none of
    // their own is not without compute.
    item(inUse?.whose ? `Clusters online (${inUse.whose})` : 'Clusters online', clustersError, inUse && `${inUse.online}/${inUse.used}`,
      inUse && inUse.used > 0 && inUse.online === inUse.used ? semantic.success : semantic.warning),
    // A status no job holds is absent from the answer.
    item('Jobs running', statsError, stats && String(stats.by_status?.RUNNING ?? 0), semantic.success),
    item('Registered agents', agentsError, agents && String(agents.total), semantic.accent),
  ]

  return (
    <div className="flex flex-wrap gap-5">
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

/**
 * The platform experiments of the selected project, and the way to a new one. A project with
 * none of its own shows the form itself; one with some lists the newest and opens the same form
 * in a dialog. The ones the platform shares are listed in both, read only, and do not count as
 * the project's own.
 */
function Experiments() {
  const { org, project } = useTenant()
  const router = useProjectRouter()
  const [dialog, setDialog] = useState<{ editing: PlatformExperiment | null } | null>(null)
  const own = useSWR(
    ['platform-experiment-count', project.id],
    () => fetchPlatformExperimentsPage({ project_id: project.id, shared: false, limit: 1 }),
  )
  const listed = useSWR(
    ['platform-experiments-home', project.id],
    () => fetchPlatformExperimentsPage({ project_id: project.id, sort: '-starts_at', limit: LISTED }),
    { refreshInterval: 15_000 },
  )

  // A new experiment opens on its own page, where its agents are added and it is started.
  const open = (pe: PlatformExperiment) => router.push(`/platform-experiments/${pe.id}`)
  const failure = own.error ?? listed.error
  if (failure) return <ErrorMessage>Could not load the platform experiments of {project.name}: {failure.message}</ErrorMessage>
  if (!own.data || !listed.data) return <Loading />

  const none = own.data.total === 0
  const experiments = listed.data.items
  return (
    <section aria-label="Platform experiments">
      <Card className="max-w-[720px]">
        <CardHeader>Start from one sentence</CardHeader>
        <CardContent>
          <GoalInput projectID={project.id} onCreated={open} />
        </CardContent>
      </Card>
      {none ? (
        <Card className="max-w-[720px]">
          <CardHeader>New platform experiment in {org.name} / {project.name}</CardHeader>
          <CardContent>
            <p className="mb-4 text-sm text-muted">
              A platform experiment sets the task, the budget, the metrics and the time window agents work within.
              Pick the repository it works on, and under Competition settings choose who runs the agents: with
              hosted agents it starts as soon as you create it.
            </p>
            <PlatformExperimentForm onSaved={open} />
          </CardContent>
        </Card>
      ) : (
        <div className="mb-3.5 flex flex-wrap items-center justify-between gap-3">
          <h2 className="text-lg font-bold">Platform experiments of {project.name}</h2>
          <Button variant="primary" onClick={() => setDialog({ editing: null })}>New experiment</Button>
        </div>
      )}
      {none && experiments.length > 0 && <Eyebrow className="mb-2.5 mt-6 block">Shared by the platform</Eyebrow>}
      {experiments.map(pe => <PlatformExperimentCard key={pe.id} pe={pe} onEdit={editing => setDialog({ editing })} />)}
      {listed.data.total > experiments.length && (
        <Link href="/platform-experiments" className="text-sm text-accent hover:underline">
          All {listed.data.total} platform experiments →
        </Link>
      )}
      {dialog && (
        <PlatformExperimentDialog
          initial={dialog.editing}
          onClose={() => setDialog(null)}
          // An edit stays here and rereads the list; a new experiment opens.
          onSaved={saved => (dialog.editing ? void listed.mutate() : open(saved))}
        />
      )}
    </section>
  )
}

export default function Home() {
  return (
    <div className="px-1 py-6">
      <div className="mb-8">
        <Eyebrow className="mb-2.5 block">HypothesisLoop Control Plane</Eyebrow>
        <h1 className="mb-3.5 text-2xl font-bold tracking-tight">Compute, orchestrated for autonomous research agents.</h1>
        <LiveStrip />
      </div>
      <Experiments />
    </div>
  )
}
