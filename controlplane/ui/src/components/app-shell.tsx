'use client'

import type { ReactNode } from 'react'
import useSWR from 'swr'
import { fetchMe } from '@/lib/api'
import { usePathname } from 'next/navigation'
import { useDropProject, useProjectParam } from '@/lib/query-state'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { Nav } from '@/components/nav'
import { CommandPalette } from '@/components/command-palette'
import { Button } from '@/components/ui/button'
import { Loading, ErrorMessage } from '@/components/ui/status-message'

// The pages that open one record by its id.
const RECORD_PAGE = /^\/(platform-experiments|jobs|hypotheses)\/[^/]+$/

function Standalone({ children }: { children: ReactNode }) {
  return <main className="mx-auto mt-20 grid w-full max-w-xl content-start gap-4 px-8">{children}</main>
}

/**
 * Everything behind sign-in: asks the API who the session is, resolves the project the URL names,
 * and renders the app inside it. Pages below never render without a project.
 */
export function AppShell({ children }: { children: ReactNode }) {
  // Reread on focus and on a timer: that is how a rename made in another tab, or a membership
  // that ended, reaches a page that is already open.
  const { data: me, error } = useSWR('me', fetchMe, { refreshInterval: 30_000 })
  const [projectID, selectProject] = useProjectParam()
  // A record is reached by its id whatever project the link named, and one of a shared platform
  // experiment is read from any project. So a project the caller does not reach is dropped
  // from the address of a record, which then opens in their own or says it is not found.
  const foreignRecord = RECORD_PAGE.test(usePathname()) && me !== undefined && resolveTenant(me, projectID).state === 'unavailable'
  useDropProject(foreignRecord)

  if (error && !me) {
    return (
      <Standalone>
        <ErrorMessage>Cannot reach the API to load your account: {error.message}. Retrying.</ErrorMessage>
      </Standalone>
    )
  }
  if (!me) return <Standalone><Loading text="Loading your account…" /></Standalone>

  const resolution = resolveTenant(me, foreignRecord ? '' : projectID)
  if (resolution.state === 'none') {
    return (
      <Standalone>
        <ErrorMessage>Your account reaches no project. Ask the owner of your organization to check your membership.</ErrorMessage>
      </Standalone>
    )
  }
  if (resolution.state === 'unavailable') {
    const home = resolveTenant(me, '')
    return (
      <Standalone>
        <ErrorMessage>
          Project <span className="font-mono text-xs">{resolution.projectID}</span> is not available to you: it was removed, or your access to its organization ended.
        </ErrorMessage>
        {home.state === 'selected' && (
          <Button variant="primary" onClick={() => selectProject(home.tenant.project.id)}>
            Go to {home.tenant.org.name} / {home.tenant.project.name}
          </Button>
        )}
      </Standalone>
    )
  }

  // Keyed by project, so a switch remounts the page: a token on show, a half-filled form or an
  // open palette belongs to the project it was opened in.
  const { project } = resolution.tenant
  return (
    <TenantContext.Provider value={resolution.tenant}>
      <Nav />
      <main key={project.id} className="max-w-[1220px] flex-1 overflow-auto px-8 py-7">
        {error && (
          <ErrorMessage className="mb-4">
            Could not reread your account: {error.message}. Your organizations, projects and role are as last read. Retrying.
          </ErrorMessage>
        )}
        {children}
      </main>
      <CommandPalette key={`palette-${project.id}`} />
    </TenantContext.Provider>
  )
}
