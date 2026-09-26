'use client'

import useSWR from 'swr'
import { fetchGitHubApp, fetchGitHubInstallations } from '@/lib/api'
import { GITHUB_CARD } from '@/lib/github-connect'
import { useTenant } from '@/lib/tenant'
import { TextLink } from '@/components/ui/link'

/**
 * Whether the org must connect GitHub before it creates an experiment: the deployment has a
 * GitHub App, so an experiment keeps its code in a repository, and the org has connected no
 * account to name one on or have one created on.
 */
export function useNeedsGitHub(): boolean {
  const { org } = useTenant()
  const app = useSWR(['github-app'], fetchGitHubApp, { shouldRetryOnError: false })
  const installations = useSWR(app.data ? ['github-installations', org.id] : null, () => fetchGitHubInstallations(org.id))
  return !!app.data && installations.data?.total === 0
}

/** Stands where an experiment is created while the org has connected no GitHub account. */
export function ConnectGitHubStep() {
  const { org } = useTenant()
  return (
    <p className="text-sm text-muted">
      An experiment keeps its code in a GitHub repository, and no GitHub account is connected to {org.name}.{' '}
      <TextLink inline href={`/settings#${GITHUB_CARD}`}>Connect GitHub in Settings</TextLink>{' '}
      first. The platform asks to create repositories and needs none of your existing ones: select one only to run an experiment in a repository you already have.
    </p>
  )
}
