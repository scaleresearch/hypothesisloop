import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { StrictMode } from 'react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, attachProjectRepo, detachProjectRepo, fetchGitHubApp, fetchProjectRepo } from '@/lib/api'
import { beginAttach, leaveFor, resetAttach } from '@/lib/repo-attach'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import RepositoryPage from '@/app/(app)/settings/repository/page'
import type { Me, ProjectRepo } from '@/types'

const navigation = vi.hoisted(() => ({ replace: vi.fn(), search: '' }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', async importActual => {
  const actual = await importActual<typeof import('@/lib/api')>()
  return {
    ApiError: actual.ApiError, APP_NOT_INSTALLED: actual.APP_NOT_INSTALLED,
    attachProjectRepo: vi.fn(), detachProjectRepo: vi.fn(), fetchGitHubApp: vi.fn(), fetchProjectRepo: vi.fn(),
  }
})
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
  project_id: 'prj_2', installation_id: 4242, repository_id: 9, repo: 'acme/widgets',
  attached_at: at, clone_url: 'https://github.com/acme/widgets.git',
}

let repos: Record<string, ProjectRepo | null>
let cache: Map<string, any>

// The page with `search` as its query and the project the query names selected, the home project
// when it names none, which is how GitHub's return arrives.
function page(search: string, as: Me) {
  navigation.search = search
  const resolved = resolveTenant(as, new URLSearchParams(search).get('project') ?? '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return (
    <SWRConfig value={{ provider: () => cache, shouldRetryOnError: false }}>
      <TenantContext.Provider value={resolved.tenant}><RepositoryPage key={resolved.tenant.project.id} /></TenantContext.Provider>
    </SWRConfig>
  )
}

function show(search = 'project=prj_2', as: Me = owner) {
  return render(page(search, as))
}

// Follows the page's own router.replace, as the browser would.
function follow(view: ReturnType<typeof render>, as: Me = owner) {
  const [to] = navigation.replace.mock.lastCall!
  view.rerender(page(to.split('?')[1] ?? '', as))
}

// Leaves for GitHub from the form, as a person would, and returns the state GitHub is handed.
function begun(projectID = 'prj_2', repo = 'acme/widgets') {
  return new URL(beginAttach(app.authorize_url, projectID, repo)).searchParams.get('state')!
}

beforeEach(() => {
  window.sessionStorage.clear()
  resetAttach()
  cache = new Map()
  repos = { prj_1: null, prj_2: null }
  vi.mocked(fetchProjectRepo).mockImplementation(async id => repos[id])
  vi.mocked(fetchGitHubApp).mockResolvedValue(app)
  vi.mocked(attachProjectRepo).mockImplementation(async id => (repos[id] = attached))
  vi.mocked(detachProjectRepo).mockImplementation(async id => { repos[id] = null })
})

describe('a project with no repository', () => {
  it('asks an owner for the name, then leaves for GitHub with a state this tab keeps', async () => {
    show()
    await userEvent.type(await screen.findByLabelText('Repository'), ' acme/widgets ')
    await userEvent.click(screen.getByRole('button', { name: 'Connect on GitHub' }))
    const address = new URL(vi.mocked(leaveFor).mock.calls[0][0])
    expect(address.origin + address.pathname).toBe('https://github.test/login/oauth/authorize')
    expect(address.searchParams.get('redirect_uri')).toBe(`${window.location.origin}/settings/repository`)
    expect(JSON.parse(window.sessionStorage.getItem('hl-repository-attach')!)).toEqual({
      nonce: address.searchParams.get('state'), projectID: 'prj_2', repo: 'acme/widgets',
    })
    expect(leaveFor).toHaveBeenCalledTimes(1)
  })

  it('links to installing the App, in a tab of its own', async () => {
    show()
    const link = await screen.findByRole('link', { name: 'Install the App on a repository' })
    expect(link.getAttribute('href')).toBe(app.install_url)
    expect(link.getAttribute('target')).toBe('_blank')
  })

  it.each(['widgets', 'acme/wid/gets', 'https://github.com/acme/widgets'])('goes nowhere with the name "%s", and says what a name is', async name => {
    show()
    await userEvent.type(await screen.findByLabelText('Repository'), name)
    await userEvent.click(screen.getByRole('button', { name: 'Connect on GitHub' }))
    expect(screen.getByText('The repository must be owner/name, as in its address on GitHub.')).toBeTruthy()
    expect(leaveFor).not.toHaveBeenCalled()
    expect(window.sessionStorage.length).toBe(0)
  })

  it('shows why the App cannot be used, in the API\'s words, whatever the failure', async () => {
    vi.mocked(fetchGitHubApp).mockRejectedValue(new ApiError(503, 'this deployment has no GitHub App configured, so no repository can be attached or cloned through it', 'github_not_configured'))
    const first = show()
    expect(await screen.findByText(/this deployment has no GitHub App configured/)).toBeTruthy()
    expect(screen.queryByRole('button')).toBeNull()
    first.unmount()

    cache = new Map()
    vi.mocked(fetchGitHubApp).mockRejectedValue(new Error('Failed to fetch'))
    show()
    expect(await screen.findByText('No repository can be attached right now: Failed to fetch')).toBeTruthy()
    expect(screen.queryByRole('button')).toBeNull()
  })

  it('shows a failed read of the repository as a failure, never as none attached', async () => {
    vi.mocked(fetchProjectRepo).mockRejectedValue(new ApiError(404, 'project not found'))
    show()
    expect(await screen.findByText('Could not load the repository of vision: project not found')).toBeTruthy()
    expect(screen.queryByText(/No repository is attached/)).toBeNull()
    expect(screen.queryByLabelText('Repository')).toBeNull()
  })

  it('tells a member who can attach one, and asks nothing about the App', async () => {
    show('project=prj_2', member)
    expect(await screen.findByText(/No repository is attached to vision. Only an owner of Ada can attach one./)).toBeTruthy()
    expect(screen.queryByRole('button')).toBeNull()
    expect(screen.queryByRole('link')).toBeNull()
    expect(fetchGitHubApp).not.toHaveBeenCalled()
  })
})

