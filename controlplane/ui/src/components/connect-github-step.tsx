'use client'

import useSWR from 'swr'
import { fetchGitHubInstallations } from '@/lib/api'
import { githubCardPath, useGitHubApp } from '@/lib/github-connect'
import { useTenant } from '@/lib/tenant'
import { TextLink } from '@/components/ui/link'
import { Text } from '@/components/ui/text'

/**
 * Where the org stands with GitHub when it creates an experiment. On a deployment whose git host
 * is GitHub the org either has an account connected, to name a repository on or have one created
 * on, or must connect one first. Undefined while it is being read, and on a deployment whose git
 * host is another one, where the repository is named by hand.
 */
export function useGitHub(): 'connected' | 'unconnected' | undefined {
  const { org } = useTenant()
  const app = useGitHubApp()
  const installations = useSWR(app.data ? ['github-installations', org.id] : null, () => fetchGitHubInstallations(org.id))
  if (!app.data || !installations.data) return undefined
  return installations.data.total > 0 ? 'connected' : 'unconnected'
}

/** Whether the org must connect GitHub before it creates an experiment. */
export function useNeedsGitHub(): boolean {
  return useGitHub() === 'unconnected'
}

/** Stands where an experiment is created while the org has connected no GitHub account. */
export function ConnectGitHubStep() {
  const { org } = useTenant()
  return (
    <Text tone="muted">
      An experiment keeps its code in a GitHub repository, and no GitHub account is connected to {org.name}.{' '}
      <TextLink inline href={githubCardPath(org.id)}>Connect GitHub in Settings</TextLink>{' '}
      first. The platform asks to create repositories and needs none of your existing ones: select one only to run an experiment in a repository you already have.
    </Text>
  )
}
