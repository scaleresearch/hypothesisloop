'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { ApiError, GITHUB_NOT_CONFIGURED, fetchGitHubInstallations, fetchGitHubRepositories } from '@/lib/api'
import { GITHUB_CARD, useGitHubApp } from '@/lib/github-connect'
import { useTenant } from '@/lib/tenant'
import type { GitHubInstallation } from '@/types'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { TextLink, inlineLinkClass } from '@/components/ui/link'
import { SearchableSelect } from '@/components/ui/searchable-select'
import { InlineError } from '@/components/ui/status-message'

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

/**
 * Picks the repository a platform experiment works on, from those the org's connected GitHub
 * accounts cover. Typing searches all of them on the server, which reads GitHub on each call.
 * With none chosen the platform creates one on a connected account. `notCreated` is what the API
 * said when GitHub would not let it: it is shown with the way on, which is always offered. The
 * person creates the repository on GitHub, lets the App reach it, and refreshes this list to
 * choose it. With no account connected it says where one is connected. On a deployment whose
 * git host is not GitHub the repository is typed: the platform creates none there.
 */
export function RepositorySelect({ value, onChange, notCreated }: { value: string; onChange: (repository: string) => void; notCreated?: string }) {
  const { org } = useTenant()
  const [q, setQ] = useState('')
  const app = useGitHubApp()
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
        <Input mono className="w-full" aria-label="Repository" value={value} onChange={e => onChange(e.target.value)} />
        <p className="text-sm text-muted">The name of a repository on this deployment&apos;s git host, created there beforehand.</p>
      </>
    )
  }
  if (app.error) return <InlineError>Could not read the deployment&apos;s git host: {app.error.message}</InlineError>
  if (installations.error) return <InlineError>Could not load the GitHub accounts of {org.name}: {installations.error.message}</InlineError>
  if (installations.data && !connected) {
    return (
      <p className="text-sm text-muted">
        No GitHub account is connected to {org.name}, and an experiment keeps its code in a repository.{' '}
        <TextLink inline href={`/settings#${GITHUB_CARD}`}>Connect GitHub in Settings</TextLink>
      </p>
    )
  }

  const listed = data?.items ?? []
  const more = (data?.total ?? 0) - listed.length
  // What is chosen stays an option while a search leaves it out.
  const options = listed.map(r => ({ value: r.repository, label: r.repository, badge: r.private ? 'private' : undefined }))
  if (value && !value.startsWith(ON_ACCOUNT) && !listed.some(r => r.repository === value)) options.unshift({ value, label: value, badge: undefined })
  // With several accounts connected, which one a new repository goes to is the person's to say.
  // One whose type GitHub has not said was removed there, and takes no new repository.
  const accounts = installations.data?.items ?? []
  const creating = accounts.filter(a => a.account_type != null)
  const createsUnasked = accounts.length === 1 && creating.length === 1
  if (accounts.length > 1) {
    options.unshift(...creating.map(a => ({ value: ON_ACCOUNT + a.account_login, label: `A new private repository on ${a.account_login}`, badge: undefined })))
  }
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
        mono
        fieldSize="default"
        className="w-full"
        value={value}
        onChange={onChange}
        onSearch={setQ}
        allLabel="A new private repository"
        placeholder={createsUnasked ? 'A new private repository' : 'Choose a repository'}
        hideAllOption={!createsUnasked}
        options={options}
        hint={more > 0 ? `${more} more, type to search` : undefined}
      />
      {notCreated && <InlineError>The platform could not create the repository: {notCreated}</InlineError>}
      {app.data && (
        <p className="text-sm text-muted">
          To use a repository of your own,{' '}
          <a href={newRepositoryURL(app.data.new_repository_url, accounts)} target="_blank" rel="noreferrer" className={inlineLinkClass}>create it on GitHub</a>,{' '}
          <a href={app.data.install_url} target="_blank" rel="noreferrer" className={inlineLinkClass}>add it to the App&apos;s repositories</a>{' '}
          when the App covers selected ones only, then{' '}
          <Button type="button" variant="link" size="sm" loading={refreshing} onClick={() => void refresh()}>refresh this list</Button>{' '}
          and choose it.
        </p>
      )}
      {error && <InlineError>Could not list the repositories: {error.message}</InlineError>}
    </>
  )
}