describe('returning from installing the App', () => {
  it('drops what GitHub added to the address and shows the same form', async () => {
    const view = show('project=prj_2&installation_id=4242&setup_action=install')
    expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/settings/repository?project=prj_2')
    follow(view)
    expect(await screen.findByLabelText('Repository')).toBeTruthy()
    expect(attachProjectRepo).not.toHaveBeenCalled()
    expect(leaveFor).not.toHaveBeenCalled()
  })
})

describe('returning from GitHub with a code', () => {
  it('attaches the repository named before leaving, once, in the project the attempt began in', async () => {
    const state = begun('prj_2', 'acme/widgets')
    // GitHub's return names no project, so the page first renders in the home project.
    const view = show(`code=code-1&state=${state}`)
    expect(attachProjectRepo).toHaveBeenCalledExactlyOnceWith('prj_2', {
      repo: 'acme/widgets', code: 'code-1', redirectURI: `${window.location.origin}/settings/repository`,
    })
    expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/settings/repository?project=prj_2')
    expect(window.sessionStorage.length).toBe(0)
    expect(screen.queryByLabelText('Repository')).toBeNull()
    follow(view)
    expect(await screen.findByText('acme/widgets')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Detach repository' })).toBeTruthy()
    expect(attachProjectRepo).toHaveBeenCalledTimes(1)
  })

  it('attaches once when React runs effects twice', async () => {
    const state = begun()
    navigation.search = `code=code-1&state=${state}`
    render(<StrictMode>{page(navigation.search, owner)}</StrictMode>)
    await vi.waitFor(() => expect(navigation.replace).toHaveBeenCalledWith('/settings/repository?project=prj_2'))
    expect(attachProjectRepo).toHaveBeenCalledTimes(1)
    expect(screen.queryByText(/does not belong to an attach/)).toBeNull()
  })

  it('shows the attached repository from the attach\'s own answer, with no reread to fail', async () => {
    const state = begun()
    const view = show(`project=prj_2&code=code-1&state=${state}`)
    await screen.findByRole('status')
    vi.mocked(fetchProjectRepo).mockRejectedValue(new Error('Failed to fetch'))
    follow(view)
    expect(await screen.findByText('acme/widgets')).toBeTruthy()
    expect(screen.queryByText(/Failed to fetch|no attach is in progress/)).toBeNull()
  })

  it('shows the API\'s refusal as it is and returns to the form with the name kept', async () => {
    vi.mocked(attachProjectRepo).mockRejectedValue(new ApiError(403, 'forbidden: your GitHub account cannot push to that repository, and agents would'))
    const state = begun()
    const view = show(`code=code-1&state=${state}`)
    follow(view)
    expect(await screen.findByText('forbidden: your GitHub account cannot push to that repository, and agents would')).toBeTruthy()
    expect((screen.getByLabelText('Repository') as HTMLInputElement).value).toBe('acme/widgets')
    expect(screen.getByText(/Not installed there yet\?/)).toBeTruthy()

    // Trying again needs a new code, so it leaves for GitHub again with a new state.
    await userEvent.click(screen.getByRole('button', { name: 'Connect on GitHub' }))
    expect(new URL(vi.mocked(leaveFor).mock.calls[0][0]).searchParams.get('state')).not.toBe(state)
    expect(attachProjectRepo).toHaveBeenCalledTimes(1)
  })

  it('points at installing the App when that is why the attach was refused', async () => {
    vi.mocked(attachProjectRepo).mockRejectedValue(new ApiError(422, 'the GitHub App is not installed on acme/widgets', 'app_not_installed'))
    const view = show(`code=code-1&state=${begun()}`)
    follow(view)
    expect(await screen.findByText('the GitHub App is not installed on acme/widgets')).toBeTruthy()
    expect(screen.getByText(/Install it, then connect again:/)).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Install the App on a repository' }).getAttribute('href')).toBe(app.install_url)
  })

  it('shows a refusal only in the project it was for', async () => {
    vi.mocked(attachProjectRepo).mockRejectedValue(new ApiError(403, 'forbidden: cannot push'))
    const view = show(`code=code-1&state=${begun('prj_2')}`)
    follow(view)
    await screen.findByText('forbidden: cannot push')
    view.rerender(page('project=prj_1', owner))
    expect(((await screen.findByLabelText('Repository')) as HTMLInputElement).value).toBe('')
    expect(screen.queryByText('forbidden: cannot push')).toBeNull()
  })

  it('posts nothing for a state this tab did not issue, and offers to start again', async () => {
    begun()
    const view = show('project=prj_2&code=stolen&state=forged')
    expect(await screen.findByText(/does not belong to an attach started in this tab/)).toBeTruthy()
    expect(attachProjectRepo).not.toHaveBeenCalled()
    expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/settings/repository?project=prj_2')
    follow(view)
    expect(screen.queryByLabelText('Repository')).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Start again' }))
    expect(await screen.findByLabelText('Repository')).toBeTruthy()
  })

  it('posts nothing for a code that arrives in a tab that started no attach', async () => {
    show('code=code-1&state=anything')
    expect(await screen.findByText(/does not belong to an attach started in this tab/)).toBeTruthy()
    expect(attachProjectRepo).not.toHaveBeenCalled()
  })
})

describe('returning from GitHub after declining', () => {
  it('returns to the form with the name kept and GitHub\'s words shown', async () => {
    const state = begun('prj_2', 'acme/widgets')
    const view = show(`error=access_denied&error_description=The+user+has+denied+access&state=${state}`)
    expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/settings/repository?project=prj_2')
    follow(view)
    expect(await screen.findByText('GitHub did not authorise the App: The user has denied access')).toBeTruthy()
    expect((screen.getByLabelText('Repository') as HTMLInputElement).value).toBe('acme/widgets')
    expect(attachProjectRepo).not.toHaveBeenCalled()
  })
})

describe('a project with a repository', () => {
  beforeEach(() => { repos.prj_2 = attached })

  it('shows an owner the repository, where it is cloned from and when it was attached, and no form', async () => {
    show()
    expect(await screen.findByText('acme/widgets')).toBeTruthy()
    expect(screen.getByText('https://github.com/acme/widgets.git')).toBeTruthy()
    expect(screen.getByText(new Date(at).toLocaleString())).toBeTruthy()
    expect(screen.getByText('To attach another repository, detach this one first.')).toBeTruthy()
    expect(screen.queryByLabelText('Repository')).toBeNull()
  })

  it('detaches after the owner confirms, and then offers the form', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Detach repository' }))
    const question = within(screen.getByRole('dialog'))
    expect(question.getByText('Detach acme/widgets from vision?')).toBeTruthy()
    expect(question.getByText('Agents and jobs of its running platform experiments can no longer clone, fetch or push within the hour.')).toBeTruthy()
    await userEvent.click(question.getByRole('button', { name: 'Detach' }))
    expect(detachProjectRepo).toHaveBeenCalledExactlyOnceWith('prj_2')
    expect(await screen.findByLabelText('Repository')).toBeTruthy()
  })

  it('detaches nothing when the owner does not confirm', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Detach repository' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(detachProjectRepo).not.toHaveBeenCalled()
    expect(screen.getByText('acme/widgets')).toBeTruthy()
  })

  it('shows why a detach failed and keeps the repository on screen', async () => {
    vi.mocked(detachProjectRepo).mockRejectedValue(new Error('Failed to fetch'))
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Detach repository' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Detach' }))
    expect(await screen.findByText('Failed to fetch')).toBeTruthy()
    expect(screen.getByText('acme/widgets')).toBeTruthy()
  })

  it('shows a member the repository and no way to detach it', async () => {
    show('project=prj_2', member)
    expect(await screen.findByText('acme/widgets')).toBeTruthy()
    expect(screen.queryByRole('button')).toBeNull()
    expect(screen.getByText('Only an owner of the organization can detach it.')).toBeTruthy()
    expect(fetchGitHubApp).not.toHaveBeenCalled()
  })
})
