'use client'

import type { ReactNode } from 'react'
import useSWR, { SWRConfig, type SWRConfiguration } from 'swr'
import { useParams } from 'next/navigation'
import { fetchMe } from '@/lib/api/accounts'
import { fetchPlatformExperiment } from '@/lib/api/platform-experiments'
import { ApiError } from '@/lib/api/request'
import type { Me } from '@/types/accounts'
import type { PlatformExperiment } from '@/types/platform-experiments'
import { HOME_PATH } from '@/lib/auth'
import { resolveExperimentTenant, resolveOrgTenant, resolveTenant, TenantContext, type TenantResolution } from '@/lib/tenant'
import { Nav } from '@/components/nav'
import { CommandPalette } from '@/components/command-palette'
import { SignOutButton } from '@/components/sign-out-button'
import { Button, ButtonLink } from '@/components/ui/button'
import { Centered } from '@/components/ui/centered'
import { Inline } from '@/components/ui/stack'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'

// A failed read stays failed until the person comes back to the tab, the network returns or they
// try again: no read is retried on a timer, and refreshInterval stops on an error until then.
const SWR_CONFIG: SWRConfiguration = {
  shouldRetryOnError: false,
  revalidateOnFocus: true,
  revalidateOnReconnect: true,
}

// The dynamic segments of the routes that name what they show. A page of an experiment, or of a
// record in it, is shown in the experiment's project, which is read from the experiment itself.
type Place = { projectId?: string; orgId?: string; experimentId?: string }

function Standalone({ children }: { children: ReactNode }) {
  return <Centered wide>{children}</Centered>
}

const goHome = <ButtonLink href={HOME_PATH} variant="primary">Go to your experiments</ButtonLink>

/** Why a read the shell depends on failed, in words that tell a refused session from an API out of reach. */
function failedRead(error: Error, what: string): string {
  if (!(error instanceof ApiError)) return `Cannot reach the API to load ${what}: ${error.message}.`
  if (error.status === 403) return `The API refused your session for ${what}: ${error.message}. Sign out and sign in again, or ask an owner of your organization for access.`
  return `The API could not load ${what}: ${error.message}.`
}

function Failed({ children, onRetry, home }: { children: ReactNode; onRetry: () => void; home?: boolean }) {
  return (
    <Standalone>
      <ErrorMessage>{children}</ErrorMessage>
      <Inline>
        {home && goHome}
        <Button onClick={onRetry}>Try again</Button>
        <SignOutButton />
      </Inline>
    </Standalone>
  )
}

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
  return <SWRConfig value={SWR_CONFIG}><Shell>{children}</Shell></SWRConfig>
}

function Shell({ children }: { children: ReactNode }) {
  // Reread on a timer: that is how a rename made in another tab, or a membership that ended,
  // reaches a page that is already open.
  const { data: me, error, mutate } = useSWR('me', fetchMe, { refreshInterval: 30_000 })
  const place = useParams<Place>()
  const { data: pe, error: peError, mutate: mutatePE } = useSWR(
    place.experimentId ? ['pe', place.experimentId] : null,
    () => fetchPlatformExperiment(place.experimentId as string),
  )

  if (error && !me) return <Failed onRetry={() => void mutate()}>{failedRead(error, 'your account')}</Failed>
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
    if (peError) return <Failed home onRetry={() => void mutatePE()}>{failedRead(peError, `experiment ${place.experimentId}`)}</Failed>
    return <Standalone><Loading text="Loading the experiment…" /></Standalone>
  }

  const resolution = resolve(me, place, pe)
  if (resolution.state === 'none') {
    return (
      <Standalone>
        <ErrorMessage>Your account reaches no project. Ask the owner of your organization to check your membership.</ErrorMessage>
        <SignOutButton />
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
              {failedRead(error, 'your account again')} Your organizations, projects and role are as last read.{' '}
              <Button variant="link" onClick={() => void mutate()}>Try again</Button>
            </ErrorMessage>
          )}
          {children}
        </div>
      </main>
      <CommandPalette key={`palette-${project.id}`} />
    </TenantContext.Provider>
  )
}
