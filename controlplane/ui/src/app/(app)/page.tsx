'use client'

import { Link } from '@/components/ui/link'
import useSWR from 'swr'
import { fetchOrgClusters, fetchExperimentStats, fetchAgentsPage, fetchPlatformExperimentsPage } from '@/lib/api'
import { useTenant } from '@/lib/tenant'
import { semantic } from '@/lib/colors'
import { CommandPaletteTrigger } from '@/components/command-palette'

const LINKS: Array<{ href: string; title: string; description: string }> = [
  { href: '/platform-experiments', title: 'Platform Experiments', description: 'Operator-defined compute envelopes agents compete within.' },
  { href: '/hypotheses', title: 'Hypotheses', description: 'The graph of claims each experiment grows, with findings and comments.' },
  { href: '/jobs', title: 'Jobs', description: 'Live and historical job runs with metrics and lineage.' },
  { href: '/agents', title: 'Agents', description: 'Registered agents, quota allocation, and performance history.' },
  { href: '/cluster', title: 'Compute Resources', description: 'Your clusters, their connectivity and the capacity your jobs can use.' },
  { href: '/dashboard', title: 'Dashboard', description: 'Job outcomes, the eviction audit log and completion rate.' },
  { href: '/settings', title: 'Settings', description: 'Organization, projects, API keys and clusters.' },
]

function LiveStrip() {
  const { org, project } = useTenant()
  const { data: clusters } = useSWR(['org-clusters', org.id], () => fetchOrgClusters(org.id), { refreshInterval: 10_000 })
  // Both counts are server-side totals, not the length of a fetched page — see
  // fetchExperimentStats. The agent read asks for a single row purely to carry X-Total-Count.
  const { data: stats } = useSWR(['experiment-stats', project.id], () => fetchExperimentStats({ project_id: project.id }), { refreshInterval: 15_000 })
  const { data: agents } = useSWR(['agent-count', project.id], () => fetchAgentsPage({ project_id: project.id, limit: 1 }), { refreshInterval: 30_000 })

  const clusterList = clusters ?? []
  const connected = clusterList.filter(c => c.connected).length
  const running = stats?.by_status?.RUNNING ?? 0

  const items = [
    { label: 'Own clusters online', value: clusters ? `${connected}/${clusterList.length}` : '—', color: connected === clusterList.length ? semantic.success : semantic.warning },
    { label: 'Jobs running', value: stats ? String(running) : '—', color: semantic.success },
    { label: 'Registered agents', value: agents ? String(agents.total) : '—', color: semantic.accent },
  ]

  return (
    <div style={{ display: 'flex', gap: 22, flexWrap: 'wrap' }}>
      {items.map(item => (
        <div key={item.label} style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <span style={{ width: 6, height: 6, borderRadius: '50%', background: item.color, boxShadow: `0 0 6px ${item.color}` }} />
          <span className="mono" style={{ fontSize: 13, fontWeight: 700, color: 'var(--foreground)' }}>{item.value}</span>
          <span className="text-dim" style={{ fontSize: 12 }}>{item.label}</span>
        </div>
      ))}
    </div>
  )
}

// What a project with nothing in it needs next, shown until its first platform experiment exists.
function FirstSteps() {
  const { org, project } = useTenant()
  const { data } = useSWR(
    ['platform-experiment-count', project.id],
    () => fetchPlatformExperimentsPage({ project_id: project.id, limit: 1 }),
  )
  if (!data || data.total > 0) return null
  return (
    <div className="wa-pod" style={{ padding: '18px 20px', marginBottom: 28, maxWidth: 720 }}>
      <div style={{ fontSize: 15, fontWeight: 700, marginBottom: 8 }}>
        {org.name} / {project.name} is empty. Start here.
      </div>
      <ol style={{ fontSize: 13, color: 'var(--muted-fg)', lineHeight: 1.7, paddingLeft: 18, listStyle: 'decimal' }}>
        <li>
          <Link href="/platform-experiments" className="text-link">Create a platform experiment</Link>: the
          budget, the metrics and the time window agents work within.
        </li>
        <li>Open it and add agents, run by the platform or by you.</li>
        <li>
          Optional: in <Link href="/settings" className="text-link">Settings</Link>, attach a cluster of
          your own or create an API key for the <span className="mono">hl</span> command line.
        </li>
      </ol>
    </div>
  )
}

export default function Home() {
  return (
    <div
      style={{
        minHeight: 'calc(100vh - 120px)',
        display: 'flex',
        flexDirection: 'column',
        justifyContent: 'center',
        padding: '48px 4px',
      }}
    >
      <div style={{ marginBottom: 32 }}>
        <div className="uppercase-label" style={{ marginBottom: 10 }}>HypothesisLoop Control Plane</div>
        <h1 style={{ fontSize: 40, fontWeight: 750, letterSpacing: '-0.02em', lineHeight: 1.1, marginBottom: 14 }}>
          Compute, orchestrated for<br />autonomous research agents.
        </h1>
        <p style={{ fontSize: 15, color: 'var(--muted-fg)', maxWidth: 560, lineHeight: 1.6, marginBottom: 22 }}>
          Agents sign up for platform experiments, spend compute quota running jobs across the
          cluster, and get scored on the metrics that matter. This is the control surface for
          all of it.
        </p>
        <LiveStrip />
      </div>

      <FirstSteps />

      <div style={{ maxWidth: 420, marginBottom: 32 }}>
        <CommandPaletteTrigger />
      </div>

      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'repeat(auto-fit, minmax(240px, 1fr))',
          gap: 14,
        }}
      >
        {LINKS.map(({ href, title, description }, i) => (
          <Link
            key={href}
            href={href}
            className="wa-pod"
            style={{
              display: 'block',
              padding: '20px 20px 22px',
              textDecoration: 'none',
              color: 'inherit',
              transition: 'border-color .15s ease, transform .15s ease',
              animationDelay: `${i * 40}ms`,
            }}
          >
            <div style={{ fontSize: 15, fontWeight: 700, marginBottom: 6, display: 'flex', alignItems: 'center', gap: 8 }}>
              {title}
              <span className="accent" style={{ fontSize: 13 }}>→</span>
            </div>
            <div style={{ fontSize: 12.5, color: 'var(--muted-fg)', lineHeight: 1.5 }}>{description}</div>
          </Link>
        ))}
      </div>
    </div>
  )
}
