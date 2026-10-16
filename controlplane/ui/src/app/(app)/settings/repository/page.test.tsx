import { render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { finishGitHubInstall } from '@/lib/api'
import { takeReturn } from '@/lib/github-connect'
import type { ReturnView } from '@/lib/github-connect'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import GitHubReturnPage from '@/app/(app)/settings/repository/page'
import type { Me } from '@/types'

const navigation = vi.hoisted(() => ({ search: '' }))

vi.mock('next/navigation', () => ({
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', () => ({ finishGitHubInstall: vi.fn() }))
vi.mock('@/lib/github-connect', async importActual => ({
  ...(await importActual<typeof import('@/lib/github-connect')>()),
  takeReturn: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

function arrive(query: string, view: ReturnView) {
  navigation.search = query
  vi.mocked(takeReturn).mockResolvedValue(view)
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(<TenantContext.Provider value={resolved.tenant}><GitHubReturnPage /></TenantContext.Provider>)
}

let close: ReturnType<typeof vi.spyOn>

beforeEach(() => {
  close = vi.spyOn(window, 'close').mockImplementation(() => {})
})

describe('the setup URL GitHub returns the install tab to', () => {
  it('connects through the API with what GitHub handed back, then closes itself', async () => {
    arrive('installation_id=7&setup_action=install&state=s', { view: 'connected', account: 'acme', returnTo: '/projects/prj_1/experiments' })
    expect(screen.getByRole('status', { name: 'Connecting GitHub…' })).toBeTruthy()
    expect(vi.mocked(takeReturn).mock.calls[0][0]).toBe('installation_id=7&setup_action=install&state=s')
    expect(vi.mocked(takeReturn).mock.calls[0][1]).toBe(finishGitHubInstall)
    await waitFor(() => expect(close).toHaveBeenCalledTimes(1))
  })

  it('says it is done and leads back when the browser does not let it close', async () => {
    arrive('installation_id=7&state=s', { view: 'connected', account: 'acme', returnTo: '/projects/prj_1/experiments' })
    expect(await screen.findByText('Connected acme. You can close this tab.')).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Back to HypothesisLoop' }).getAttribute('href')).toBe('/projects/prj_1/experiments')
  })

  it('shows a refusal and leads to the GitHub card of the org', async () => {
    arrive('installation_id=7&state=forged', { view: 'failed', message: 'This return from GitHub does not match an install you started.' })
    expect((await screen.findByRole('alert')).textContent).toBe('This return from GitHub does not match an install you started.')
    expect(close).toHaveBeenCalledTimes(1)
    expect(screen.getByRole('link', { name: 'Back to HypothesisLoop' }).getAttribute('href')).toBe('/orgs/org_a/settings#github')
  })

  it('stays open on a plain visit', async () => {
    arrive('', { view: 'nothing' })
    expect(await screen.findByText('This page finishes an install of the GitHub App. Nothing was connected.')).toBeTruthy()
    expect(close).not.toHaveBeenCalled()
  })
})
