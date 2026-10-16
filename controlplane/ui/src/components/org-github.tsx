'use client'

import { useEffect, useState } from 'react'
import useSWR from 'swr'
import { ApiError, GITHUB_NOT_CONFIGURED, checkGitHubInstallation, disconnectGitHubInstallation, fetchGitHubInstallations, makeGitHubInstallationDefault } from '@/lib/api'
import { beginConnect, beginInstall, githubCardPath, leaveFor, resetConnect, useConnect, useGitHubApp } from '@/lib/github-connect'
import { useTenant } from '@/lib/tenant'
import type { GitHubApp, GitHubInstallation } from '@/types'
import { Actor } from '@/components/actor'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Table } from '@/components/ui/table'
import { InsetCard } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { Inline, Stack } from '@/components/ui/stack'
import { Callout, ErrorMessage, InlineError, Loading } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'

const KIND = { User: 'Personal', Organization: 'Organisation' } as const

/**
 * Whether GitHub still lets the App create and push on a connected account, asked of GitHub by
 * the check a create runs there. A refusal says what to do, with the way to GitHub.
 */
function InstallationState({ installation, app }: { installation: GitHubInstallation; app?: GitHubApp }) {
  const { org } = useTenant()
  const { data, error, isValidating, mutate } = useSWR(
    ['github-installation-check', org.id, installation.installation_id],
    () => checkGitHubInstallation(org.id, installation.installation_id).then(() => true),
    { shouldRetryOnError: false },
  )
  if (error) {
    return (
      <InlineError>
        {error.message}{' '}
        {app && <><a href={app.install_url} target="_blank" rel="noreferrer" className="underline">Open the App on GitHub</a>,{' '}</>}
        <Button type="button" variant="link" size="sm" loading={isValidating} onClick={() => void mutate()}>check again</Button>
      </InlineError>
    )
  }
  if (!data) return <Text as="span" variant="hint">Checking…</Text>
  return <Badge tone="success">Working</Badge>
}

/**
 * The GitHub accounts the org connected through the platform's GitHub App. An experiment
 * of the org may name any repository such an account's installation covers. An owner connects an
 * account with one button, which installs the App on GitHub first where it is not installed, and
 * disconnects one; a member reads the list.
 */
