'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { ApiError, GITHUB_NOT_CONFIGURED, fetchGitHubInstallations, fetchGitHubRepositories } from '@/lib/api'
import { GITHUB_CARD } from '@/lib/github-connect'
import { useTenant } from '@/lib/tenant'
import { TextLink } from '@/components/ui/link'
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

/**
 * Picks the repository a platform experiment works on, from those the org's connected GitHub
 * accounts cover. Typing searches all of them on the server, which reads GitHub on each call.
 * With none chosen the platform creates one. With no account connected it says where one is
 * connected.
 */
export function RepositorySelect({ value, onChange }: { value: string; onChange: (repository: string) => void }) {
  const { org } = useTenant()
  const [q, setQ] = useState('')
  const installations = useSWR(['github-installations', org.id], () => fetchGitHubInstallations(org.id))
  const connected = (installations.data?.total ?? 0) > 0
  const { data, error } = useSWR(
    connected ? ['github-repositories', org.id, q] : null,
    () => fetchGitHubRepositories(org.id, { q, limit: LISTED }),
    { keepPreviousData: true, shouldRetryOnError: false },
  )

  if (installations.error) return <InlineError>Could not load the GitHub accounts of {org.name}: {installations.error.message}</InlineError>
  if (installations.data && !connected) {
    return (
      <p className="text-sm text-muted">
        No GitHub account is connected to {org.name}, and an experiment keeps its code in a repository.{' '}
        <TextLink inline href={`/settings#${GITHUB_CARD}`}>Connect GitHub in Settings</TextLink>
      </p>
    )
  }
  if (error instanceof ApiError && error.reason === GITHUB_NOT_CONFIGURED) {
    return <p className="text-sm text-muted">This deployment has no GitHub App configured, so the experiment has no repository.</p>
  }

  const listed = data?.items ?? []
  const more = (data?.total ?? 0) - listed.length
  // What is chosen stays an option while a search leaves it out.
  const options = listed.map(r => ({ value: r.repository, label: r.repository, badge: r.private ? 'private' : undefined }))
  if (value && !value.startsWith(ON_ACCOUNT) && !listed.some(r => r.repository === value)) options.unshift({ value, label: value, badge: undefined })
  // With several accounts connected, which one a new repository goes to is the person's to say.
  const accounts = installations.data?.items ?? []
  if (accounts.length > 1) {
    options.unshift(...accounts.map(a => ({ value: ON_ACCOUNT + a.account_login, label: `A new private repository on ${a.account_login}`, badge: undefined })))
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
        placeholder="A new private repository"
        options={options}
        hint={more > 0 ? `${more} more, type to search` : undefined}
      />
      {error && <InlineError>Could not list the repositories: {error.message}</InlineError>}
    </>
  )
}
