import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, fetchGitHubApp, fetchGitHubInstallations, fetchGitHubRepositories } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { RepositorySelect, repositoryFields } from '@/components/repository-select'
import type { GitHubInstallation, GitHubRepository, Me } from '@/types'

vi.mock('next/navigation', () => ({ useSearchParams: () => new URLSearchParams('project=prj_1') }))
vi.mock('@/lib/api', async importActual => ({
  ...(await importActual<typeof import('@/lib/api')>()),
  fetchGitHubApp: vi.fn(), fetchGitHubInstallations: vi.fn(), fetchGitHubRepositories: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'member', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const repositories: GitHubRepository[] = [
  { repository: 'acme/widgets', installation_id: 7, private: true },
  { repository: 'acme/open', installation_id: 7, private: false },
]

const organisation: GitHubInstallation = { org_id: 'org_a', installation_id: 7, account_login: 'acme', account_type: 'Organization', connected_actor: { kind: 'user', id: 'u9', name: 'Una' }, connected_at: at }

function Picker({ initial = '' }: { initial?: string }) {
  const [value, setValue] = useState(initial)
  return (
    <>
      <RepositorySelect value={value} onChange={setValue} />
      <output>{value}</output>
    </>
  )
}

function show(initial?: string) {
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><Picker initial={initial} /></TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  vi.mocked(fetchGitHubApp).mockResolvedValue({ slug: 'hl', client_id: 'c', install_url: 'https://github.test/install', authorize_url: 'https://github.test/authorize' })
  vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [{ org_id: 'org_a', installation_id: 7, account_login: 'acme', account_type: 'Organization', connected_actor: { kind: 'user', id: 'u9', name: 'Una' }, connected_at: at }], total: 1 })
  vi.mocked(fetchGitHubRepositories).mockImplementation(async (_, params) => {
    const found = repositories.filter(r => r.repository.includes(params?.q ?? ''))
    return { items: found, total: found.length }
  })
})

