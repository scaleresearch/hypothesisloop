'use client'

import type { ReactNode } from 'react'
import type { Tone } from '@/lib/colors'
import { githubCardPath, installationSettingsURL, type Install } from '@/lib/github-connect'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { inlineLinkClass, TextLink } from '@/components/ui/link'
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

/** The GitHub section of the org's settings, where an owner manages the connection and a member sees it. */
export function GitHubSettingsLink({ orgID }: { orgID: string }) {
  return <TextLink inline href={githubCardPath(orgID)} className="text-xs">See the connection in settings</TextLink>
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
      return { tone: 'danger', label: 'Not installed', explain: `The GitHub App was removed${on}, so experiments cannot use its repositories. ${owner ? 'Reinstall it' : 'An owner of the org reinstalls it'} to connect again.`, fix: 'install' }
    case 'installation_suspended':
      return { tone: 'warning', label: 'Suspended', explain: `The GitHub App is suspended${on}. Unsuspend it on GitHub to use its repositories again.`, fix: 'review' }
    case 'git_host_permission_missing': {
      const missing = permissionsIn(message)
      return { tone: 'warning', label: 'Missing permission', explain: `The GitHub App${on} has not been granted ${missing || 'every permission it asks for'}. Accept the permissions on GitHub.`, fix: 'review' }
    }
    case 'repository_not_reachable':
      return { tone: 'warning', label: 'No access', explain: `The GitHub App cannot reach this repository. ${owner ? 'Add it' : 'An owner of the org adds it'} to the App's repositories${on}.`, fix: 'install' }
    case 'repository_creation_not_permitted':
      return { tone: 'warning', label: 'Cannot create', explain: `GitHub does not let the App create repositories${on}. Review the App's permissions on GitHub, or pick an existing repository.`, fix: 'review' }
    case 'installation_not_connected':
    case 'git_host_not_connected':
      return { tone: 'warning', label: 'Not connected', explain: `No GitHub account${on} is connected. ${owner ? 'Install' : 'An owner of the org installs'} the GitHub App on it.`, fix: 'install' }
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
    action = <Button type="button" variant="link" size="sm" onClick={onInstall}>{reason === 'installation_gone' ? 'Reinstall' : reason === 'repository_not_reachable' ? 'Add repositories' : 'Install GitHub App'}</Button>
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
          <Text as="span" variant="hint">Waiting for GitHub. Finish the install in the new tab.</Text>
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
