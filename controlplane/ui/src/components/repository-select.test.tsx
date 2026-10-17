import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { beginGitHubInstall, checkGitHubInstallation, fetchGitHubApp, fetchGitHubInstallations, fetchGitHubRepositories } from '@/lib/api/github'
import { checkRepository } from '@/lib/api/platform-experiments'
import { ApiError } from '@/lib/api/request'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { takeReturn } from '@/lib/github-connect'
import { RepositorySelect, repositoryFields } from '@/components/repository-select'
import type { Me, OrgGitHubInstallation, Repository } from '@/types/api'

vi.mock('@/lib/api/github', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/github')>()),
  beginGitHubInstall: vi.fn(), checkGitHubInstallation: vi.fn(), fetchGitHubApp: vi.fn(), fetchGitHubInstallations: vi.fn(), fetchGitHubRepositories: vi.fn(),
}))
vi.mock('@/lib/api/platform-experiments', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/platform-experiments')>()),
  checkRepository: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'member', created_at: at, actions: ['create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const owner: Me = { ...me, orgs: [{ ...me.orgs[0], role: 'owner', actions: ['edit', 'create_api_key'] }] }
const repositories: Repository[] = [
  { repository: 'acme/widgets', installation_id: 7, private: true },
  { repository: 'acme/open', installation_id: 7, private: false },
]

const organisation: OrgGitHubInstallation = { org_id: 'org_a', installation_id: 7, account_login: 'acme', account_type: 'Organization', connected_actor: { kind: 'user', id: 'u9', name: 'Una' }, connected_at: at, default: false }

function Picker({ initial = '' }: { initial?: string }) {
  const [value, setValue] = useState(initial)
  return (
    <>
      <RepositorySelect value={value} onChange={setValue} />
      <output>{value}</output>
    </>
  )
}

function show(initial?: string, as: Me = me) {
  const resolved = resolveTenant(as, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><Picker initial={initial} /></TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  vi.mocked(checkRepository).mockResolvedValue({ repository_account: 'acme' })
  vi.mocked(checkGitHubInstallation).mockResolvedValue({ working: true })
  vi.mocked(fetchGitHubApp).mockResolvedValue({ slug: 'hl', install_url: 'https://github.test/install', new_repository_url: 'https://github.test/new' })
  vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [{ org_id: 'org_a', installation_id: 7, account_login: 'acme', account_type: 'Organization', connected_actor: { kind: 'user', id: 'u9', name: 'Una' }, connected_at: at, default: false }], total: 1 })
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
    await userEvent.click(await screen.findByRole('option', { name: 'A new private repository on acme' }))
    expect(screen.getByRole('status').textContent).toBe('')
    expect([repositoryFields(''), repositoryFields('acme/open'), repositoryFields('@acme')]).toEqual([{}, { repository: 'acme/open' }, { repository_account: 'acme' }])
  })

  it('asks which account a new one goes to when several are connected', async () => {
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [organisation, { ...organisation, installation_id: 8, account_login: 'second' }], total: 2 })
    show()
    await userEvent.click(await screen.findByRole('combobox'))
    await userEvent.click(await screen.findByRole('option', { name: 'A new private repository on second' }))
    expect(screen.getByRole('status').textContent).toBe('@second')
    expect(screen.queryByRole('option', { name: 'A new private repository on acme' })).toBeNull()
  })

  it('offers a new one on a personal account as on an organisation', async () => {
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [{ ...organisation, account_login: 'ada', account_type: 'User' }], total: 1 })
    show('acme/open')
    await userEvent.click(await screen.findByRole('combobox'))
    await userEvent.click(await screen.findByRole('option', { name: 'A new private repository on ada' }))
    expect(screen.getByRole('status').textContent).toBe('')
  })

  it('offers a new one on either when a personal account is connected beside an organisation', async () => {
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [organisation, { ...organisation, installation_id: 8, account_login: 'ada', account_type: 'User' }], total: 2 })
    show()
    await userEvent.click(await screen.findByRole('combobox'))
    expect(await screen.findByRole('option', { name: 'A new private repository on acme' })).toBeTruthy()
    await userEvent.click(screen.getByRole('option', { name: 'A new private repository on ada' }))
    expect(screen.getByRole('status').textContent).toBe('@ada')
    expect(screen.queryByRole('option', { name: 'A new private repository on acme' })).toBeNull()
  })

  it('offers no new one on an account connected before its type was kept', async () => {
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [{ ...organisation, account_type: undefined }], total: 1 })
    show()
    await userEvent.click(await screen.findByRole('combobox'))
    await screen.findByRole('option', { name: 'acme/open' })
    expect(screen.queryByRole('option', { name: /A new private repository/ })).toBeNull()
    expect(screen.getByRole('combobox').getAttribute('placeholder')).toBe('Choose a repository')
  })

  it('leads to GitHub for a repository of the person\'s own, and lists it once they refresh', async () => {
    show()
    const create = await screen.findByRole('link', { name: 'create it on GitHub' })
    expect(create.getAttribute('href')).toBe('https://github.test/new?owner=acme&visibility=private')
    expect(screen.queryByRole('button', { name: 'give the App access' })).toBeNull()
    await userEvent.click(screen.getByRole('combobox'))
    await screen.findByRole('option', { name: 'acme/open' })
    expect(screen.queryByRole('option', { name: /acme\/mine/ })).toBeNull()
    await userEvent.keyboard('{Escape}')
    repositories.push({ repository: 'acme/mine', installation_id: 7, private: true })
    try {
      await userEvent.click(screen.getByRole('button', { name: 'refresh this list' }))
      await userEvent.click(screen.getByRole('combobox'))
      await userEvent.click(await screen.findByRole('option', { name: /acme\/mine/ }))
      expect(screen.getByRole('status').textContent).toBe('acme/mine')
    } finally {
      repositories.pop()
    }
  })

  it('takes the org\'s default for a new one among several accounts, and offers the others', async () => {
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [organisation, { ...organisation, installation_id: 8, account_login: 'ada', account_type: 'User', default: true }], total: 2 })
    show('acme/open')
    await userEvent.click(await screen.findByRole('combobox'))
    expect(await screen.findByRole('option', { name: 'A new private repository on acme' })).toBeTruthy()
    expect(screen.getAllByRole('option', { name: /on ada/ })).toHaveLength(1)
    await userEvent.click(screen.getByRole('option', { name: 'A new private repository on ada' }))
    expect(screen.getByRole('status').textContent).toBe('')
  })

  it('offers no account GitHub removed the App from, and takes the one working account unasked', async () => {
    const removed = { ...organisation, installation_id: 8, account_login: 'antonibertel', account_type: 'User' as const }
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [organisation, removed], total: 2 })
    vi.mocked(checkGitHubInstallation).mockImplementation(async (_, id) => id === 8 ? { working: false, reason: 'installation_gone', message: 'gone' } : { working: true })
    show()
    await waitFor(() => expect(screen.getByRole('combobox').getAttribute('placeholder')).toBe('A new private repository on acme'))
    await userEvent.click(screen.getByRole('combobox'))
    await screen.findByRole('option', { name: 'acme/open' })
    expect(screen.queryByRole('option', { name: /antonibertel/ })).toBeNull()
  })

  it('shows what the create would refuse as a chip and its fix, before anything is created', async () => {
    const gone = 'the GitHub App is no longer installed on acme (installation 7): its owner installs the App again from Settings, GitHub'
    vi.mocked(checkRepository).mockRejectedValue(new ApiError(422, gone, 'installation_gone'))
    show()
    const chip = (await screen.findByText('Not installed')).closest('[title]')!
    expect(chip.getAttribute('title')).toBe('The GitHub App was removed on acme, so experiments cannot use its repositories. Reinstall it to connect again.')
    expect(document.body.textContent).not.toContain(gone)
    expect(checkRepository).toHaveBeenCalledWith('prj_1', {})
    vi.mocked(checkRepository).mockResolvedValue({ repository_account: 'acme' })
  vi.mocked(checkGitHubInstallation).mockResolvedValue({ working: true })
    await userEvent.click(screen.getByRole('button', { name: 'Check again' }))
    await waitFor(() => expect(screen.queryByText('Not installed')).toBeNull())
  })

  it('offers an owner to reinstall where the App was removed', async () => {
    vi.mocked(checkRepository).mockRejectedValue(new ApiError(422, 'gone', 'installation_gone'))
    show(undefined, owner)
    expect(await screen.findByRole('button', { name: 'Reinstall' })).toBeTruthy()
  })

  it('checks a repository picked as the create would, and offers to add it to the App', async () => {
    vi.mocked(checkRepository).mockRejectedValue(new ApiError(422, 'the GitHub installation the org connected does not cover that repository', 'repository_not_reachable'))
    show('acme/open', owner)
    expect((await screen.findByText('No access')).closest('[title]')!.getAttribute('title')).toBe('The GitHub App cannot reach this repository. Add it to the App\'s repositories on acme.')
    expect(checkRepository).toHaveBeenCalledWith('prj_1', { repository: 'acme/open' })
    expect(screen.getByRole('button', { name: 'Add repositories' })).toBeTruthy()
  })

  it('leads to the installation\'s page on GitHub for a permission the App was not granted', async () => {
    vi.mocked(checkRepository).mockRejectedValue(new ApiError(422, 'git_host_permission_missing: administration:write. The GitHub App installation has not granted that', 'git_host_permission_missing'))
    show()
    expect((await screen.findByText('Missing permission')).closest('[title]')!.getAttribute('title')).toContain('administration (write)')
    expect(screen.getByRole('link', { name: 'Review on GitHub' }).getAttribute('href')).toBe('https://github.com/organizations/acme/settings/installations/7')
  })

  it('names no owner for a new repository on GitHub when several accounts are connected', async () => {
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [organisation, { ...organisation, installation_id: 8, account_login: 'ada', account_type: 'User' }], total: 2 })
    show()
    expect((await screen.findByRole('link', { name: 'create it on GitHub' })).getAttribute('href')).toBe('https://github.test/new?visibility=private')
  })

  it('is searched on the server once typing pauses', async () => {
    show()
    const field = await screen.findByRole('combobox')
    await waitFor(() => expect(fetchGitHubRepositories).toHaveBeenCalledWith('org_a', { q: '', limit: 50 }))
    await userEvent.type(field, 'wid')
    await waitFor(() => expect(fetchGitHubRepositories).toHaveBeenLastCalledWith('org_a', { q: 'wid', limit: 50 }))
    expect(vi.mocked(fetchGitHubRepositories).mock.calls.map(c => c[1]?.q)).not.toContain('w')
    await waitFor(() => expect(screen.getAllByRole('option').map(o => o.textContent)).toEqual(['A new private repository on acme', 'acme/widgetsprivate']))
  })

  it('says an owner installs the App when no account is connected, and asks GitHub nothing', async () => {
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [], total: 0 })
    show()
    expect(await screen.findByText(/An owner of the org installs the GitHub App/)).toBeTruthy()
    expect(screen.queryByRole('combobox')).toBeNull()
    expect(fetchGitHubRepositories).not.toHaveBeenCalled()
  })

  it('is typed on a deployment whose git host is not GitHub', async () => {
    vi.mocked(fetchGitHubApp).mockRejectedValue(new ApiError(503, 'no GitHub App', 'github_not_configured'))
    show()
    await userEvent.type(await screen.findByRole('textbox', { name: 'Repository' }), 'sweep.git')
    expect(screen.getByRole('status').textContent).toBe('sweep.git')
    expect(fetchGitHubInstallations).not.toHaveBeenCalled()
  })

  it('says why the repositories could not be listed', async () => {
    vi.mocked(fetchGitHubRepositories).mockRejectedValue(new Error('GitHub is unreachable'))
    show()
    expect((await screen.findByRole('alert')).textContent).toBe('Could not list the repositories: GitHub is unreachable')
  })
})

