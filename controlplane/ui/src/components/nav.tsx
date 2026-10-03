'use client'

import { clustersInUse } from '@/lib/clusters'
import { useState } from 'react'
import { usePathname } from 'next/navigation'
import { clsx } from 'clsx'
import useSWR from 'swr'
import { FlaskConical, ListTree, Server, LineChart, Lightbulb, Settings } from 'lucide-react'
import { fetchOrgClusters } from '@/lib/api'
import { HOME_PATH, LANDING_PATH, signOut } from '@/lib/auth'
import { useTenant } from '@/lib/tenant'
import { budgetUnit } from '@/lib/units'
import { CommandPaletteTrigger } from '@/components/command-palette'
import { ProjectSwitcher } from '@/components/project-switcher'
import { ThemeSwitch } from '@/components/theme-switch'
import { Button } from '@/components/ui/button'
import { Link } from '@/components/ui/link'
import { Logo } from '@/components/ui/logo'
import { InlineError } from '@/components/ui/status-message'

const navItems = [
  { href: '/platform-experiments', label: 'Experiments',          icon: FlaskConical },
  { href: '/hypotheses',           label: 'Hypotheses',           icon: Lightbulb    },
  { href: '/jobs',                 label: 'Jobs',                 icon: ListTree     },
  { href: '/cluster',              label: 'Compute Resources',    icon: Server       },
  { href: '/statistics',           label: 'Statistics',           icon: LineChart,   staffOnly: true },
  { href: '/settings',             label: 'Settings',             icon: Settings     },
]

const statusTone = {
  idle: 'text-subtle',
  success: 'text-success',
  warning: 'text-warning',
  danger: 'text-danger',
}

function StatusLine({ tone, label, sub }: { tone: keyof typeof statusTone; label: string; sub?: string }) {
  return (
    <div className={clsx('mb-3 flex items-center gap-2 rounded border bg-surface-inset px-2.5 py-2', statusTone[tone])}>
      <span aria-hidden="true" className={clsx('h-1.5 w-1.5 flex-none rounded-full bg-current', tone === 'idle' && 'animate-pulse')} />
      <div className="min-w-0">
        <div className="truncate text-xs font-semibold">{label}</div>
        {sub && <div className="truncate text-2xs text-subtle" title={sub}>{sub}</div>}
      </div>
    </div>
  )
}

function SystemStatus() {
  const { org } = useTenant()
  const { data, error } = useSWR(['org-clusters', org.id], () => fetchOrgClusters(org.id), { refreshInterval: 10_000 })
  if (error || !data) {
    return (
      <StatusLine tone={error ? 'danger' : 'idle'} label={error ? 'Cluster status unknown' : 'Checking clusters…'} sub={error?.message} />
    )
  }
  // What the org's jobs can run on: its own clusters and the platform's it uses.
  const { used, online, whose } = clustersInUse(data)
  const tone = used === 0 ? 'idle' : online === used ? 'success' : online > 0 ? 'warning' : 'danger'
  const label = used === 0 ? 'No cluster in use' : online === used ? 'All clusters online' : online > 0 ? 'Some clusters offline' : 'All clusters offline'

  return <StatusLine tone={tone} label={label} sub={`${online}/${used} clusters online${whose ? ` · ${whose}` : ''}`} />
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
    window.location.assign(LANDING_PATH)
  }

  return (
    <div className="mb-3 flex flex-wrap items-center justify-between gap-x-2 gap-y-1 text-xs">
      <span className="min-w-0 truncate font-semibold" title={me.name}>{me.name || 'Signed in'}</span>
      <Button variant="link" size="sm" onClick={leave}>Sign out</Button>
      {error && <InlineError>Sign-out failed: {error}</InlineError>}
    </div>
  )
}

export function Nav() {
  const pathname = usePathname()
  const { staff } = useTenant()

  return (
    <nav aria-label="Main" className="flex h-screen w-56 flex-none flex-col border-r bg-surface">
      <Link href={HOME_PATH} className="border-b px-4 py-5">
        <Logo />
      </Link>
      <ProjectSwitcher />
      <div className="mb-2 mt-3 px-2.5">
        <CommandPaletteTrigger />
      </div>
      <ul className="mt-1 flex flex-col gap-0.5 px-2.5">
        {navItems.filter(item => staff || !item.staffOnly).map(({ href, label, icon: Icon }) => {
          const active = pathname === href || pathname.startsWith(href + '/')
          return (
            <li key={href}>
              <Link
                href={href}
                aria-current={active ? 'page' : undefined}
                className={clsx(
                  'flex items-center gap-2.5 rounded px-3 py-2 text-sm font-medium transition-colors',
                  active ? 'bg-highlight text-foreground' : 'text-muted hover:bg-hover hover:text-foreground',
                )}
              >
                <Icon aria-hidden="true" size={16} strokeWidth={2} className="flex-none" />
                {label}
              </Link>
            </li>
          )
        })}
      </ul>
      <div className="mt-auto px-2.5 pb-5">
        <SystemStatus />
        <Account />
        <div className="border-t pt-3">
          <div className="flex items-center justify-between gap-2">
            <p className="text-xs font-semibold text-muted">HypothesisLoop v2.0</p>
            <ThemeSwitch />
          </div>
          <p className="mt-0.5 text-2xs text-subtle">Budgets are counted in {budgetUnit('accelerator', 'long')}, or {budgetUnit('cpu', 'long')} for jobs on CPU.</p>
        </div>
      </div>
    </nav>
  )
}
