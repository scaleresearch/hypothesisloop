import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, fetchGitHubInstallations, fetchGitHubRepositories } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { RepositorySelect, repositoryFields } from '@/components/repository-select'
import type { GitHubRepository, Me } from '@/types'

vi.mock('next/navigation', () => ({ useSearchParams: () => new URLSearchParams('project=prj_1') }))
vi.mock('@/lib/api', async importActual => ({
  ...(await importActual<typeof import('@/lib/api')>()),
  fetchGitHubInstallations: vi.fn(), fetchGitHubRepositories: vi.fn(),
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
  vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [{ org_id: 'org_a', installation_id: 7, account_login: 'acme', connected_actor: { kind: 'user', id: 'u9', name: 'Una' }, connected_at: at }], total: 1 })
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

  it('says so on a deployment without a GitHub App', async () => {
    vi.mocked(fetchGitHubRepositories).mockRejectedValue(new ApiError(503, 'no GitHub App', 'github_not_configured'))
    show()
    expect(await screen.findByText('This deployment has no GitHub App configured, so the experiment has no repository.')).toBeTruthy()
  })

  it('says why the repositories could not be listed', async () => {
    vi.mocked(fetchGitHubRepositories).mockRejectedValue(new Error('GitHub is unreachable'))
    show()
    expect((await screen.findByRole('alert')).textContent).toBe('Could not list the repositories: GitHub is unreachable')
  })
})
