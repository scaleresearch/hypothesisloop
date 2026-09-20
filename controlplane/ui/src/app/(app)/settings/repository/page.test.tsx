import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, attachProjectRepo, detachProjectRepo, fetchGitHubApp, fetchProjectRepo } from '@/lib/api'
import { installAddress, leaveFor } from '@/lib/repo-attach'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import RepositoryPage from '@/app/(app)/settings/repository/page'
import type { Me, ProjectRepo } from '@/types'

const navigation = vi.hoisted(() => ({ replace: vi.fn(), push: vi.fn(), search: '' }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  usePathname: () => '/settings/repository',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', async importActual => ({
  ApiError: (await importActual<typeof import('@/lib/api')>()).ApiError,
  attachProjectRepo: vi.fn(), detachProjectRepo: vi.fn(), fetchGitHubApp: vi.fn(), fetchProjectRepo: vi.fn(),
}))
vi.mock('@/lib/repo-attach', async importActual => ({
  ...(await importActual<typeof import('@/lib/repo-attach')>()),
  leaveFor: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const owner: Me = {
  kind: 'user',
  user_id: 'u1',
  name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [
    { id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at },
    { id: 'prj_2', org_id: 'org_a', name: 'vision', created_at: at },
  ],
}
const member: Me = { ...owner, orgs: [{ ...owner.orgs[0], role: 'member' }] }
const app = {
  slug: 'hl', client_id: 'Iv1.abc',
  install_url: 'https://github.test/apps/hl/installations/new',
  authorize_url: 'https://github.test/login/oauth/authorize?client_id=Iv1.abc',
}
const attached: ProjectRepo = {
  project_id: 'prj_2', installation_id: 4242, repository_id: 9, repo: 'acme/widgets', default_branch: 'main',
  attached_at: at, clone_url: 'https://github.com/acme/widgets.git',
}

let repo: ProjectRepo | null

// Renders the page with `search` as the query and the project it names selected.
function show(search = 'project=prj_2', as: Me = owner) {
  navigation.search = search
  const resolved = resolveTenant(as, new URLSearchParams(search).get('project') ?? '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map(), shouldRetryOnError: false }}>
      <TenantContext.Provider value={resolved.tenant}><RepositoryPage /></TenantContext.Provider>
    </SWRConfig>,
  )
}

// Starts an attempt in this tab, as the Install button does, and returns its state.
function begun(projectID = 'prj_2') {
  return new URL(installAddress(app.install_url, projectID)).searchParams.get('state')!
}

beforeEach(() => {
  window.sessionStorage.clear()
  repo = null
  vi.mocked(fetchProjectRepo).mockImplementation(async () => repo)
  vi.mocked(fetchGitHubApp).mockResolvedValue(app)
  vi.mocked(attachProjectRepo).mockImplementation(async () => (repo = attached))
  vi.mocked(detachProjectRepo).mockImplementation(async () => { repo = null })
  vi.stubGlobal('confirm', vi.fn(() => true))
})

describe('a project with no repository', () => {
  it('offers one action, installing the App, which leaves with a state this tab keeps', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Install the GitHub App' }))
    expect(screen.getAllByRole('button')).toHaveLength(1)
    const address = new URL(vi.mocked(leaveFor).mock.calls[0][0])
    expect(address.origin + address.pathname).toBe(app.install_url)
    const kept = JSON.parse(window.sessionStorage.getItem('hl-repository-attach')!)
    expect(kept).toEqual({ nonce: address.searchParams.get('state'), projectID: 'prj_2' })
    expect(fetchProjectRepo).toHaveBeenCalledWith('prj_2')
  })

  it('says plainly when the deployment has no GitHub App', async () => {
    vi.mocked(fetchGitHubApp).mockRejectedValue(new ApiError(503, 'this deployment has no GitHub App configured'))
    show()
    expect(await screen.findByText(/GitHub is not configured on this deployment/)).toBeTruthy()
    expect(screen.queryByRole('button')).toBeNull()
  })

  it('shows another failure of the App read as a failure', async () => {
    vi.mocked(fetchGitHubApp).mockRejectedValue(new Error('Failed to fetch'))
    show()
    expect(await screen.findByText(/Could not load the platform's GitHub App: Failed to fetch/)).toBeTruthy()
    expect(screen.queryByRole('button')).toBeNull()
  })

  it('shows a failed read of the repository as a failure, never as none attached', async () => {
    vi.mocked(fetchProjectRepo).mockRejectedValue(new Error('Failed to fetch'))
    show()
    expect(await screen.findByText('Could not load the repository of vision: Failed to fetch')).toBeTruthy()
    expect(screen.queryByText(/No repository is attached/)).toBeNull()
    expect(screen.queryByRole('button')).toBeNull()
  })

  it('tells a member who can attach one, and asks nothing about the App', async () => {
    show('project=prj_2', member)
    expect(await screen.findByText(/No repository is attached to vision. Only an owner of Ada can attach one./)).toBeTruthy()
    expect(screen.queryByRole('button')).toBeNull()
    expect(fetchGitHubApp).not.toHaveBeenCalled()
  })
})

describe('returning from installing the App', () => {
  it('goes on to GitHub to authorise, carrying the installation in the state', async () => {
    const state = begun()
    show(`project=prj_2&installation_id=4242&setup_action=install&state=${state}`)
    expect(await screen.findByRole('status', { name: 'Going to GitHub to confirm who you are…' })).toBeTruthy()
    const address = new URL(vi.mocked(leaveFor).mock.calls[0][0])
    expect(address.searchParams.get('state')).toBe(`${state}.4242`)
    expect(address.searchParams.get('redirect_uri')).toBe(`${window.location.origin}/settings/repository`)
    expect(address.searchParams.get('client_id')).toBe('Iv1.abc')
  })

  it('first points the URL at the project the attempt began in, which GitHub\'s return does not name', async () => {
    const state = begun('prj_2')
    show(`installation_id=4242&setup_action=install&state=${state}`)
    expect(navigation.replace).toHaveBeenCalledExactlyOnceWith(`/settings/repository?installation_id=4242&setup_action=install&state=${state}&project=prj_2`)
    expect(leaveFor).not.toHaveBeenCalled()
    expect(fetchGitHubApp).toHaveBeenCalled()
  })

  it('is refused, and goes nowhere, with a state this tab did not issue', async () => {
    begun()
    show('project=prj_2&installation_id=4242&setup_action=install&state=forged')
    expect(await screen.findByText(/does not belong to an attach started in this tab/)).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Start again' }).getAttribute('href')).toBe('/settings/repository?project=prj_2')
    expect(leaveFor).not.toHaveBeenCalled()
  })
})

describe('returning from authorising', () => {
  it('attaches the repository named, with the installation and the code, then shows it', async () => {
    const state = begun()
    show(`project=prj_2&code=code-1&state=${state}.4242`)
    await userEvent.type(await screen.findByLabelText('Repository'), ' acme/widgets ')
    await userEvent.click(screen.getByRole('button', { name: 'Attach repository' }))
    expect(attachProjectRepo).toHaveBeenCalledExactlyOnceWith('prj_2', 4242, 'acme/widgets', 'code-1')
    expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/settings/repository?project=prj_2')
    expect(window.sessionStorage.getItem('hl-repository-attach')).toBeNull()
  })

  it('shows the API\'s refusal as it is, keeps the name, and offers to confirm on GitHub again', async () => {
    vi.mocked(attachProjectRepo).mockRejectedValue(new ApiError(403, 'forbidden: your GitHub account cannot push to that repository, and agents would'))
    const state = begun()
    show(`project=prj_2&code=code-1&state=${state}.4242`)
    await userEvent.type(await screen.findByLabelText('Repository'), 'acme/widgets')
    await userEvent.click(screen.getByRole('button', { name: 'Attach repository' }))
    expect(await screen.findByText('forbidden: your GitHub account cannot push to that repository, and agents would')).toBeTruthy()
    expect((screen.getByLabelText('Repository') as HTMLInputElement).value).toBe('acme/widgets')
    expect(navigation.replace).not.toHaveBeenCalled()
    await userEvent.click(screen.getByRole('button', { name: 'confirm on GitHub again' }))
    expect(new URL(vi.mocked(leaveFor).mock.calls[0][0]).searchParams.get('state')).toBe(`${state}.4242`)
  })

  it('posts nothing for a code that came with a forged state', async () => {
    begun()
    show('project=prj_2&code=code-1&state=forged.4242')
    expect(await screen.findByText(/does not belong to an attach started in this tab/)).toBeTruthy()
    expect(screen.queryByLabelText('Repository')).toBeNull()
    expect(attachProjectRepo).not.toHaveBeenCalled()
  })

  it('posts nothing for a code that arrives in a tab that started no attach', async () => {
    show('project=prj_2&code=code-1&state=anything.4242')
    expect(await screen.findByText(/does not belong to an attach started in this tab/)).toBeTruthy()
    expect(screen.queryByLabelText('Repository')).toBeNull()
  })
})

describe('a project with a repository', () => {
  beforeEach(() => { repo = attached })

  it('shows the repository, its default branch and when it was attached', async () => {
    show()
    expect(await screen.findByText('acme/widgets')).toBeTruthy()
    expect(screen.getByText('main')).toBeTruthy()
    expect(screen.getByText('https://github.com/acme/widgets.git')).toBeTruthy()
    expect(screen.getByText(new Date(at).toLocaleString())).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Install the GitHub App' })).toBeNull()
  })

  it('detaches after the owner confirms, and then offers to attach one', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Detach repository' }))
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining('Detach acme/widgets from vision?'))
    expect(detachProjectRepo).toHaveBeenCalledExactlyOnceWith('prj_2')
    expect(await screen.findByRole('button', { name: 'Install the GitHub App' })).toBeTruthy()
  })

  it('detaches nothing when the owner does not confirm', async () => {
    vi.mocked(confirm).mockReturnValue(false)
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Detach repository' }))
    expect(detachProjectRepo).not.toHaveBeenCalled()
  })

  it('shows why a detach failed and keeps the repository on screen', async () => {
    vi.mocked(detachProjectRepo).mockRejectedValue(new Error('Failed to fetch'))
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Detach repository' }))
    expect(await screen.findByText('Failed to fetch')).toBeTruthy()
    expect(screen.getByText('acme/widgets')).toBeTruthy()
  })

  it('shows a member the repository and no way to detach it', async () => {
    show('project=prj_2', member)
    expect(await screen.findByText('acme/widgets')).toBeTruthy()
    expect(screen.queryByRole('button')).toBeNull()
    expect(screen.getByText('Only an owner of the organization can detach it.')).toBeTruthy()
  })
})
