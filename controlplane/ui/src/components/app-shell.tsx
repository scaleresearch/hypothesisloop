'use client'

import type { ReactNode } from 'react'
import useSWR from 'swr'
import { useParams } from 'next/navigation'
import { ApiError, fetchMe, fetchPlatformExperiment } from '@/lib/api'
import { HOME_PATH } from '@/lib/auth'
import { resolveExperimentTenant, resolveOrgTenant, resolveTenant, TenantContext, type TenantResolution } from '@/lib/tenant'
import type { Me, PlatformExperiment } from '@/types'
import { Nav } from '@/components/nav'
import { CommandPalette } from '@/components/command-palette'
import { ButtonLink } from '@/components/ui/button'
import { Centered } from '@/components/ui/centered'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'

// The dynamic segments of the routes that name what they show. A page of an experiment, or of a
// record in it, is shown in the experiment's project, which is read from the experiment itself.
type Place = { projectId?: string; orgId?: string; experimentId?: string }

function Standalone({ children }: { children: ReactNode }) {
  return <Centered wide>{children}</Centered>
}

const goHome = <ButtonLink href={HOME_PATH} variant="primary">Go to your experiments</ButtonLink>

function resolve(me: Me, place: Place, pe: PlatformExperiment | undefined): TenantResolution {
  if (pe) return resolveExperimentTenant(me, pe)
  if (place.orgId) return resolveOrgTenant(me, place.orgId)
  return resolveTenant(me, place.projectId ?? '')
}

/**
 * Everything behind sign-in: asks the API who the session is, resolves the project or org the
 * URL names, or the one the experiment it names belongs to, and renders the app inside it.
 * Pages below never render without a project.
 */
export function AppShell({ children }: { children: ReactNode }) {
  // Reread on focus and on a timer: that is how a rename made in another tab, or a membership
  // that ended, reaches a page that is already open.
  const { data: me, error } = useSWR('me', fetchMe, { refreshInterval: 30_000 })
  const place = useParams<Place>()
  const { data: pe, error: peError } = useSWR(
    place.experimentId ? ['pe', place.experimentId] : null,
    () => fetchPlatformExperiment(place.experimentId as string),
  )

  if (error && !me) {
    return (
      <Standalone>
        <ErrorMessage>Cannot reach the API to load your account: {error.message}. Retrying.</ErrorMessage>
      </Standalone>
    )
  }
  if (!me) return <Standalone><Loading text="Loading your account…" /></Standalone>

  if (place.experimentId && !pe) {
    if (peError instanceof ApiError && peError.status === 404) {
      return (
        <Standalone>
          <ErrorMessage>
            Experiment <Text as="span" variant="code">{place.experimentId}</Text> was not found: it does not exist, or it belongs to an organization you are not in.
          </ErrorMessage>
          {goHome}
        </Standalone>
      )
    }
    if (peError) return <Standalone><ErrorMessage>Could not load experiment {place.experimentId}: {peError.message}. Retrying.</ErrorMessage></Standalone>
    return <Standalone><Loading text="Loading the experiment…" /></Standalone>
  }

  const resolution = resolve(me, place, pe)
  if (resolution.state === 'none') {
    return (
      <Standalone>
        <ErrorMessage>Your account reaches no project. Ask the owner of your organization to check your membership.</ErrorMessage>
      </Standalone>
    )
  }
  if (resolution.state === 'unavailable') {
    return (
      <Standalone>
        <ErrorMessage>
          The {resolution.what} <Text as="span" variant="code">{resolution.id}</Text> is not available to you: it was removed, or your access to it ended.
        </ErrorMessage>
        {goHome}
      </Standalone>
    )
  }

  // Keyed by project, so a switch remounts the page: a token on show, a half-filled form or an
  // open palette belongs to the project it was opened in.
  const { project } = resolution.tenant
  return (
    <TenantContext.Provider value={resolution.tenant}>
      <Nav />
      <main key={project.id} className="min-w-0 flex-1 overflow-auto">
        <div className="mx-auto w-full max-w-6xl px-10 pb-16 pt-10">
          {error && (
            <ErrorMessage className="mb-4">
              Could not reread your account: {error.message}. Your organizations, projects and role are as last read. Retrying.
            </ErrorMessage>
          )}
          {children}
        </div>
      </main>
      <CommandPalette key={`palette-${project.id}`} />
    </TenantContext.Provider>
  )
}
