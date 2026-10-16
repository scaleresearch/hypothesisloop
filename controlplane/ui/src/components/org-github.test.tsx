import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, checkGitHubInstallation, connectGitHubInstallation, disconnectGitHubInstallation, fetchGitHubApp, fetchGitHubInstallations } from '@/lib/api'
import { beginConnect, leaveFor, resetConnect, takeReturn } from '@/lib/github-connect'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { OrgGitHub } from '@/components/org-github'
import type { GitHubInstallation, Me } from '@/types'

vi.mock('@/lib/api', async importActual => ({
  ...(await importActual<typeof import('@/lib/api')>()),
  checkGitHubInstallation: vi.fn(), connectGitHubInstallation: vi.fn(), disconnectGitHubInstallation: vi.fn(), fetchGitHubApp: vi.fn(), fetchGitHubInstallations: vi.fn(),
}))
vi.mock('@/lib/github-connect', async importActual => ({
  ...(await importActual<typeof import('@/lib/github-connect')>()),
  leaveFor: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const owner: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const member: Me = { ...owner, user_id: 'u2', orgs: [{ ...owner.orgs[0], role: 'member' }] }
const app = { slug: 'hl', client_id: 'app', install_url: 'https://github.test/apps/hl/installations/new', authorize_url: 'https://github.test/login/oauth/authorize?client_id=app', new_repository_url: 'https://github.test/new' }
const acme: GitHubInstallation = { org_id: 'org_a', installation_id: 7, account_login: 'acme', connected_actor: { kind: 'user', id: 'u1', name: 'Ada Lovelace' }, connected_at: at, default: false }
const lab: GitHubInstallation = { org_id: 'org_a', installation_id: 9, account_login: 'lab', connected_actor: { kind: 'key', id: 'key_3', name: 'ci' }, connected_at: at, default: false }

let installations: GitHubInstallation[]

function show(as: Me = owner) {
  const resolved = resolveTenant(as, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><OrgGitHub /></TenantContext.Provider>
    </SWRConfig>,
  )
}

// Leaves for GitHub as the card does and comes back with what GitHub would return.
async function comeBack(query: (state: string) => string, installationID?: number) {
  const address = beginConnect(app.authorize_url, { orgID: 'org_a', returnTo: '/orgs/org_a/settings#github' }, installationID)
  return takeReturn(query(new URL(address).searchParams.get('state')!), {
    app: async () => app,
    connect: (orgID, code, id) => connectGitHubInstallation(orgID, { code, redirectURI: 'u', installationID: id }),
    installations: async () => installations,
  })
}

beforeEach(() => {
  window.sessionStorage.clear()
  resetConnect()
  installations = [acme]
  vi.mocked(fetchGitHubApp).mockResolvedValue(app)
  vi.mocked(checkGitHubInstallation).mockResolvedValue(undefined)
  vi.mocked(fetchGitHubInstallations).mockImplementation(async () => ({ items: [...installations], total: installations.length }))
  vi.mocked(disconnectGitHubInstallation).mockImplementation(async (_, id) => { installations = installations.filter(i => i.installation_id !== id) })
  vi.mocked(connectGitHubInstallation).mockImplementation(async () => {
    installations = [...installations, lab]
    return lab
  })
})

describe('the connected GitHub accounts', () => {
  it('say why they could not be read', async () => {
    vi.mocked(fetchGitHubInstallations).mockRejectedValue(new Error('API unreachable'))
    show()
    expect((await screen.findByRole('alert')).textContent).toBe('Could not load the GitHub accounts of Lab: API unreachable')
  })
})

describe('a member who is no owner', () => {
  it('reads the list and is offered nothing to change, and the App is not asked about', async () => {
    show(member)
    await screen.findByRole('cell', { name: 'acme' })
    expect(screen.queryByRole('button')).toBeNull()
    expect(fetchGitHubApp).not.toHaveBeenCalled()
  })
})

describe('connecting', () => {
  it('leaves for GitHub\'s authorisation with a state, and where to return', async () => {
    installations = []
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Connect GitHub' }))
    const address = new URL(vi.mocked(leaveFor).mock.calls[0][0])
    expect(address.origin + address.pathname).toBe('https://github.test/login/oauth/authorize')
    expect(address.searchParams.get('redirect_uri')).toBe(`${window.location.origin}/settings/repository`)
    expect(address.searchParams.get('state')).toBeTruthy()
    expect(address.searchParams.has('installation_id')).toBe(false)
    expect(screen.getByRole('button', { name: 'Connect GitHub' })).toHaveProperty('disabled', true)
  })

  it('asks which account when the person reaches several, and authorises again for the one chosen', async () => {
    vi.mocked(connectGitHubInstallation).mockRejectedValue(new ApiError(409, 'name one', 'installation_choice_required', {
      installations: [{ installation_id: 7, account_login: 'acme' }, { installation_id: 9, account_login: 'lab' }],
    }))
    await comeBack(state => `code=c&state=${state}`)
    show()
    const choice = within(await screen.findByRole('group', { name: 'GitHub account to connect' }))
    expect(choice.getByText('Which GitHub account do you connect to Lab?')).toBeTruthy()
    await userEvent.click(choice.getByRole('button', { name: 'lab' }))
    const address = new URL(vi.mocked(leaveFor).mock.calls[0][0])
    expect(address.searchParams.get('state')).toBeTruthy()

    vi.mocked(connectGitHubInstallation).mockImplementation(async () => {
      installations = [...installations, lab]
      return lab
    })
    await takeReturn(`code=c2&state=${address.searchParams.get('state')}`, {
      app: async () => app,
      connect: (orgID, code, id) => connectGitHubInstallation(orgID, { code, redirectURI: 'u', installationID: id }),
      installations: async () => installations,
    })
    expect(connectGitHubInstallation).toHaveBeenLastCalledWith('org_a', { code: 'c2', redirectURI: 'u', installationID: 9 })
    expect(await screen.findByRole('cell', { name: 'lab' })).toBeTruthy()
  })

  it('offers to install the App on another account instead of those listed', async () => {
    vi.mocked(connectGitHubInstallation).mockRejectedValue(new ApiError(409, 'name one', 'installation_choice_required', {
      installations: [{ installation_id: 7, account_login: 'acme' }, { installation_id: 9, account_login: 'lab' }],
    }))
    await comeBack(state => `code=c&state=${state}`)
    show()
    await userEvent.click(within(await screen.findByRole('group', { name: 'GitHub account to connect' })).getByRole('button', { name: 'Another account' }))
    expect(leaveFor).toHaveBeenCalledExactlyOnceWith(app.install_url)
  })

  it('lets the person drop the choice', async () => {
    vi.mocked(connectGitHubInstallation).mockRejectedValue(new ApiError(409, 'name one', 'installation_choice_required', {
      installations: [{ installation_id: 7, account_login: 'acme' }],
    }))
    await comeBack(state => `code=c&state=${state}`)
    show()
    await userEvent.click(within(await screen.findByRole('group', { name: 'GitHub account to connect' })).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('group', { name: 'GitHub account to connect' })).toBeNull()
  })

  it('goes on to install the App when the person controls no account to connect', async () => {
    vi.mocked(connectGitHubInstallation).mockRejectedValue(new ApiError(422, 'Install the GitHub App', 'app_not_installed'))
    await expect(comeBack(state => `code=c&state=${state}`)).resolves.toEqual({ to: 'github', address: app.install_url })
    show()
    await screen.findByRole('cell', { name: 'acme' })
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('shows a refusal in the API\'s one sentence', async () => {
    vi.mocked(connectGitHubInstallation).mockRejectedValue(new ApiError(422, 'Ask an admin of the GitHub organisation acme to connect it.', 'installation_not_controlled'))
    await comeBack(state => `code=c&state=${state}`)
    show()
    expect((await screen.findByRole('alert')).textContent).toBe('Ask an admin of the GitHub organisation acme to connect it.')
  })

  it('shows GitHub\'s refusal when the person declined', async () => {
    await comeBack(state => `error=access_denied&error_description=denied&state=${state}`)
    show()
    expect(await screen.findByText('GitHub did not authorise the App: denied')).toBeTruthy()
    expect(connectGitHubInstallation).not.toHaveBeenCalled()
  })

  it('says nothing of a connect that belongs to another org', async () => {
    const address = beginConnect(app.authorize_url, { orgID: 'org_other', returnTo: '/orgs/org_other/settings#github' })
    await takeReturn(`code=c&state=${new URL(address).searchParams.get('state')}`, {
      app: async () => app, connect: async () => ({ ...lab, org_id: 'org_other' }), installations: async () => [],
    })
    show()
    await screen.findByRole('cell', { name: 'acme' })
    expect(fetchGitHubInstallations).toHaveBeenCalledTimes(1)
  })

  it('is not offered, and says why, on a deployment without a GitHub App', async () => {
    vi.mocked(fetchGitHubApp).mockRejectedValue(new ApiError(503, 'no GitHub App', 'github_not_configured'))
    show()
    expect(await screen.findByText('This deployment has no GitHub App configured, so no GitHub account can be connected.')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Connect GitHub' })).toBeNull()
    expect(await screen.findByRole('cell', { name: 'acme' })).toBeTruthy()
  })

  it('says why when the App cannot be read for another reason', async () => {
    vi.mocked(fetchGitHubApp).mockRejectedValue(new Error('API unreachable'))
    show()
    expect(await screen.findByText('GitHub cannot be connected right now: API unreachable')).toBeTruthy()
  })
})

describe('disconnecting', () => {
  it('asks first, says what stops working, then removes the account from the list', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Disconnect' }))
    expect(disconnectGitHubInstallation).not.toHaveBeenCalled()
    const question = within(screen.getByRole('dialog'))
    expect(question.getByText('Disconnect acme from Lab?')).toBeTruthy()
    expect(question.getByText(/can no longer\s+clone, fetch or push within the hour/)).toBeTruthy()
    await userEvent.click(question.getByRole('button', { name: 'Disconnect' }))
    expect(disconnectGitHubInstallation).toHaveBeenCalledExactlyOnceWith('org_a', 7)
    await waitFor(() => expect(screen.queryByRole('cell', { name: 'acme' })).toBeNull())
  })

  it('changes nothing when the answer is no', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Disconnect' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    expect(disconnectGitHubInstallation).not.toHaveBeenCalled()
    expect(screen.getByRole('cell', { name: 'acme' })).toBeTruthy()
  })

  it('shows the API\'s refusal and keeps the account listed', async () => {
    vi.mocked(disconnectGitHubInstallation).mockRejectedValue(new Error('not an owner'))
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Disconnect' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Disconnect' }))
    expect(await screen.findByText('not an owner')).toBeTruthy()
    expect(screen.getByRole('cell', { name: 'acme' })).toBeTruthy()
  })
})
