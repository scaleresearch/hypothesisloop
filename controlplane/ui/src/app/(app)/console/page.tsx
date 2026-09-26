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
import { PlatformExperimentDialog } from '@/components/platform-experiment-form'
import { Dot } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader } from '@/components/ui/card'
import { Link } from '@/components/ui/link'
import { Callout, ErrorMessage, Loading } from '@/components/ui/status-message'
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
 * The platform experiments of the selected project, and the way to a new one: a sentence saying
 * what to find out, which the platform plans. The full form, for who wants to state the metrics,
 * the budget and the agents themselves, is behind Advanced and opens in a dialog, as it does
 * for an edit. The experiments the platform shares are listed too, read only.
 */
function Experiments() {
  const { org, project } = useTenant()
  const router = useProjectRouter()
  const [dialog, setDialog] = useState<{ editing: PlatformExperiment | null } | null>(null)
  const listed = useSWR(
    ['platform-experiments-home', project.id],
    () => fetchPlatformExperimentsPage({ project_id: project.id, sort: '-starts_at', limit: LISTED }),
    { refreshInterval: 15_000 },
  )
  // The strip above reads the same key, so this is one request for both.
  const { data: clusters } = useSWR(['org-clusters', org.id], () => fetchOrgClusters(org.id), { refreshInterval: 10_000 })

  // A new experiment opens on its own page: a draft to be planned, or one to add agents to.
  const open = (pe: PlatformExperiment) => router.push(`/platform-experiments/${pe.id}`)
  const experiments = listed.data?.items ?? []
  return (
    <section aria-label="Platform experiments">
      <Card className="max-w-[720px]">
        <CardHeader>New experiment in {org.name} / {project.name}</CardHeader>
        <CardContent className="grid gap-3">
          {clusters && clustersInUse(clusters).online === 0 && (
            <Callout tone="warning" className="mb-0">
              No cluster this org can use is online, so a plan&apos;s baseline job would wait. <Link href="/cluster" className="text-accent hover:underline">Compute resources</Link> shows what is connected.
            </Callout>
          )}
          <GoalInput projectID={project.id} onCreated={open} />
          <div>
            <Button variant="link" onClick={() => setDialog({ editing: null })}>Advanced</Button>
            <span className="ml-2 text-xs text-subtle">Set the metrics, the budget, the repository and the agents yourself.</span>
          </div>
        </CardContent>
      </Card>
      {listed.error && <ErrorMessage>Could not load the platform experiments of {project.name}: {listed.error.message}</ErrorMessage>}
      {!listed.error && !listed.data && <Loading />}
      {listed.data && (
        <>
          <h2 className="mb-3.5 mt-6 text-lg font-bold">Platform experiments of {project.name}</h2>
          {experiments.length === 0 && <p className="text-sm text-muted">None yet. Write what you want to find out above and the platform proposes the first.</p>}
          {experiments.map(pe => <PlatformExperimentCard key={pe.id} pe={pe} onEdit={editing => setDialog({ editing })} />)}
          {listed.data.total > experiments.length && (
            <Link href="/platform-experiments" className="text-sm text-accent hover:underline">
              All {listed.data.total} platform experiments →
            </Link>
          )}
        </>
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