export function OrgGitHub() {
  const { me, org, administers } = useTenant()
  const connect = useConnect()
  const { data, error, mutate } = useSWR(['github-installations', org.id], () => fetchGitHubInstallations(org.id))
  const app = useGitHubApp(administers)
  const [leaving, setLeaving] = useState(false)
  const [disconnecting, setDisconnecting] = useState<GitHubInstallation | null>(null)
  const [actionError, setActionError] = useState('')

  // What a connect of another org left behind says nothing here.
  const outcome = 'orgID' in connect && connect.orgID !== org.id ? null : connect
  const connected = outcome?.state === 'connected' ? outcome.installation : null
  useEffect(() => {
    if (connected) void mutate()
  }, [connected, mutate])

  const origin = { orgID: org.id, returnTo: githubCardPath(org.id) }
  function leave(address: string) {
    setLeaving(true)
    leaveFor(address)
  }

  async function makeDefault(installation: GitHubInstallation) {
    setActionError('')
    try {
      await makeGitHubInstallationDefault(org.id, installation.installation_id)
      await mutate()
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e))
    }
  }

  async function disconnect(installation: GitHubInstallation) {
    setDisconnecting(null)
    setActionError('')
    try {
      await disconnectGitHubInstallation(org.id, installation.installation_id)
      if (connected?.installation_id === installation.installation_id) resetConnect()
      await mutate()
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e))
    }
  }

  const unconfigured = app.error instanceof ApiError && app.error.reason === GITHUB_NOT_CONFIGURED
  const github: GitHubApp | undefined = app.data
  const installations = data?.items

  return (
    <Stack>
      <Text tone="muted">
        An experiment of {org.name} keeps its code in a repository of a GitHub account connected here, and that
        account&apos;s owner chooses on GitHub which repositories the App reaches.
      </Text>

      {outcome?.state === 'refused' && <ErrorMessage>{outcome.message}</ErrorMessage>}
      {outcome?.state === 'failed' && <ErrorMessage>{outcome.message}</ErrorMessage>}
      {outcome?.state === 'choice' && github && (
        <InsetCard edge="strong" start role="group" aria-label="GitHub account to connect">
          <Text>Which GitHub account do you connect to {org.name}?</Text>
          <Inline>
            {outcome.installations.map(i => (
              <Button key={i.installation_id} size="sm" disabled={leaving} onClick={() => leave(beginConnect(github.authorize_url, origin, i.installation_id))}>
                {i.account_login}
              </Button>
            ))}
            <Button size="sm" disabled={leaving} onClick={() => leave(beginInstall(github.install_url, origin))}>Another account</Button>
            <Button size="sm" variant="ghost" disabled={leaving} onClick={resetConnect}>Cancel</Button>
          </Inline>
        </InsetCard>
      )}

      {administers && unconfigured && (
        <Callout tone="neutral">This deployment has no GitHub App configured, so no GitHub account can be connected.</Callout>
      )}
      {administers && app.error && !unconfigured && <ErrorMessage>GitHub cannot be connected right now: {app.error.message}</ErrorMessage>}
      {actionError && <ErrorMessage>{actionError}</ErrorMessage>}

      {error && <ErrorMessage>Could not load the GitHub accounts of {org.name}: {error.message}</ErrorMessage>}
      {!data && !error && <Loading />}
      {installations && installations.length === 0 && (
        <EmptyState>
          No GitHub account is connected to {org.name}, and an experiment needs one: it keeps its code in a repository there.
          {administers ? '' : ' An owner connects one here.'}
        </EmptyState>
      )}
      {installations && installations.length > 0 && (
        <Table aria-label="Connected GitHub accounts">
          <thead>
            <tr><th>GitHub account</th><th>Kind</th><th>On GitHub</th>{installations.length > 1 && <th>New repositories</th>}<th>Connected by</th>{administers && <th />}</tr>
          </thead>
          <tbody>
            {installations.map(installation => (
              <tr key={installation.installation_id}>
                <td className="font-mono text-xs font-medium">{installation.account_login}</td>
                <td className="text-xs text-muted">{installation.account_type ? KIND[installation.account_type] : ''}</td>
                <td><InstallationState installation={installation} app={github} /></td>
                {installations.length > 1 && (
                  <td>
                    {installation.default ? <Badge tone="accent">Default</Badge>
                      : administers ? <Button size="sm" onClick={() => void makeDefault(installation)}>Make default</Button> : null}
                  </td>
                )}
                <td title={new Date(installation.connected_at).toLocaleString()}>
                  <Actor actor={installation.connected_actor} self={installation.connected_actor.kind === 'user' && installation.connected_actor.id === me.user_id} />
                </td>
                {administers && (
                  <td className="text-right"><Button size="sm" variant="danger" onClick={() => setDisconnecting(installation)}>Disconnect</Button></td>
                )}
              </tr>
            ))}
          </tbody>
        </Table>
      )}
      {data && data.total > data.items.length && (
        <Text variant="hint">Showing the first {data.items.length} of {data.total} connected accounts.</Text>
      )}

      {administers && github && (
        <Inline>
          <Button size="sm" variant="primary" loading={leaving} onClick={() => leave(beginConnect(github.authorize_url, origin))}>
            {installations && installations.length > 0 ? 'Connect another account' : 'Connect GitHub'}
          </Button>
          <Text as="span" variant="hint">Where the App is not installed yet, GitHub asks you to install it first.</Text>
        </Inline>
      )}
      {disconnecting && (
        <ConfirmDialog
          open
          title={`Disconnect ${disconnecting.account_login} from ${org.name}?`}
          confirmLabel="Disconnect"
          onConfirm={() => disconnect(disconnecting)}
          onCancel={() => setDisconnecting(null)}
        >
          Experiments that name one of its repositories keep running, but their agents and jobs can no longer
          clone, fetch or push within the hour. The App stays installed on GitHub.
        </ConfirmDialog>
      )}
    </Stack>
  )
}
