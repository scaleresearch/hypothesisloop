import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { beginGitHubInstall, checkGitHubInstallation, disconnectGitHubInstallation, fetchGitHubApp, fetchGitHubInstallations } from '@/lib/api/github'
import { ApiError } from '@/lib/api/request'
import { takeReturn } from '@/lib/github-connect'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { OrgGitHub } from '@/components/org-github'
import type { Me, OrgGitHubInstallation } from '@/types/api'

vi.mock('@/lib/api/github', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/github')>()),
  beginGitHubInstall: vi.fn(), checkGitHubInstallation: vi.fn(), disconnectGitHubInstallation: vi.fn(), fetchGitHubApp: vi.fn(), fetchGitHubInstallations: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const owner: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'owner', created_at: at, actions: ['edit', 'create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const member: Me = { ...owner, user_id: 'u2', orgs: [{ ...owner.orgs[0], role: 'member', actions: ['create_api_key'] }] }
const app = { slug: 'hl', install_url: 'https://github.test/apps/hl/installations/new', new_repository_url: 'https://github.test/new' }
const acme: OrgGitHubInstallation = { org_id: 'org_a', installation_id: 7, account_login: 'acme', connected_actor: { kind: 'user', id: 'u1', name: 'Ada Lovelace' }, connected_at: at, default: false }
const lab: OrgGitHubInstallation = { org_id: 'org_a', installation_id: 9, account_login: 'lab', connected_actor: { kind: 'key', id: 'key_3', name: 'ci' }, connected_at: at, default: false }

let installations: OrgGitHubInstallation[]

function show(as: Me = owner) {
  const resolved = resolveTenant(as, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><OrgGitHub /></TenantContext.Provider>
    </SWRConfig>,
  )
}

// The tab GitHub's install page opens in.
let opened: { location: { href: string }; close: () => void }

beforeEach(() => {
  installations = [acme]
  opened = { location: { href: '' }, close: vi.fn() }
  vi.spyOn(window, 'open').mockReturnValue(opened as unknown as Window)
  vi.mocked(beginGitHubInstall).mockResolvedValue({ state: 's-1', install_url: `${app.install_url}?state=s-1`, expires_at: at })
  vi.mocked(fetchGitHubApp).mockResolvedValue(app)
  vi.mocked(checkGitHubInstallation).mockResolvedValue({ working: true })
  vi.mocked(fetchGitHubInstallations).mockImplementation(async () => ({ items: [...installations], total: installations.length }))
  vi.mocked(disconnectGitHubInstallation).mockImplementation(async (_, id) => { installations = installations.filter(i => i.installation_id !== id) })
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

describe('installing the GitHub App', () => {
  it('opens GitHub in a new tab with a state for this org and page, waits, and lists the account the other tab connected', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Install GitHub App' }))
    expect(window.open).toHaveBeenCalledExactlyOnceWith('', '_blank')
    expect(beginGitHubInstall).toHaveBeenCalledExactlyOnceWith('org_a', '/orgs/org_a/settings#github')
    expect((await screen.findByRole('status')).textContent).toContain('Waiting for GitHub')
    expect(opened.location.href).toBe(`${app.install_url}?state=s-1`)
    expect(screen.getByRole('button', { name: 'Install GitHub App' })).toHaveProperty('disabled', true)
    expect(screen.getByText('Install it on your account or an organization.')).toBeTruthy()

    // The tab GitHub returns to connects lab and tells this one.
    installations = [...installations, lab]
    await takeReturn('installation_id=9&setup_action=install&state=s-1', async () => ({ org_id: 'org_a', return_to: '/orgs/org_a/settings#github', installation: lab }))
    expect(await screen.findByText('lab is connected.')).toBeTruthy()
    expect(await screen.findByRole('cell', { name: 'lab' })).toBeTruthy()
  })

  it('shows the refusal the other tab was given', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Install GitHub App' }))
    await screen.findByRole('status')
    await takeReturn('installation_id=9&state=s-1', () => Promise.reject(new ApiError(422, 'GitHub says the App on lab was last changed before this install started.', 'installation_not_new')))
    expect((await screen.findByRole('alert')).textContent).toBe('GitHub says the App on lab was last changed before this install started.')
    expect(screen.queryByRole('cell', { name: 'lab' })).toBeNull()
  })

  it('stops waiting on Cancel', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Install GitHub App' }))
    await userEvent.click(within(await screen.findByRole('status')).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('status')).toBeNull()
    expect(screen.getByRole('button', { name: 'Install GitHub App' })).toHaveProperty('disabled', false)
  })

  it('says why the tab did not open', async () => {
    vi.mocked(window.open).mockReturnValue(null)
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Install GitHub App' }))
    expect((await screen.findByRole('alert')).textContent).toContain('Allow pop-ups for this site')
  })

  it('is not offered, and says why, on a deployment without a GitHub App', async () => {
    vi.mocked(fetchGitHubApp).mockRejectedValue(new ApiError(503, 'no GitHub App', 'github_not_configured'))
    show()
    expect(await screen.findByText('This deployment has no GitHub App configured, so no GitHub account can be connected.')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Install GitHub App' })).toBeNull()
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
