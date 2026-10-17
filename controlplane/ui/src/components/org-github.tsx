'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { checkGitHubInstallation, disconnectGitHubInstallation, fetchGitHubInstallations, GITHUB_NOT_CONFIGURED, makeGitHubInstallationDefault } from '@/lib/api/github'
import { ApiError } from '@/lib/api/request'
import { githubCardPath, useGitHubApp, useGitHubInstall } from '@/lib/github-connect'
import { errorMessage } from '@/lib/errors'
import { useTenant } from '@/lib/tenant'
import { Actor } from '@/components/actor'
import { GitHubProblem, InstallProgress, StatusChip } from '@/components/github-status'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Table } from '@/components/ui/table'
import { EmptyState } from '@/components/ui/empty-state'
import { Inline, Stack } from '@/components/ui/stack'
import { Callout, ErrorMessage, Loading } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { keys } from '@/lib/keys'
import type { OrgGitHubInstallation } from '@/types/api'

const KIND = { User: 'Personal', Organization: 'Organization' } as const

/**
 * Whether GitHub still lets the App create and push on a connected account, asked of GitHub by
 * the check a create runs there, as one chip and the one action that fixes it.
 */
function InstallationState({ installation, onInstall }: { installation: OrgGitHubInstallation; onInstall?: () => void }) {
  const { org } = useTenant()
  const { data, error, isValidating, mutate } = useSWR(
    keys.githubInstallationCheck(org.id, installation.installation_id),
    () => checkGitHubInstallation(org.id, installation.installation_id),
  )
  // What GitHub says of it, or why the check could not be made.
  const problem = error ? { reason: error instanceof ApiError ? error.reason : undefined, message: String(error.message) }
    : data && !data.working ? { reason: data.reason, message: data.message ?? '' } : null
  if (problem) {
    return (
      <GitHubProblem
        reason={problem.reason}
        message={problem.message}
        account={installation.account_login}
        installation={installation}
        onInstall={onInstall}
        onRetry={() => void mutate()}
        retrying={isValidating}
      />
    )
  }
  if (!data) return <Text as="span" variant="hint">Checking…</Text>
  return <StatusChip tone="success" label="Working" explain={`The GitHub App can create and push to repositories on ${installation.account_login}.`} />
}

/**
 * The GitHub accounts the org connected through the platform's GitHub App. An experiment of the
 * org may name any repository such an account's installation covers. An owner connects an
 * account by installing the App on it, in a new tab this page waits for, and disconnects one; a
 * member reads the list.
 */
export function OrgGitHub() {
  const { me, org } = useTenant()
  const editsOrg = org.actions.includes('edit')
  const { data, error, mutate } = useSWR(keys.githubInstallations(org.id), () => fetchGitHubInstallations(org.id))
  const app = useGitHubApp(editsOrg)
  const { install, start, reset } = useGitHubInstall(org.id, githubCardPath(org.id))
  const [disconnecting, setDisconnecting] = useState<OrgGitHubInstallation | null>(null)
  const [defaulting, setDefaulting] = useState<number | null>(null)
  const [actionError, setActionError] = useState('')

  async function makeDefault(installation: OrgGitHubInstallation) {
    setDefaulting(installation.installation_id)
    setActionError('')
    try {
      await makeGitHubInstallationDefault(org.id, installation.installation_id)
      await mutate()
    } catch (e) {
      setActionError(errorMessage(e))
    } finally {
      setDefaulting(null)
    }
  }

  async function disconnect(installation: OrgGitHubInstallation) {
    await disconnectGitHubInstallation(org.id, installation.installation_id)
    if (install.phase === 'connected' && install.installation.installation_id === installation.installation_id) reset()
    await mutate()
  }

  const unconfigured = app.error instanceof ApiError && app.error.reason === GITHUB_NOT_CONFIGURED
  const canInstall = editsOrg && app.data != null
  const installations = data?.items

  return (
    <Stack>
      {editsOrg && unconfigured && (
        <Callout tone="neutral">This deployment has no GitHub App configured, so no GitHub account can be connected.</Callout>
      )}
      {editsOrg && app.error && !unconfigured && <ErrorMessage>GitHub cannot be connected right now: {app.error.message}</ErrorMessage>}
      {actionError && <ErrorMessage>{actionError}</ErrorMessage>}

      {error && <ErrorMessage>Could not load the GitHub accounts of {org.name}: {error.message}</ErrorMessage>}
      {!data && !error && <Loading />}
      {installations && installations.length === 0 && (
        <EmptyState>
          No GitHub account is connected to {org.name}, and an experiment needs one: it keeps its code in a repository there.
          {editsOrg ? '' : ' An owner installs the GitHub App here.'}
        </EmptyState>
      )}
      {installations && installations.length > 0 && (
        <Table aria-label="Connected GitHub accounts">
          <thead>
            <tr><th>GitHub account</th><th>Kind</th><th>On GitHub</th>{installations.length > 1 && <th>New repositories</th>}<th>Connected by</th>{editsOrg && <th />}</tr>
          </thead>
          <tbody>
            {installations.map(installation => (
              <tr key={installation.installation_id}>
                <td className="font-mono text-xs font-medium">{installation.account_login}</td>
                <td className="text-xs text-muted">{installation.account_type ? KIND[installation.account_type] : ''}</td>
                <td className="whitespace-nowrap"><InstallationState installation={installation} onInstall={canInstall ? start : undefined} /></td>
                {installations.length > 1 && (
                  <td>
                    {installation.default ? <Badge tone="accent">Default</Badge>
                      : editsOrg ? <Button size="sm" loading={defaulting === installation.installation_id} disabled={defaulting !== null} onClick={() => void makeDefault(installation)}>Make default</Button> : null}
                  </td>
                )}
                <td title={new Date(installation.connected_at).toLocaleString()}>
                  <Actor actor={installation.connected_actor} self={installation.connected_actor.kind === 'user' && installation.connected_actor.id === me.user_id} />
                </td>
                {editsOrg && (
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

      {canInstall && (
        <Stack gap="xs" start>
          <Inline>
            <Button size="sm" variant="primary" disabled={install.phase === 'waiting'} onClick={start}>Install GitHub App</Button>
            <Text as="span" variant="hint">Install it on your account or an organization.</Text>
          </Inline>
          <InstallProgress install={install} onCancel={reset} />
        </Stack>
      )}
      {disconnecting && (
        <ConfirmDialog
          open
          title={`Disconnect ${disconnecting.account_login} from ${org.name}?`}
          confirmLabel="Disconnect"
          onConfirm={() => disconnect(disconnecting)}
          onClose={() => setDisconnecting(null)}
        >
          Experiments that name one of its repositories keep running, but their agents and jobs can no longer
          clone, fetch or push within the hour. The App stays installed on GitHub.
        </ConfirmDialog>
      )}
    </Stack>
  )
}
