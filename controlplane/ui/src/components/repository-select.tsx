'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { ApiError, GITHUB_NOT_CONFIGURED, checkRepository, fetchGitHubInstallations, fetchGitHubRepositories } from '@/lib/api'
import { useGitHubApp, useGitHubInstall } from '@/lib/github-connect'
import { useTenant } from '@/lib/tenant'
import type { GitHubInstallation } from '@/types'
import { GitHubProblem, InstallProgress } from '@/components/github-status'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { inlineLinkClass } from '@/components/ui/link'
import { SearchableSelect } from '@/components/ui/searchable-select'
import { InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'

const LISTED = 50

// The value that stands for a new repository on one connected account, when there are several.
const ON_ACCOUNT = '@'

/** What a create sends for the value picked: a repository, the account a new one goes to, or neither. */
export function repositoryFields(value: string): { repository?: string; repository_account?: string } {
  if (value === '') return {}
  return value.startsWith(ON_ACCOUNT) ? { repository_account: value.slice(ON_ACCOUNT.length) } : { repository: value }
}

// GitHub's form for a new repository, private and on the connected account when there is one.
function newRepositoryURL(form: string, accounts: GitHubInstallation[]): string {
  const url = new URL(form)
  if (accounts.length === 1) url.searchParams.set('owner', accounts[0].account_login)
  url.searchParams.set('visibility', 'private')
  return url.toString()
}

// The connected account a value concerns: the owner of the repository named, the account named for
// a new one, or the account a new one goes to unasked.
function accountOf(value: string, accounts: GitHubInstallation[], unasked?: GitHubInstallation): GitHubInstallation | undefined {
  const owner = value.startsWith(ON_ACCOUNT) ? value.slice(ON_ACCOUNT.length) : value.split('/')[0]
  return owner ? accounts.find(a => a.account_login.toLowerCase() === owner.toLowerCase()) : unasked
}

/**
 * Runs the create's own repository checks for the value picked, so a GitHub account the App left
 * or a repository it cannot push to shows here, as one chip and the one action that fixes it,
 * before anything is created.
 */
function RepositoryCheckNote({ value, account, onInstall }: { value: string; account?: GitHubInstallation; onInstall?: () => void }) {
  const { project } = useTenant()
  const { error, isValidating, mutate } = useSWR(['repository-check', project.id, value], () => checkRepository(project.id, repositoryFields(value)), { shouldRetryOnError: false })
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
 * With none chosen the platform creates one on a connected account. Unless `compact`, the way on
 * is offered: the person creates the repository on GitHub and refreshes this list to choose it,
 * or an owner lets the App reach more in a new tab, after which the list is read again. With no
 * account connected an owner installs the App from here. On a deployment whose git host is not GitHub
 * the repository is typed: the platform creates none there. A new repository goes to the only
 * connected account or the org's default, named in the option, and with several the person may
 * pick another. Unless `compact`, what is picked is checked as the create would check it.
 */
export function RepositorySelect({ value, onChange, compact = false }: {
  value: string
  onChange: (repository: string) => void
  compact?: boolean
}) {
  const { org, administers } = useTenant()
  const [q, setQ] = useState('')
  const app = useGitHubApp()
  const { install, start, reset } = useGitHubInstall(org.id, typeof window === 'undefined' ? '/' : window.location.pathname + window.location.search)
  const onInstall = administers && app.data ? start : undefined
  const installations = useSWR(app.data ? ['github-installations', org.id] : null, () => fetchGitHubInstallations(org.id))
  const connected = (installations.data?.total ?? 0) > 0
  const [refreshing, setRefreshing] = useState(false)
  const { data, error, mutate } = useSWR(
    connected ? ['github-repositories', org.id, q] : null,
    () => fetchGitHubRepositories(org.id, { q, limit: LISTED }),
    { keepPreviousData: true, shouldRetryOnError: false },
  )

  if (app.error instanceof ApiError && app.error.reason === GITHUB_NOT_CONFIGURED) {
    return (
      <>
        <Input className="w-full" aria-label="Repository" value={value} onChange={e => onChange(e.target.value)} />
        <Text variant="hint">A repository on this deployment&apos;s git host, created there first.</Text>
      </>
    )
  }
  if (app.error) return <InlineError>Could not read the deployment&apos;s git host: {app.error.message}</InlineError>
  if (installations.error) return <InlineError>Could not load the GitHub accounts of {org.name}: {installations.error.message}</InlineError>
  if (installations.data && !connected) {
    return (
      <>
        <Text variant="note">
          No GitHub account is connected to {org.name}.{' '}
          {onInstall
            ? <Button type="button" variant="link" size="sm" disabled={install.phase === 'waiting'} onClick={onInstall}>Install GitHub App</Button>
            : 'An owner of the org installs the GitHub App.'}
        </Text>
        <InstallProgress install={install} onCancel={reset} />
      </>
    )
  }

  const newLabel = compact ? 'New private repo' : 'A new private repository'
  const listed = data?.items ?? []
  const more = (data?.total ?? 0) - listed.length
  // What is chosen stays an option while a search leaves it out.
  const options = listed.map(r => ({ value: r.repository, label: r.repository, badge: r.private ? 'private' : undefined }))
  if (value && !value.startsWith(ON_ACCOUNT) && !listed.some(r => r.repository === value)) options.unshift({ value, label: value, badge: undefined })
  // A new repository goes to the only account or the org's default unasked, and with several
  // connected the person may name another. One connected before its type was kept takes no new
  // repository until it is connected again.
  const accounts = installations.data?.items ?? []
  const creating = accounts.filter(a => a.account_type != null)
  const unasked = accounts.length === 1 ? creating[0] : creating.find(a => a.default)
  const createsUnasked = unasked != null
  if (accounts.length > 1) {
    options.unshift(...creating.filter(a => a !== unasked).map(a => ({ value: ON_ACCOUNT + a.account_login, label: `${newLabel} on ${a.account_login}`, badge: undefined })))
  }
  const unaskedLabel = unasked ? `${newLabel} on ${unasked.account_login}` : newLabel
  async function refresh() {
    setRefreshing(true)
    try {
      await mutate()
    } finally {
      setRefreshing(false)
    }
  }
  return (
    <>
      <SearchableSelect
        fieldSize="default"
        className={compact ? 'w-56' : 'w-full'}
        value={value}
        onChange={onChange}
        onSearch={setQ}
        allLabel={unaskedLabel}
        placeholder={createsUnasked ? unaskedLabel : compact ? 'Choose a repo' : 'Choose a repository'}
        hideAllOption={!createsUnasked}
        options={options}
        hint={more > 0 ? `${more} more, type to search` : undefined}
      />
      {app.data && !compact && (
        <Text variant="hint">
          Not listed?{' '}
          <a href={newRepositoryURL(app.data.new_repository_url, accounts)} target="_blank" rel="noreferrer" className={inlineLinkClass}>create it on GitHub</a>
          {onInstall && (
            <>
              {' '}or{' '}
              <Button type="button" variant="link" size="sm" disabled={install.phase === 'waiting'} onClick={onInstall}>give the App access</Button>
            </>
          )}, then{' '}
          <Button type="button" variant="link" size="sm" loading={refreshing} onClick={() => void refresh()}>refresh this list</Button>.
        </Text>
      )}
      {!compact && <InstallProgress install={install} onCancel={reset} />}
      {error && <InlineError>Could not list the repositories: {error.message}</InlineError>}
      {!compact && (value !== '' || createsUnasked) && <RepositoryCheckNote value={value} account={accountOf(value, accounts, unasked)} onInstall={onInstall} />}
    </>
  )
}
