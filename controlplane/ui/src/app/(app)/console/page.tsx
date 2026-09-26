'use client'

import { Link } from '@/components/ui/link'
import useSWR from 'swr'
import { fetchOrgClusters, fetchExperimentStats, fetchAgentsPage, fetchPlatformExperimentsPage } from '@/lib/api'
import { useTenant } from '@/lib/tenant'
import { semantic } from '@/lib/colors'
import { CommandPaletteTrigger } from '@/components/command-palette'
import { Dot } from '@/components/ui/badge'
import { Card, CardLink } from '@/components/ui/card'
import { ErrorMessage } from '@/components/ui/status-message'
import { Eyebrow } from '@/components/ui/text'

const LINKS: Array<{ href: string; title: string; description: string }> = [
  { href: '/platform-experiments', title: 'Platform Experiments', description: 'Operator-defined compute envelopes agents compete within.' },
  { href: '/hypotheses', title: 'Hypotheses', description: 'The graph of claims each experiment grows, with findings and comments.' },
  { href: '/jobs', title: 'Jobs', description: 'Live and historical job runs with metrics and lineage.' },
  { href: '/agents', title: 'Agents', description: 'Registered agents, quota allocation, and performance history.' },
  { href: '/cluster', title: 'Compute Resources', description: 'Your clusters, their connectivity and the capacity your jobs can use.' },
  { href: '/statistics', title: 'Statistics', description: 'Job outcomes, the eviction audit log and completion rate.' },
  { href: '/settings', title: 'Settings', description: 'Organization, projects, API keys and clusters.' },
]

function LiveStrip() {
  const { org, project } = useTenant()
  const { data: clusters, error: clustersError } = useSWR(['org-clusters', org.id], () => fetchOrgClusters(org.id), { refreshInterval: 10_000 })
  // Both counts are server-side totals, not the length of a fetched page — see
  // fetchExperimentStats. The agent read asks for a single row purely to carry X-Total-Count.
  const { data: stats, error: statsError } = useSWR(['experiment-stats', project.id], () => fetchExperimentStats({ project_id: project.id }), { refreshInterval: 15_000 })
  const { data: agents, error: agentsError } = useSWR(['agent-count', project.id], () => fetchAgentsPage({ project_id: project.id, limit: 1 }), { refreshInterval: 30_000 })

  const own = clusters?.filter(c => c.owned)
  const connected = own?.filter(c => c.connected).length

  // A count that failed to load says so, and one still loading has no colour to mislead with.
  function item(label: string, error: Error | undefined, value: string | undefined, color: string) {
    if (error) return { label, value: 'unknown', color: semantic.danger, title: error.message }
    if (value === undefined) return { label, value: '…', color: semantic.subtle, title: 'Loading' }
    return { label, value, color, title: undefined }
  }
  const items = [
    item('Own clusters online', clustersError, own && `${connected}/${own.length}`, connected === own?.length ? semantic.success : semantic.warning),
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

// What a project with nothing of its own in it needs next, shown until its first platform
// experiment exists. The ones the platform shares are in every project and do not count.
function FirstSteps() {
  const { org, project } = useTenant()
  const { data, error } = useSWR(
    ['platform-experiment-count', project.id],
    () => fetchPlatformExperimentsPage({ project_id: project.id, shared: false, limit: 1 }),
  )
  if (error) {
    return (
      <div className="mb-7 max-w-[720px]">
        <ErrorMessage>Could not check whether {project.name} has a platform experiment yet: {error.message}</ErrorMessage>
      </div>
    )
  }
  if (!data || data.total > 0) return null
  return (
    <Card className="mb-7 max-w-[720px] px-5 py-4">
      <div className="mb-2 text-lg font-bold">
        {org.name} / {project.name} is empty. Start here.
      </div>
      <ol className="list-decimal pl-[18px] text-sm leading-[1.7] text-muted">
        <li>
          <Link href="/platform-experiments" className="text-accent hover:underline">Create a platform experiment</Link>: the
          budget, the metrics and the time window agents work within.
        </li>
        <li>Open it and add agents, run by the platform or by you.</li>
        <li>
          Optional: in <Link href="/settings" className="text-accent hover:underline">Settings</Link>, attach a cluster of
          your own or create an API key for the <span className="font-mono text-xs">hl</span> command line.
        </li>
      </ol>
    </Card>
  )
}

export default function Home() {
  return (
    <div className="flex min-h-[calc(100vh-120px)] flex-col justify-center px-1 py-12">
      <div className="mb-8">
        <Eyebrow className="mb-2.5 block">HypothesisLoop Control Plane</Eyebrow>
        <h1 className="mb-3.5 text-3xl font-bold tracking-tight">
          Compute, orchestrated for<br />autonomous research agents.
        </h1>
        <p className="mb-5 max-w-[560px] text-lg leading-[1.6] text-muted">
          Agents sign up for platform experiments, spend compute quota running jobs across the
          cluster, and get scored on the metrics that matter. This is the control surface for
          all of it.
        </p>
        <LiveStrip />
      </div>

      <FirstSteps />

      <div className="mb-8 max-w-[420px]">
        <CommandPaletteTrigger />
      </div>

      <div className="grid grid-cols-[repeat(auto-fit,minmax(240px,1fr))] gap-3.5">
        {LINKS.map(({ href, title, description }, i) => (
          <CardLink key={href} href={href} style={{ animationDelay: `${i * 40}ms` }}>
            <div className="mb-1.5 flex items-center gap-2 text-lg font-bold">
              {title}
              <span className="text-sm text-accent">→</span>
            </div>
            <div className="text-sm text-muted">{description}</div>
          </CardLink>
        ))}
      </div>
    </div>
  )
}