describe('installing the App from the form', () => {
  let opened: { location: { href: string }; close: () => void }
  beforeEach(() => {
    opened = { location: { href: '' }, close: vi.fn() }
    vi.spyOn(window, 'open').mockReturnValue(opened as unknown as Window)
    vi.mocked(beginGitHubInstall).mockResolvedValue({ state: 's-1', install_url: 'https://github.test/install?state=s-1', expires_at: at })
  })

  it('opens GitHub in a new tab, leaves the form as it is, and lists what the App reaches once the other tab connects', async () => {
    show('acme/open', owner)
    await userEvent.click(await screen.findByRole('button', { name: 'give the App access' }))
    expect(beginGitHubInstall).toHaveBeenCalledExactlyOnceWith('org_a', '/')
    expect(opened.location.href).toBe('https://github.test/install?state=s-1')
    expect((await screen.findByText(/Waiting for GitHub/))).toBeTruthy()
    expect(screen.getAllByRole('status').some(s => s.textContent === 'acme/open')).toBe(true)
    const listed = vi.mocked(fetchGitHubRepositories).mock.calls.length
    await takeReturn('installation_id=7&setup_action=update&state=s-1', async () => ({ org_id: 'org_a', return_to: '/', installation: organisation }))
    expect(await screen.findByText('acme is connected.')).toBeTruthy()
    await waitFor(() => expect(vi.mocked(fetchGitHubRepositories).mock.calls.length).toBeGreaterThan(listed))
  })

  it('installs the App from the form when no account is connected', async () => {
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [], total: 0 })
    show(undefined, owner)
    await userEvent.click(await screen.findByRole('button', { name: 'Install GitHub App' }))
    expect(beginGitHubInstall).toHaveBeenCalledExactlyOnceWith('org_a', '/')
    expect(await screen.findByText(/Waiting for GitHub/)).toBeTruthy()
  })
})