describe('the repository of a new platform experiment', () => {
  it('is picked from what the org\'s GitHub accounts cover, a private one marked', async () => {
    show()
    await userEvent.click(await screen.findByRole('combobox'))
    const option = await screen.findByRole('option', { name: /acme\/widgets/ })
    expect(option.textContent).toBe('acme/widgetsprivate')
    expect(screen.getByRole('option', { name: 'acme/open' }).textContent).toBe('acme/open')
    await userEvent.click(option)
    expect(screen.getByRole('status').textContent).toBe('acme/widgets')
  })

  it('may be left out', async () => {
    show('acme/open')
    await userEvent.click(await screen.findByRole('combobox'))
    await userEvent.click(await screen.findByRole('option', { name: 'A new private repository' }))
    expect(screen.getByRole('status').textContent).toBe('')
    expect([repositoryFields(''), repositoryFields('acme/open'), repositoryFields('@acme')]).toEqual([{}, { repository: 'acme/open' }, { repository_account: 'acme' }])
  })

  it('asks which account a new one goes to when several are connected', async () => {
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [organisation, { ...organisation, installation_id: 8, account_login: 'second' }], total: 2 })
    show()
    await userEvent.click(await screen.findByRole('combobox'))
    await userEvent.click(await screen.findByRole('option', { name: 'A new private repository on second' }))
    expect(screen.getByRole('status').textContent).toBe('@second')
    expect(screen.queryByRole('option', { name: 'A new private repository' })).toBeNull()
    expect(screen.queryByText(/personal account/)).toBeNull()
  })

  it('offers no new one on a personal account, and says why and what to do', async () => {
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [{ ...organisation, account_login: 'ada', account_type: 'User' }], total: 1 })
    show()
    await userEvent.click(await screen.findByRole('combobox'))
    await screen.findByRole('option', { name: 'acme/open' })
    expect(screen.queryByRole('option', { name: /A new private repository/ })).toBeNull()
    expect(screen.getByRole('combobox').getAttribute('placeholder')).toBe('Choose a repository')
    expect(screen.getByText(/GitHub lets the platform create no repository on a personal account \(ada\): create an empty private one there and choose it, or/)).toBeTruthy()
    expect(screen.getByRole('link', { name: 'connect an organisation', hidden: true }).getAttribute('href')).toBe('/settings?project=prj_1#github')
  })

  it('offers a new one on the organisation alone when a personal account is connected beside it', async () => {
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [organisation, { ...organisation, installation_id: 8, account_login: 'ada', account_type: 'User' }], total: 2 })
    show()
    await userEvent.click(await screen.findByRole('combobox'))
    await userEvent.click(await screen.findByRole('option', { name: 'A new private repository on acme' }))
    expect(screen.getByRole('status').textContent).toBe('@acme')
    expect(screen.queryByRole('option', { name: 'A new private repository on ada' })).toBeNull()
    expect(screen.queryByRole('option', { name: 'A new private repository' })).toBeNull()
    expect(screen.getByText(/personal account \(ada\)/)).toBeTruthy()
  })

  it('offers no new one on an account whose type GitHub has not said', async () => {
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [{ ...organisation, account_type: undefined }], total: 1 })
    show()
    await userEvent.click(await screen.findByRole('combobox'))
    await screen.findByRole('option', { name: 'acme/open' })
    expect(screen.queryByRole('option', { name: /A new private repository/ })).toBeNull()
    expect(screen.queryByText(/personal account/)).toBeNull()
  })

  it('is searched on the server once typing pauses', async () => {
    show()
    const field = await screen.findByRole('combobox')
    await waitFor(() => expect(fetchGitHubRepositories).toHaveBeenCalledWith('org_a', { q: '', limit: 50 }))
    await userEvent.type(field, 'wid')
    await waitFor(() => expect(fetchGitHubRepositories).toHaveBeenLastCalledWith('org_a', { q: 'wid', limit: 50 }))
    expect(vi.mocked(fetchGitHubRepositories).mock.calls.map(c => c[1]?.q)).not.toContain('w')
    await waitFor(() => expect(screen.getAllByRole('option').map(o => o.textContent)).toEqual(['A new private repository', 'acme/widgetsprivate']))
  })

  it('says how many more there are than it lists', async () => {
    vi.mocked(fetchGitHubRepositories).mockResolvedValue({ items: repositories, total: 120 })
    show()
    await userEvent.click(await screen.findByRole('combobox'))
    expect(await screen.findByText('118 more, type to search')).toBeTruthy()
  })

  it('leads to the GitHub card of Settings when no account is connected, and asks GitHub nothing', async () => {
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [], total: 0 })
    show()
    expect(await screen.findByText(/No GitHub account is connected to Lab, and an experiment keeps its code in a repository\./)).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Connect GitHub in Settings' }).getAttribute('href')).toBe('/settings?project=prj_1#github')
    expect(screen.queryByRole('combobox')).toBeNull()
    expect(fetchGitHubRepositories).not.toHaveBeenCalled()
  })

  it('is typed on a deployment whose git host is not GitHub', async () => {
    vi.mocked(fetchGitHubApp).mockRejectedValue(new ApiError(503, 'no GitHub App', 'github_not_configured'))
    show()
    await userEvent.type(await screen.findByRole('textbox', { name: 'Repository' }), 'sweep.git')
    expect(screen.getByRole('status').textContent).toBe('sweep.git')
    expect(screen.getByText(/created there beforehand/)).toBeTruthy()
    expect(fetchGitHubInstallations).not.toHaveBeenCalled()
  })

  it('says why the repositories could not be listed', async () => {
    vi.mocked(fetchGitHubRepositories).mockRejectedValue(new Error('GitHub is unreachable'))
    show()
    expect((await screen.findByRole('alert')).textContent).toBe('Could not list the repositories: GitHub is unreachable')
  })
})
