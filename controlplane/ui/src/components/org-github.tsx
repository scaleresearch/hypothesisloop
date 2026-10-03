'use client'

import { useEffect, useState } from 'react'
import useSWR from 'swr'
import { ApiError, GITHUB_NOT_CONFIGURED, disconnectGitHubInstallation, fetchGitHubInstallations } from '@/lib/api'
import { beginConnect, beginInstall, leaveFor, resetConnect, useConnect, useGitHubApp } from '@/lib/github-connect'
import { useTenant } from '@/lib/tenant'
import type { GitHubApp, GitHubInstallation } from '@/types'
import { Actor } from '@/components/actor'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Table } from '@/components/ui/table'
import { Callout, EmptyState, ErrorMessage, Loading } from '@/components/ui/status-message'

const KIND = { User: 'Personal', Organization: 'Organisation' } as const

/**
 * The GitHub accounts the org connected through the platform's GitHub App. An experiment
 * of the org may name any repository such an account's installation covers. An owner connects an
 * account with one button, which installs the App on GitHub first where it is not installed, and
 * disconnects one; a member reads the list.
 */
export function OrgGitHub() {
  const { me, org, project, administers } = useTenant()
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

  const origin = { orgID: org.id, projectID: project.id }
  function leave(address: string) {
    setLeaving(true)
    leaveFor(address)
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
    <div className="grid gap-3.5">
      <p className="text-sm text-muted">
        An experiment of {org.name} keeps its code in a repository of a GitHub account connected here, and that
        account&apos;s owner chooses on GitHub which repositories the App reaches.
      </p>

      {outcome?.state === 'refused' && <ErrorMessage>{outcome.message}</ErrorMessage>}
      {outcome?.state === 'failed' && <ErrorMessage>{outcome.message}</ErrorMessage>}
      {outcome?.state === 'choice' && github && (
        <div role="group" aria-label="GitHub account to connect" className="grid justify-items-start gap-2 rounded border border-border-strong bg-surface-inset px-3.5 py-3">
          <p className="text-sm">Which GitHub account do you connect to {org.name}?</p>
          <div className="flex flex-wrap gap-2">
            {outcome.installations.map(i => (
              <Button key={i.installation_id} size="sm" disabled={leaving} onClick={() => leave(beginConnect(github.authorize_url, origin, i.installation_id))}>
                {i.account_login}
              </Button>
            ))}
            <Button size="sm" disabled={leaving} onClick={() => leave(beginInstall(github.install_url, origin))}>Another account</Button>
            <Button size="sm" variant="ghost" disabled={leaving} onClick={resetConnect}>Cancel</Button>
          </div>
        </div>
      )}

      {administers && unconfigured && (
        <Callout tone="neutral" className="mb-0">This deployment has no GitHub App configured, so no GitHub account can be connected.</Callout>
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
            <tr><th>GitHub account</th><th>Kind</th><th>Connected by</th>{administers && <th />}</tr>
          </thead>
          <tbody>
            {installations.map(installation => (
              <tr key={installation.installation_id}>
                <td className="font-mono text-xs font-semibold">{installation.account_login}</td>
                <td className="text-xs text-muted">{installation.account_type ? KIND[installation.account_type] : ''}</td>
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
        <p className="text-xs text-subtle">Showing the first {data.items.length} of {data.total} connected accounts.</p>
      )}

      {administers && github && (
        <div className="flex flex-wrap items-center gap-2">
          <Button size="sm" variant="primary" loading={leaving} onClick={() => leave(beginConnect(github.authorize_url, origin))}>
            {installations && installations.length > 0 ? 'Connect another account' : 'Connect GitHub'}
          </Button>
          <span className="text-xs text-subtle">Where the App is not installed yet, GitHub asks you to install it first.</span>
        </div>
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
    </div>
  )
}
