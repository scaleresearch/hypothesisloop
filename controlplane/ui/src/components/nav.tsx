'use client'

import { clustersInUse } from '@/lib/clusters'
import { useState } from 'react'
import { usePathname } from 'next/navigation'
import { clsx } from 'clsx'
import useSWR from 'swr'
import { FlaskConical, ListTree, Server, LineChart, Lightbulb, Settings } from 'lucide-react'
import { fetchOrgClusters } from '@/lib/api'
import { HOME_PATH, LANDING_PATH, signOut } from '@/lib/auth'
import { sectionOf, sectionPath, type Section } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { budgetUnit } from '@/lib/units'
import { CommandPaletteTrigger } from '@/components/command-palette'
import { ProjectSwitcher } from '@/components/project-switcher'
import { ThemeSwitch } from '@/components/theme-switch'
import { Button } from '@/components/ui/button'
import { Link } from '@/components/ui/link'
import { Logo } from '@/components/ui/logo'
import { StatusLabel } from '@/components/ui/badge'
import { Inline, Stack } from '@/components/ui/stack'
import { InlineError } from '@/components/ui/status-message'
import { Eyebrow, Text } from '@/components/ui/text'
import type { Tone } from '@/lib/colors'

const navItems: { section: Section; label: string; icon: typeof Settings; staffOnly?: boolean }[] = [
  { section: 'experiments', label: 'Experiments', icon: FlaskConical },
  { section: 'hypotheses', label: 'Hypotheses', icon: Lightbulb },
  { section: 'jobs', label: 'Jobs', icon: ListTree },
  { section: 'clusters', label: 'Compute Resources', icon: Server },
  { section: 'statistics', label: 'Statistics', icon: LineChart, staffOnly: true },
  { section: 'settings', label: 'Settings', icon: Settings },
]

// Checking is no state of the clusters yet: it pulses in the quiet colour until one is known.
function StatusLine({ tone, label, sub }: { tone: Tone | 'idle'; label: string; sub?: string }) {
  return (
    <div className="rounded border bg-surface px-3 py-2">
      <StatusLabel tone={tone === 'idle' ? 'neutral' : tone} pulse={tone === 'idle'} className="w-full">
        <span className="truncate">{label}</span>
      </StatusLabel>
      {sub && <Text variant="meta" tone="subtle" className="truncate pl-3" title={sub}>{sub}</Text>}
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
    <Inline className="justify-between text-xs">
      <span className="min-w-0 truncate font-medium" title={me.name}>{me.name || 'Signed in'}</span>
      <Button variant="link" size="sm" onClick={leave}>Sign out</Button>
      {error && <InlineError>Sign-out failed: {error}</InlineError>}
    </Inline>
  )
}

export function Nav() {
  const current = sectionOf(usePathname())
  const { project, staff } = useTenant()

  return (
    // The nav is a sand field beside the paper of the page; the current page is a card lifted out of it.
    <nav aria-label="Main" className="flex h-screen w-60 flex-none flex-col overflow-y-auto border-r bg-surface-inset">
      <Link href={HOME_PATH} className="px-5 pb-2 pt-6">
        <Logo />
      </Link>
      <ProjectSwitcher />
      <div className="mb-3 mt-4 px-3">
        <CommandPaletteTrigger />
      </div>
      <ul className="flex flex-col gap-0.5 px-3">
        {navItems.filter(item => staff || !item.staffOnly).map(({ section, label, icon: Icon }) => {
          const active = current === section
          return (
            <li key={section}>
              <Link
                href={sectionPath(section, project)}
                aria-current={active ? 'page' : undefined}
                className={clsx(
                  'flex items-center gap-3 rounded border px-3 py-2 text-sm transition-colors',
                  active ? 'border-border bg-surface text-foreground' : 'border-transparent text-muted hover:bg-hover hover:text-foreground',
                )}
              >
                <Icon aria-hidden="true" size={16} strokeWidth={1.75} className="flex-none" />
                {label}
              </Link>
            </li>
          )
        })}
      </ul>
      <Stack className="mt-auto px-3 pb-5 pt-6">
        <SystemStatus />
        <Account />
        <Stack gap="2xs" className="border-t pt-3">
          <Inline nowrap className="justify-between">
            <Eyebrow>HypothesisLoop</Eyebrow>
            <ThemeSwitch />
          </Inline>
        </Stack>
      </Stack>
    </nav>
  )
}
