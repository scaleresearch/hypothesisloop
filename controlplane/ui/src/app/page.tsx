'use client'

import Link from 'next/link'
import useSWR from 'swr'
import { fetchClusters, fetchExperiments, fetchAgents } from '@/lib/api'
import type { ClustersResponse, Experiment, Agent } from '@/types'
import { semantic } from '@/lib/colors'
import { CommandPaletteTrigger } from '@/components/command-palette'

const LINKS: Array<{ href: string; title: string; description: string }> = [
  { href: '/dashboard', title: 'Dashboard', description: 'Fleet-wide health, throughput, and budget burn at a glance.' },
  { href: '/platform-experiments', title: 'Platform Experiments', description: 'Operator-defined compute envelopes agents compete within.' },
  { href: '/jobs', title: 'Jobs', description: 'Live and historical job runs with metrics and lineage.' },
  { href: '/agents', title: 'Agents', description: 'Registered agents, balances, and performance history.' },
  { href: '/cluster', title: 'Cluster', description: 'Registered clusters and node-agent connectivity.' },
]

function LiveStrip() {
  const { data: clusters } = useSWR<ClustersResponse>('clusters', fetchClusters, { refreshInterval: 10_000 })
  const { data: jobs } = useSWR<Experiment[]>('jobs-all', () => fetchExperiments({ limit: 1000 }), { refreshInterval: 15_000 })
  const { data: agents } = useSWR<Agent[]>('agents', fetchAgents, { refreshInterval: 30_000 })

  const clusterList = clusters?.clusters ?? []
  const connected = clusterList.filter(c => c.connected).length
  const running = (jobs ?? []).filter(j => j.status === 'RUNNING').length

  const items = [
    { label: 'Clusters online', value: clusters ? `${connected}/${clusterList.length}` : '—', color: clusterList.length > 0 && connected === clusterList.length ? semantic.success : semantic.warning },
    { label: 'Jobs running', value: jobs ? String(running) : '—', color: semantic.success },
    { label: 'Registered agents', value: agents ? String(agents.length) : '—', color: semantic.accent },
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
