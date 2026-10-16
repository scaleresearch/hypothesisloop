'use client'

import type { ReactNode } from 'react'
import { usePathname } from 'next/navigation'
import { PageNav } from '@/components/ui/page'
import { LinkTabs } from '@/components/ui/tabs'
import { agentTypesPath, orgPath } from '@/lib/routes'

export default function SettingsLayout({ params, children }: { params: { orgId: string }; children: ReactNode }) {
  const pathname = usePathname()
  const types = agentTypesPath(params.orgId)
  const onTypes = pathname.startsWith(types)
  const tabs = [
    { href: orgPath(params.orgId, 'settings'), label: 'General', active: !onTypes },
    { href: types, label: 'Agent types', active: onTypes },
  ]
  return <PageNav value={<LinkTabs label="Settings" tabs={tabs} />}>{children}</PageNav>
}
