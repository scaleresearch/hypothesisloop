import { render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { connectGitHubInstallation, fetchGitHubApp, fetchGitHubInstallations } from '@/lib/api'
import { leaveFor, takeReturn } from '@/lib/github-connect'
import type { Next } from '@/lib/github-connect'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import GitHubReturnPage from '@/app/(app)/settings/repository/page'
import type { Me } from '@/types'

const navigation = vi.hoisted(() => ({ search: '', replace: vi.fn() }))

vi.mock('next/navigation', () => ({
  useRouter: () => ({ replace: navigation.replace }),
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', () => ({ connectGitHubInstallation: vi.fn(), fetchGitHubApp: vi.fn(), fetchGitHubInstallations: vi.fn() }))
vi.mock('@/lib/github-connect', async importActual => ({
  ...(await importActual<typeof import('@/lib/github-connect')>()),
  leaveFor: vi.fn(), takeReturn: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

function arrive(query: string, next: Next) {
  navigation.search = query
  vi.mocked(takeReturn).mockResolvedValue(next)
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(<TenantContext.Provider value={resolved.tenant}><GitHubReturnPage /></TenantContext.Provider>)
}

beforeEach(() => {
  navigation.replace.mockReset()
})

describe('the address GitHub returns to', () => {
  it('takes the return and lands on the page the person left from', async () => {
    arrive('code=c&state=s', { to: 'app', path: '/projects/prj_1/experiments?draft=draft-1' })
    expect(screen.getByRole('status', { name: 'Returning from GitHub…' })).toBeTruthy()
    expect(vi.mocked(takeReturn).mock.calls[0][0]).toBe('code=c&state=s')
    await waitFor(() => expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/projects/prj_1/experiments?draft=draft-1'))
    expect(leaveFor).not.toHaveBeenCalled()
  })

  it('lands on the GitHub card of the caller\'s own org when the return names no page, as on a plain visit', async () => {
    arrive('', { to: 'app', path: null })
    await waitFor(() => expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/orgs/org_a/settings#github'))
  })

  it('goes on to GitHub when the App was just installed and is yet to be authorised', async () => {
    arrive('installation_id=4242&setup_action=install', { to: 'github', address: 'https://github.test/authorize?state=n' })
    await waitFor(() => expect(leaveFor).toHaveBeenCalledExactlyOnceWith('https://github.test/authorize?state=n'))
    expect(navigation.replace).not.toHaveBeenCalled()
  })

  it('connects through the API with the code, where GitHub returned it and the installation', async () => {
    arrive('code=c&state=s', { to: 'app', path: '/orgs/org_a/settings#github' })
    const calls = vi.mocked(takeReturn).mock.calls[0][1]
    vi.mocked(fetchGitHubApp).mockResolvedValue({ slug: 'hl', client_id: 'app', install_url: 'i', authorize_url: 'a', new_repository_url: 'n' })
    await expect(calls.app()).resolves.toMatchObject({ authorize_url: 'a', install_url: 'i' })
    await calls.connect('org_a', 'c', 9)
    expect(connectGitHubInstallation).toHaveBeenCalledExactlyOnceWith('org_a', {
      code: 'c', redirectURI: `${window.location.origin}/settings/repository`, installationID: 9,
    })
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [], total: 0 })
    await expect(calls.installations('org_a')).resolves.toEqual([])
    expect(fetchGitHubInstallations).toHaveBeenCalledExactlyOnceWith('org_a')
  })
})
