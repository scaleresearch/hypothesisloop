'use client'

import type { ReactNode } from 'react'
import type { Tone } from '@/lib/colors'
import { installationSettingsURL, type Install } from '@/lib/github-connect'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { inlineLinkClass } from '@/components/ui/link'
import { Inline } from '@/components/ui/stack'
import { InlineError, Spinner } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { useTenant } from '@/lib/tenant'
import type { OrgGitHubInstallation } from '@/types/api'

/** A state in a short chip, with its explanation on hover and focus and to a screen reader. */
export function StatusChip({ tone, label, explain }: { tone: Tone; label: string; explain: string }) {
  return (
    <span tabIndex={0} title={explain} className="inline-flex cursor-help rounded-full outline-none focus-visible:ring-2 focus-visible:ring-accent">
      <Badge tone={tone}>{label}</Badge>
      <span className="sr-only">: {explain}</span>
    </span>
  )
}

/** What fixes a GitHub problem: installing again from here, or the installation's page on GitHub. */
type Fix = 'install' | 'review' | 'retry'

interface Problem {
  tone: Tone
  label: string
  explain: string
  fix: Fix
}

// The permissions a refusal names, as name:level.
function permissionsIn(message: string): string {
  return (message.match(/\b[a-z_]+:(read|write|admin)\b/g) ?? []).map(named => {
    const [name, level] = named.split(':')
    return `${name.replaceAll('_', ' ')} (${level})`
  }).join(', ')
}

/**
 * A refusal about GitHub as a short status: the reason code picks the chip and its one fix, and
 * the API's message is shown only where no plain sentence says it better. An install is an
 * owner's, so a member is told an owner does it.
 */
function githubProblem(reason: string | undefined, message: string, account: string, owner: boolean): Problem {
  const on = account ? ` on ${account}` : ''
  switch (reason) {
    case 'installation_gone':
      return { tone: 'danger', label: 'Removed', explain: `GitHub access was removed${on}, so experiments cannot use its repositories. ${owner ? 'Reconnect GitHub' : 'Ask an owner of your organization to reconnect GitHub'} to use them again.`, fix: 'install' }
    case 'installation_suspended':
      return { tone: 'warning', label: 'Suspended', explain: `GitHub access is suspended${on}. Unsuspend it on GitHub to use its repositories again.`, fix: 'review' }
    case 'git_host_permission_missing': {
      const missing = permissionsIn(message)
      return { tone: 'warning', label: 'Missing permission', explain: `GitHub access${on} is missing ${missing || 'a permission it needs'}. Accept the permissions on GitHub.`, fix: 'review' }
    }
    case 'repository_not_reachable':
      return { tone: 'warning', label: 'No access', explain: `GitHub access does not cover this repository. ${owner ? 'Add it' : 'Ask an owner of your organization to add it'} to the repositories it can reach${on}.`, fix: 'install' }
    case 'repository_creation_not_permitted':
      return { tone: 'warning', label: 'Cannot create', explain: `GitHub does not allow creating repositories${on}. Review the permissions on GitHub, or pick an existing repository.`, fix: 'review' }
    case 'installation_not_connected':
    case 'git_host_not_connected':
      return { tone: 'warning', label: 'Not connected', explain: `No GitHub account${on} is connected. ${owner ? 'Connect it' : 'Ask an owner of your organization to connect it'}.`, fix: 'install' }
    case 'git_host_failed':
    case 'unavailable':
      return { tone: 'neutral', label: 'No answer', explain: 'GitHub did not answer the check. Try again in a moment.', fix: 'retry' }
    default:
      return { tone: 'danger', label: 'Problem', explain: message, fix: 'retry' }
  }
}

/**
 * A GitHub problem as one chip and one action next to it. `onInstall` opens the install in a new
 * tab, `installation` names the installation whose page on GitHub reviews it, `onRetry` checks again.
 */
export function GitHubProblem({ reason, message, account, installation, onInstall, onRetry, retrying }: {
  reason?: string
  message: string
  account: string
  installation?: Pick<OrgGitHubInstallation, 'installation_id' | 'account_login' | 'account_type'>
  onInstall?: () => void
  onRetry: () => void
  retrying?: boolean
}) {
  const { org } = useTenant()
  const problem = githubProblem(reason, message, account, org.actions.includes('edit'))
  let action: ReactNode = <Button type="button" variant="link" size="sm" loading={retrying} onClick={onRetry}>Check again</Button>
  if (problem.fix === 'install' && onInstall) {
    action = <Button type="button" variant="link" size="sm" onClick={onInstall}>{reason === 'installation_gone' ? 'Reconnect' : reason === 'repository_not_reachable' ? 'Add repositories' : 'Connect GitHub'}</Button>
  } else if (problem.fix === 'review' && installation) {
    action = <a href={installationSettingsURL(installation)} target="_blank" rel="noreferrer" className={`${inlineLinkClass} text-xs`}>Review on GitHub</a>
  }
  return (
    <Inline gap="sm" nowrap>
      <StatusChip tone={problem.tone} label={problem.label} explain={problem.explain} />
      {action}
    </Inline>
  )
}

/**
 * Where an install started from this tab stands: waiting for the GitHub tab, with a way to stop
 * waiting, connected, or refused with what to do.
 */
export function InstallProgress({ install, onCancel }: { install: Install; onCancel: () => void }) {
  switch (install.phase) {
    case 'waiting':
      return (
        <Inline gap="sm" role="status">
          <Spinner />
          <Text as="span" variant="hint">Waiting for GitHub. Finish connecting in the GitHub tab.</Text>
          <Button type="button" variant="link" size="sm" onClick={onCancel}>Cancel</Button>
        </Inline>
      )
    case 'connected':
      return <Text variant="note" role="status">{install.installation.account_login} is connected.</Text>
    case 'failed':
      return <InlineError>{install.message}</InlineError>
    default:
      return null
  }
}
