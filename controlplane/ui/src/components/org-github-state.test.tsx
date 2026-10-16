import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { beginGitHubInstall, checkGitHubInstallation, fetchGitHubApp, fetchGitHubInstallations, makeGitHubInstallationDefault } from '@/lib/api/github'
import { ApiError } from '@/lib/api/request'
import type { Me } from '@/types/accounts'
import type { GitHubInstallation } from '@/types/github'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { OrgGitHub } from '@/components/org-github'

vi.mock('@/lib/api/github', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/github')>()),
  beginGitHubInstall: vi.fn(), checkGitHubInstallation: vi.fn(), fetchGitHubApp: vi.fn(), fetchGitHubInstallations: vi.fn(), makeGitHubInstallationDefault: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const owner: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const member: Me = { ...owner, user_id: 'u2', orgs: [{ ...owner.orgs[0], role: 'member' }] }
const app = { slug: 'hl', install_url: 'https://github.test/apps/hl/installations/new', new_repository_url: 'https://github.test/new' }
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

  it('names its kind of account', async () => {
    installations = [acme, ada]
    show()
    expect((await screen.findByRole('cell', { name: 'acme' })).closest('tr')!.textContent).toContain('Organization')
    expect(screen.getByRole('cell', { name: 'ada' }).closest('tr')!.textContent).toContain('Personal')
  })
})

// Each refusal of the check, as the chip it shows, the explanation behind it and its one action.
describe('a connected GitHub account GitHub refuses', () => {
  const cases: { reason: string; message: string; chip: string; explain: string; action: string; href?: string }[] = [
    { reason: 'installation_gone', message: 'the GitHub App is no longer installed on acme (installation 7): its owner installs the App again from Settings, GitHub', chip: 'Not installed', explain: 'The GitHub App was removed on acme, so experiments cannot use its repositories. Reinstall it to connect again.', action: 'Reinstall' },
    { reason: 'installation_suspended', message: 'the GitHub App is suspended on acme (installation 7): its owner unsuspends it on GitHub', chip: 'Suspended', explain: 'The GitHub App is suspended on acme. Unsuspend it on GitHub to use its repositories again.', action: 'Review on GitHub', href: 'https://github.com/organizations/acme/settings/installations/7' },
    { reason: 'github_permission_missing', message: 'github_permission_missing: administration:write, contents:write. The GitHub App installation has not granted that', chip: 'Missing permission', explain: 'The GitHub App on acme has not been granted administration (write), contents (write). Accept the permissions on GitHub.', action: 'Review on GitHub', href: 'https://github.com/organizations/acme/settings/installations/7' },
    { reason: 'github_failed', message: 'GitHub failed the platform\'s call for GitHub App installation 7: it answered 502', chip: 'No answer', explain: 'GitHub did not answer the check. Try again in a moment.', action: 'Check again' },
  ]
  for (const tc of cases) {
    it(`shows ${tc.chip} with ${tc.action}, and the API's words nowhere in the row`, async () => {
      vi.mocked(checkGitHubInstallation).mockRejectedValue(new ApiError(422, tc.message, tc.reason))
      show()
      const row = (await screen.findByRole('cell', { name: 'acme' })).closest('tr')!
      const chip = (await within(row).findByText(tc.chip)).closest('[title]')!
      expect(chip.getAttribute('title')).toBe(tc.explain)
      expect(chip.getAttribute('tabindex')).toBe('0')
      expect(chip.textContent).toContain(tc.explain)
      expect(row.textContent).not.toContain(tc.message)
      const action = within(row).getByRole(tc.href ? 'link' : 'button', { name: tc.action })
      if (tc.href) expect(action.getAttribute('href')).toBe(tc.href)
    })
  }

  it('reinstalls in a new tab, as Install GitHub App does', async () => {
    vi.mocked(checkGitHubInstallation).mockRejectedValue(new ApiError(422, 'gone', 'installation_gone'))
    vi.spyOn(window, 'open').mockReturnValue({ location: { href: '' }, close: vi.fn() } as unknown as Window)
    vi.mocked(beginGitHubInstall).mockResolvedValue({ state: 's-1', install_url: `${app.install_url}?state=s-1`, expires_at: at })
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Reinstall' }))
    expect(beginGitHubInstall).toHaveBeenCalledExactlyOnceWith('org_a', '/orgs/org_a/settings#github')
    expect((await screen.findByRole('status')).textContent).toContain('Waiting for GitHub')
  })

  it('checks again on request', async () => {
    vi.mocked(checkGitHubInstallation).mockRejectedValue(new ApiError(502, 'no answer', 'github_failed'))
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Check again' }))
    vi.mocked(checkGitHubInstallation).mockResolvedValue(undefined)
    await userEvent.click(screen.getByRole('button', { name: 'Check again' }))
    expect(await screen.findByText('Working')).toBeTruthy()
  })

  it('offers a member no action that installs', async () => {
    vi.mocked(checkGitHubInstallation).mockRejectedValue(new ApiError(422, 'gone', 'installation_gone'))
    show(member)
    expect(await screen.findByText('Not installed')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Reinstall' })).toBeNull()
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
