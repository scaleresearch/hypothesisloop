import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, checkGitHubInstallation, fetchGitHubApp, fetchGitHubInstallations, makeGitHubInstallationDefault } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { OrgGitHub } from '@/components/org-github'
import type { GitHubInstallation, Me } from '@/types'

vi.mock('@/lib/api', async importActual => ({
  ...(await importActual<typeof import('@/lib/api')>()),
  checkGitHubInstallation: vi.fn(), fetchGitHubApp: vi.fn(), fetchGitHubInstallations: vi.fn(), makeGitHubInstallationDefault: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const owner: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const member: Me = { ...owner, user_id: 'u2', orgs: [{ ...owner.orgs[0], role: 'member' }] }
const app = { slug: 'hl', client_id: 'app', install_url: 'https://github.test/apps/hl/installations/new', authorize_url: 'https://github.test/authorize', new_repository_url: 'https://github.test/new' }
const acme: GitHubInstallation = { org_id: 'org_a', installation_id: 7, account_login: 'acme', account_type: 'Organization', connected_actor: { kind: 'user', id: 'u1', name: 'Ada' }, connected_at: at, default: false }
const ada: GitHubInstallation = { ...acme, installation_id: 9, account_login: 'ada', account_type: 'User' }

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

beforeEach(() => {
  installations = [acme]
  vi.mocked(fetchGitHubApp).mockResolvedValue(app)
  vi.mocked(fetchGitHubInstallations).mockImplementation(async () => ({ items: [...installations], total: installations.length }))
  vi.mocked(checkGitHubInstallation).mockResolvedValue(undefined)
  vi.mocked(makeGitHubInstallationDefault).mockImplementation(async (_, id) => {
    installations = installations.map(i => ({ ...i, default: i.installation_id === id }))
  })
})

describe('a connected GitHub account', () => {
  it('shows it works when GitHub says the App may create and push there', async () => {
    show()
    const row = (await screen.findByRole('cell', { name: 'acme' })).closest('tr')!
    expect(await within(row).findByText('Working')).toBeTruthy()
    expect(checkGitHubInstallation).toHaveBeenCalledWith('org_a', 7)
  })

  it('says what GitHub refused and leads to the App on GitHub when it was removed there', async () => {
    const gone = 'the GitHub App is no longer installed on acme: GitHub answers that installation 7 was removed or suspended.'
    vi.mocked(checkGitHubInstallation).mockRejectedValue(new ApiError(422, gone, 'installation_gone'))
    show()
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toContain(gone)
    expect(within(alert).getByRole('link', { name: 'Open the App on GitHub' }).getAttribute('href')).toBe(app.install_url)
    vi.mocked(checkGitHubInstallation).mockResolvedValue(undefined)
    await userEvent.click(within(alert).getByRole('button', { name: 'check again' }))
    expect(await screen.findByText('Working')).toBeTruthy()
  })
})

describe('where new repositories go', () => {
  it('is no question with one account connected', async () => {
    show()
    await screen.findByRole('cell', { name: 'acme' })
    expect(screen.queryByRole('columnheader', { name: 'New repositories' })).toBeNull()
  })

  it('is the default an owner picks among several', async () => {
    installations = [acme, ada]
    show()
    const row = (await screen.findByRole('cell', { name: 'ada' })).closest('tr')!
    await userEvent.click(within(row).getByRole('button', { name: 'Make default' }))
    expect(makeGitHubInstallationDefault).toHaveBeenCalledWith('org_a', 9)
    expect(await within(row).findByText('Default')).toBeTruthy()
    const other = screen.getByRole('cell', { name: 'acme' }).closest('tr')!
    expect(within(other).getByRole('button', { name: 'Make default' })).toBeTruthy()
  })

  it('is shown to a member, who changes nothing', async () => {
    installations = [acme, { ...ada, default: true }]
    show(member)
    expect(await screen.findByText('Default')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Make default' })).toBeNull()
  })
})
