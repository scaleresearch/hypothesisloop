'use client'

import { useState } from 'react'
import { usePathname } from 'next/navigation'
import { clsx } from 'clsx'
import useSWR from 'swr'
import { FlaskConical, Bot, ListTree, Server, LineChart, Lightbulb, Settings } from 'lucide-react'
import { fetchOrgClusters } from '@/lib/api'
import { LOGIN_PATH, signOut } from '@/lib/auth'
import { semantic } from '@/lib/colors'
import { useTenant } from '@/lib/tenant'
import { CommandPaletteTrigger } from '@/components/command-palette'
import { ProjectSwitcher } from '@/components/project-switcher'
import { Button } from '@/components/ui/button'
import { Link } from '@/components/ui/link'

const navItems = [
  { href: '/platform-experiments', label: 'Platform Experiments', icon: FlaskConical },
  { href: '/hypotheses',           label: 'Hypotheses',           icon: Lightbulb    },
  { href: '/agents',               label: 'Research Agents',      icon: Bot          },
  { href: '/jobs',                 label: 'Jobs',                 icon: ListTree     },
  { href: '/cluster',              label: 'Compute Resources',    icon: Server       },
  { href: '/dashboard',            label: 'Dashboard',            icon: LineChart    },
  { href: '/settings',             label: 'Settings',             icon: Settings     },
]

function SystemStatus() {
  const { org } = useTenant()
  const { data, error } = useSWR(['org-clusters', org.id], () => fetchOrgClusters(org.id), { refreshInterval: 10_000 })
  if (error || !data) {
    const color = error ? semantic.danger : 'var(--dim-fg)'
    return (
      <div className="wa-nav-status">
        <span className="wa-nav-status-dot" style={{ background: color }} />
        <div style={{ minWidth: 0 }}>
          <div className="wa-nav-status-label" style={{ color }}>{error ? 'Cluster status unknown' : 'Checking clusters…'}</div>
          {error && <div className="wa-nav-status-sub" title={error.message}>{error.message}</div>}
        </div>
      </div>
    )
  }
  const clusters = data.filter(c => c.owned)
  const connected = clusters.filter(c => c.connected).length
  const allUp = clusters.length > 0 && connected === clusters.length
  const someUp = connected > 0
  // An org with no cluster of its own is not degraded: its jobs run on the shared ones.
  const color = clusters.length === 0 ? 'var(--dim-fg)' : allUp ? semantic.success : someUp ? semantic.warning : semantic.danger
  const label = clusters.length === 0 ? 'No clusters of your own' : allUp ? 'All systems nominal' : someUp ? 'Degraded' : 'Offline'

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

function Account() {
  const { me } = useTenant()
  const [error, setError] = useState('')

  async function leave() {
    try {
      await signOut()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      return
    }
    // A full load, so nothing the last session fetched stays in memory for the next one.
    window.location.assign(LOGIN_PATH)
  }

  return (
    <div className="wa-nav-account">
      <span className="wa-nav-account-name" title={me.name}>{me.name || 'Signed in'}</span>
      <Button variant="link" onClick={leave}>Sign out</Button>
      {error && <span className="text-error" style={{ fontSize: 11 }}>Sign-out failed: {error}</span>}
    </div>
  )
}

export function Nav() {
  const pathname = usePathname()

  return (
    <nav className="wa-nav flex h-screen w-56 flex-col">
      <Link href="/" className="wa-nav-logo" style={{ textDecoration: 'none', color: 'inherit' }}>
        <span className="wa-nav-mark">O</span>
        <span>HypothesisLoop</span>
      </Link>
      <ProjectSwitcher />
      <div className="mt-3">
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
        <Account />
        <div className="wa-nav-footer">
          <p className="wa-nav-footer-title">HypothesisLoop v2.0</p>
          <p className="wa-nav-footer-sub">Budget: normalized compute-hours</p>
        </div>
      </div>
    </nav>
  )
}
