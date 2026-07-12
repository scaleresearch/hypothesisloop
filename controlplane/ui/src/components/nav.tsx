'use client'

import Link from 'next/link'
import { usePathname } from 'next/navigation'
import { clsx } from 'clsx'
import useSWR from 'swr'
import { FlaskConical, Bot, ListTree, Server, LineChart, Lightbulb } from 'lucide-react'
import { fetchClusters } from '@/lib/api'
import type { ClustersResponse } from '@/types'
import { semantic } from '@/lib/colors'
import { CommandPaletteTrigger } from '@/components/command-palette'

const navItems = [
  { href: '/platform-experiments', label: 'Platform Experiments', icon: FlaskConical },
  { href: '/hypotheses',           label: 'Hypotheses',           icon: Lightbulb    },
  { href: '/agents',               label: 'Research Agents',      icon: Bot          },
  { href: '/jobs',                 label: 'Jobs',                 icon: ListTree     },
  { href: '/cluster',              label: 'Compute Resources',    icon: Server       },
  { href: '/dashboard',            label: 'Scheduler Quality',    icon: LineChart    },
]

function SystemStatus() {
  const { data } = useSWR<ClustersResponse>('clusters', fetchClusters, { refreshInterval: 10_000 })
  const clusters = data?.clusters ?? []
  const connected = clusters.filter(c => c.connected).length
  const allUp = clusters.length > 0 && connected === clusters.length
  const someUp = connected > 0
  const color = allUp ? semantic.success : someUp ? semantic.warning : semantic.danger
  const label = clusters.length === 0 ? 'No clusters' : allUp ? 'All systems nominal' : someUp ? 'Degraded' : 'Offline'

  return (
    <div className="wa-nav-status">
      <span className="wa-nav-status-dot" style={{ background: color, boxShadow: `0 0 8px ${color}` }} />
      <div style={{ minWidth: 0 }}>
        <div className="wa-nav-status-label" style={{ color }}>{label}</div>
        <div className="wa-nav-status-sub">{connected}/{clusters.length} clusters online</div>
      </div>
    </div>
  )
}

export function Nav() {
  const pathname = usePathname()

  return (
    <nav className="wa-nav flex h-screen w-56 flex-col">
      <Link href="/" className="wa-nav-logo" style={{ textDecoration: 'none', color: 'inherit' }}>
        <span className="wa-nav-mark">O</span>
        <span>OpenResearch</span>
      </Link>
      <div className="mt-4">
        <CommandPaletteTrigger />
      </div>
      <ul className="flex flex-col mt-1 px-1 gap-0.5">
        {navItems.map(({ href, label, icon: Icon }) => {
          const active = pathname === href || pathname.startsWith(href + '/')
          return (
            <li key={href}>
              <Link
                href={href}
                className={clsx('wa-nav-item', active && 'active')}
              >
                <Icon size={16} strokeWidth={2} className="wa-nav-icon" />
                {label}
              </Link>
            </li>
          )
        })}
      </ul>
      <div className="mt-auto px-4 pb-5">
        <SystemStatus />
        <div className="wa-nav-footer">
          <p className="wa-nav-footer-title">OpenResearch v2.0</p>
          <p className="wa-nav-footer-sub">Unit: T4-GPU-hours</p>
        </div>
      </div>
    </nav>
  )
}
