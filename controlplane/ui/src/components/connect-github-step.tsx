'use client'

import useSWR from 'swr'
import { fetchGitHubInstallations } from '@/lib/api'
import { useGitHubApp, useGitHubInstall } from '@/lib/github-connect'
import { useTenant } from '@/lib/tenant'
import { InstallProgress } from '@/components/github-status'
import { Button } from '@/components/ui/button'
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

/**
 * Installs the GitHub App from where an experiment is created, in a new tab this page waits for,
 * without leaving the page. A member is told an owner does it.
 */
export function InstallGitHub({ label = 'Install GitHub App' }: { label?: string }) {
  const { org, administers } = useTenant()
  const { install, start, reset } = useGitHubInstall(org.id, typeof window === 'undefined' ? '/' : window.location.pathname + window.location.search)
  if (!administers) return <Text as="span" variant="note">An owner of {org.name} installs the GitHub App to start.</Text>
  return (
    <>
      <Button type="button" variant="primary" disabled={install.phase === 'waiting'} onClick={start}>{label}</Button>
      <InstallProgress install={install} onCancel={reset} />
    </>
  )
}

/** Stands where an experiment is created while the org has connected no GitHub account. */
export function ConnectGitHubStep() {
  const { org } = useTenant()
  return (
    <>
      <Text tone="muted">
        An experiment keeps its code in a GitHub repository, and no GitHub account is connected to {org.name}.
        The platform creates repositories and needs none of your existing ones.
      </Text>
      <InstallGitHub />
    </>
  )
}
