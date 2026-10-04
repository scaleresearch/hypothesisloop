'use client'

import type { ReactNode } from 'react'
import { usePathname } from 'next/navigation'
import { PageNav } from '@/components/ui/page'
import { LinkTabs } from '@/components/ui/tabs'
import { experimentPath } from '@/lib/routes'

const SECTIONS = [
  { section: undefined, label: 'Overview' },
  { section: 'hypotheses', label: 'Hypotheses' },
  { section: 'jobs', label: 'Jobs' },
  { section: 'agents', label: 'Agents' },
] as const

export default function ExperimentLayout({ params, children }: { params: { experimentId: string }; children: ReactNode }) {
  const pathname = usePathname()
  const base = experimentPath(params.experimentId)
  // A hypothesis or a job opens under the tab that lists it.
  const current = pathname.slice(base.length).split('/')[1]
  const tabs = SECTIONS.map(({ section, label }) => ({
    href: experimentPath(params.experimentId, section),
    label,
    active: current === section,
  }))
  return <PageNav value={<LinkTabs label="Experiment" tabs={tabs} />}>{children}</PageNav>
}
