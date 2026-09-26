import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, acceptOrgInvite, fetchMe } from '@/lib/api'
import { accessToken } from '@/lib/auth'
import { AcceptInvite } from '@/components/accept-invite'
import type { Me } from '@/types'

const router = vi.hoisted(() => ({ push: vi.fn(), replace: vi.fn() }))

vi.mock('next/navigation', () => ({ useRouter: () => router }))
vi.mock('@/lib/api', async importActual => ({
  ApiError: (await importActual<typeof import('@/lib/api')>()).ApiError,
  acceptOrgInvite: vi.fn(), fetchMe: vi.fn(),
}))
vi.mock('@/lib/auth', async importActual => ({
  ...(await importActual<typeof import('@/lib/auth')>()),
  accessToken: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const TOKEN = 'hli_the-invite-token'
const STORED = 'hl-invite'
const me: Me = {
  kind: 'user',
  user_id: 'u2',
  name: 'Bob',
  orgs: [
    { id: 'org_b', name: 'Bob', role: 'owner', created_at: at },
    { id: 'org_a', name: 'Lab', role: 'member', created_at: at },
  ],
  projects: [
    { id: 'prj_b', org_id: 'org_b', name: 'default', created_at: at },
    { id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at },
  ],
}

function open(fragment = TOKEN) {
  window.history.replaceState(null, '', fragment ? `/invite#${fragment}` : '/invite')
  return render(<SWRConfig value={{ provider: () => new Map() }}><AcceptInvite /></SWRConfig>)
}

async function accept() {
  await userEvent.click(await screen.findByRole('button', { name: 'Accept invite' }))
}

beforeEach(() => {
  window.sessionStorage.clear()
  router.push.mockReset()
  router.replace.mockReset()
  vi.mocked(accessToken).mockResolvedValue('session-token')
  vi.mocked(fetchMe).mockResolvedValue(me)
  vi.mocked(acceptOrgInvite).mockResolvedValue({ org_id: 'org_a', user_id: 'u2', name: 'Bob', role: 'member', joined_at: at })
})

describe('opening an invite link', () => {
  it('takes the token out of the address and keeps it in this tab', async () => {
    open()
    await screen.findByRole('button', { name: 'Accept invite' })
    expect(window.location.hash).toBe('')
    expect(window.location.pathname).toBe('/invite')
    expect(window.sessionStorage.getItem(STORED)).toBe(TOKEN)
    expect(document.body.textContent).not.toContain(TOKEN)
  })

  it('sends nothing until the person accepts', async () => {
    open()
    await screen.findByRole('button', { name: 'Accept invite' })
    expect(acceptOrgInvite).not.toHaveBeenCalled()
    expect(fetchMe).not.toHaveBeenCalled()
  })

  it('says so when the page was opened with no invite', async () => {
    open('')
    expect(await screen.findByText(/This page holds no invite/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Accept invite' })).toBeNull()
    expect(accessToken).not.toHaveBeenCalled()
  })
})

describe('a visitor without a session', () => {
  beforeEach(() => vi.mocked(accessToken).mockResolvedValue(null))

  it('is sent to sign in with this page as the way back, the token staying in the tab', async () => {
    open()
    await userEvent.click(await screen.findByRole('button', { name: 'Sign in to accept' }))
    expect(router.push).toHaveBeenCalledExactlyOnceWith('/login?next=%2Finvite')
    expect(window.sessionStorage.getItem(STORED)).toBe(TOKEN)
    expect(acceptOrgInvite).not.toHaveBeenCalled()
  })

  it('finds the invite again on the way back from sign-in, where the address no longer carries it', async () => {
    const before = open()
    await screen.findByRole('button', { name: 'Sign in to accept' })
    before.unmount()

    vi.mocked(accessToken).mockResolvedValue('session-token')
    open('')
    await accept()
    expect(acceptOrgInvite).toHaveBeenCalledExactlyOnceWith(TOKEN)
  })
})

describe('accepting', () => {
  it('joins, rereads the account and opens the home of the org that was joined', async () => {
    open()
    await accept()
    expect(acceptOrgInvite).toHaveBeenCalledExactlyOnceWith(TOKEN)
    await waitFor(() => expect(router.replace).toHaveBeenCalledExactlyOnceWith('/console?project=prj_1'))
    expect(fetchMe).toHaveBeenCalledTimes(1)
    expect(screen.getByText(/You joined as a member/)).toBeTruthy()
  })

  it('is single use: the token is gone from the tab once the API has answered', async () => {
    open()
    await accept()
    await waitFor(() => expect(router.replace).toHaveBeenCalled())
    expect(window.sessionStorage.getItem(STORED)).toBeNull()
  })

  it('names the role when the invite made an owner', async () => {
    vi.mocked(acceptOrgInvite).mockResolvedValue({ org_id: 'org_a', user_id: 'u2', name: 'Bob', role: 'owner', joined_at: at })
    open()
    await accept()
    expect(await screen.findByText(/You joined as an owner/)).toBeTruthy()
  })

  it('says so when the org that was joined has no project to open', async () => {
    vi.mocked(fetchMe).mockResolvedValue({ ...me, projects: [me.projects[0]] })
    open()
    await accept()
    expect(await screen.findByText('The organization has no project yet.')).toBeTruthy()
    expect(router.replace).not.toHaveBeenCalled()
    await userEvent.click(screen.getByRole('button', { name: 'Go to experiments' }))
    expect(router.replace).toHaveBeenCalledExactlyOnceWith('/console')
  })

  it.each([
    ['invite_used', 409, 'the invite was already accepted', /already accepted, and an invite works once/],
    ['invite_revoked', 409, 'the invite was revoked', /This invite was revoked/],
    ['invite_expired', 409, 'the invite has expired: ask an owner of the org for a new one', /This invite has expired/],
    ['already_member', 409, 'you are already a member of this org; the invite is still unused', /You are already a member of this organization/],
    ['not_found', 404, 'invite not found', /This invite link is not valid/],
  ])('says what %s means, joins nothing and forgets the token', async (reason, status, message, shown) => {
    vi.mocked(acceptOrgInvite).mockRejectedValue(new ApiError(status, message, reason))
    open()
    await accept()
    expect(await screen.findByText(shown)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Accept invite' })).toBeNull()
    expect(window.sessionStorage.getItem(STORED)).toBeNull()
    expect(router.replace).not.toHaveBeenCalled()
    expect(fetchMe).not.toHaveBeenCalled()
  })

  it('shows a refusal it has no words for in the API\'s own', async () => {
    vi.mocked(acceptOrgInvite).mockRejectedValue(new ApiError(403, 'forbidden: an API key is no person'))
    open()
    await accept()
    expect(await screen.findByText('forbidden: an API key is no person')).toBeTruthy()
  })

  it('keeps the invite when the API never answered, so the same token can be sent again', async () => {
    vi.mocked(acceptOrgInvite).mockRejectedValueOnce(new TypeError('Failed to fetch'))
    open()
    await accept()
    expect(await screen.findByText('The invite could not be sent: Failed to fetch')).toBeTruthy()
    expect(window.sessionStorage.getItem(STORED)).toBe(TOKEN)
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
    await waitFor(() => expect(router.replace).toHaveBeenCalledExactlyOnceWith('/console?project=prj_1'))
    expect(acceptOrgInvite).toHaveBeenCalledTimes(2)
  })
})
