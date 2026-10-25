'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { checkGitHubInstallation, fetchGitHubInstallations, fetchGitHubRepositories, GITHUB_NOT_CONFIGURED } from '@/lib/api/github'
import { checkRepository } from '@/lib/api/platform-experiments'
import { ApiError } from '@/lib/api/request'
import { clsx } from 'clsx'
import { installationSettingsURL, useGitHubApp } from '@/lib/github-connect'
import { useTenant } from '@/lib/tenant'
import { ConnectGitHubRow, useConnectGitHub } from '@/components/connect-github-step'
import { GitHubProblem, InstallProgress } from '@/components/github-status'
import { controlHeight } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { textLinkClass } from '@/components/ui/link'
import { SearchableSelect } from '@/components/ui/searchable-select'
import { InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { keys } from '@/lib/keys'
import type { OrgGitHubInstallation } from '@/types/api'

const LISTED = 50

// The value that stands for a new repository on one connected account, when there are several.
const ON_ACCOUNT = '@'

/** The branch a named repository is worked on at until the person names another. */
export const DEFAULT_BRANCH = 'main'

/** Whether the value picked names a repository, which is worked on at a branch the person names. */
export function namesRepository(value: string): boolean {
  return value !== '' && !value.startsWith(ON_ACCOUNT)
}

/** What the repository check sends for the value picked: a repository, the account a new one goes to, or neither. */
export function repositoryFields(value: string): { repository?: string; repository_account?: string } {
  if (value === '') return {}
  return value.startsWith(ON_ACCOUNT) ? { repository_account: value.slice(ON_ACCOUNT.length) } : { repository: value }
}

/** What a create sends for the value picked: a repository at its branch, the account a new one goes to, or neither. */
export function codeFields(value: string, branch: string): { repository?: string; repository_account?: string; branch?: string } {
  return namesRepository(value) ? { repository: value, branch: branch.trim() } : repositoryFields(value)
}

// The installations GitHub says are unusable, asked by the check Settings runs on each: removed,
// suspended, or without a permission the App asks for. A check that failed says nothing of one.
async function unusable(orgID: string, accounts: OrgGitHubInstallation[]): Promise<number[]> {
  const found = await Promise.all(accounts.map(a => checkGitHubInstallation(orgID, a.installation_id).then(
    check => check.working ? null : a.installation_id,
    () => null,
  )))
  return found.filter((id): id is number => id != null)
}

// The connected account a value concerns: the owner of the repository named, the account named for
// a new one, or the account a new one goes to unasked.
function accountOf(value: string, accounts: OrgGitHubInstallation[], unasked?: OrgGitHubInstallation): OrgGitHubInstallation | undefined {
  const owner = value.startsWith(ON_ACCOUNT) ? value.slice(ON_ACCOUNT.length) : value.split('/')[0]
  return owner ? accounts.find(a => a.account_login.toLowerCase() === owner.toLowerCase()) : unasked
}

/**
 * Runs the create's own repository checks for the value picked, so a GitHub account the App left
 * or a repository it cannot push to shows here, as one chip and the one action that fixes it,
 * before anything is created.
 */
function RepositoryCheckNote({ value, account, onInstall }: { value: string; account?: OrgGitHubInstallation; onInstall?: () => void }) {
  const { project } = useTenant()
  const { error, isValidating, mutate } = useSWR(keys.repositoryCheck(project.id, value), () => checkRepository(project.id, repositoryFields(value)))
  if (!error) return null
  return (
    <GitHubProblem
      reason={error instanceof ApiError ? error.reason : undefined}
      message={error.message}
      account={account?.account_login ?? ''}
      installation={account}
      onInstall={onInstall}
      onRetry={() => void mutate()}
      retrying={isValidating}
    />
  )
}

/**
 * Picks the repository a platform experiment works on, from those the org's connected GitHub
 * accounts cover. Typing searches all of them on the server, which reads GitHub on each call.
 * With none chosen the platform creates a new private one from the experiment template, on the
 * org's default account or the only one, and with several the person may pick another. With no
 * account connected it is the way to connect one. On a deployment whose git host is not GitHub
 * the repository is typed. What is picked is checked as the create would check it.
 */
export function RepositorySelect({ value, onChange, invalid = false, describedBy }: {
  value: string
  onChange: (repository: string) => void
  /** Marks the field as what stops the create. */
  invalid?: boolean
  describedBy?: string
}) {
  const { org } = useTenant()
  const editsOrg = org.actions.includes('edit')
  const [q, setQ] = useState('')
  const app = useGitHubApp()
  const { install, start, reset } = useConnectGitHub()
  const onInstall = editsOrg && app.data ? start : undefined
  const installations = useSWR(app.data ? keys.githubInstallations(org.id) : null, () => fetchGitHubInstallations(org.id))
  const connected = (installations.data?.total ?? 0) > 0
  const listedAccounts = installations.data?.items ?? []
  const dead = useSWR(
    listedAccounts.length > 0 ? keys.githubInstallationChecks(org.id, listedAccounts.map(a => a.installation_id)) : null,
    () => unusable(org.id, listedAccounts),
  ).data ?? []
  const { data, error } = useSWR(
    connected ? keys.githubRepositories(org.id, q) : null,
    () => fetchGitHubRepositories(org.id, { q, limit: LISTED }),
    { keepPreviousData: true },
  )

  if (app.error instanceof ApiError && app.error.reason === GITHUB_NOT_CONFIGURED) {
    return <Input className="w-full" aria-label="Repository" placeholder="owner/name" invalid={invalid} aria-describedby={describedBy} value={value} onChange={e => onChange(e.target.value)} />
  }
  if (app.error) return <InlineError>Could not read the deployment&apos;s git host: {app.error.message}</InlineError>
  if (installations.error) return <InlineError>Could not load the connected GitHub accounts: {installations.error.message}</InlineError>
  if (!installations.data) return <div aria-hidden className={clsx(controlHeight.default, 'animate-pulse rounded border border-border-control bg-surface-inset')} />
  if (!connected) return <ConnectGitHubRow invalid={invalid} describedBy={describedBy} />

  const newLabel = 'New repository'
  const listed = data?.items ?? []
  const more = (data?.total ?? 0) - listed.length
  // What is chosen stays an option while a search leaves it out.
  const options = listed.map(r => ({ value: r.repository, label: r.repository, badge: r.private ? 'private' : undefined }))
  if (value && !value.startsWith(ON_ACCOUNT) && !listed.some(r => r.repository === value)) options.unshift({ value, label: value, badge: undefined })
  // A new repository goes to the org's default or else the only working account unasked, and the
  // person may name another working one. One GitHub says is unusable is offered for none, and one
  // connected before its type was kept takes no new repository until it is connected again.
  const accounts = listedAccounts
  const creating = accounts.filter(a => a.account_type != null && !dead.includes(a.installation_id))
  const unasked = creating.find(a => a.default) ?? (creating.length === 1 ? creating[0] : undefined)
  const createsUnasked = unasked != null
  options.unshift(...creating.filter(a => a !== unasked).map(a => ({ value: ON_ACCOUNT + a.account_login, label: `${newLabel} on ${a.account_login}`, badge: undefined })))
  const unaskedLabel = unasked ? `${newLabel} on ${unasked.account_login}` : newLabel
  // GitHub lists only the repositories an installation was given; its settings widen that, and the
  // list is read again when this window takes focus back.
  const access = accountOf(value, accounts, unasked) ?? accounts[0]
  return (
    <>
      <SearchableSelect
        fieldSize="default"
        className="w-full"
        aria-label="Repository"
        value={value}
        onChange={onChange}
        onSearch={setQ}
        allLabel={unaskedLabel}
        placeholder={createsUnasked ? unaskedLabel : 'Choose a repository'}
        hideAllOption={!createsUnasked}
        options={options}
        hint={more > 0 ? `${more} more, type to search` : undefined}
      />
      {access && (
        <Text variant="hint">
          Missing a repository? <a href={installationSettingsURL(access)} target="_blank" rel="noreferrer" className={textLinkClass}>Adjust GitHub access</a>
        </Text>
      )}
      <InstallProgress install={install} onCancel={reset} />
      {error && <InlineError>Could not list the repositories: {error.message}</InlineError>}
      {(value !== '' || createsUnasked) && <RepositoryCheckNote value={value} account={accountOf(value, accounts, unasked)} onInstall={onInstall} />}
    </>
  )
}
