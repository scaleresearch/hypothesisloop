import { render, screen, waitFor, within } from '@testing-library/react'
import { choose } from '@/test-select'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  ApiError, createOrgInvite, fetchMe, fetchOrgInvites, fetchOrgMembers, removeOrgMember, revokeOrgInvite, setOrgMemberRole,
} from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { OrgMembers } from '@/components/org-members'
import type { Me, OrgInvite, OrgMember } from '@/types'

const router = vi.hoisted(() => ({ replace: vi.fn() }))

vi.mock('next/navigation', () => ({ useRouter: () => router }))
vi.mock('@/lib/api', async importActual => ({
  ApiError: (await importActual<typeof import('@/lib/api')>()).ApiError,
  createOrgInvite: vi.fn(), fetchMe: vi.fn(), fetchOrgInvites: vi.fn(), fetchOrgMembers: vi.fn(),
  removeOrgMember: vi.fn(), revokeOrgInvite: vi.fn(), setOrgMemberRole: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const expires = '2026-01-08T00:00:00Z'
const TOKEN = 'hli_the-secret-shown-once'
const LAST_OWNER = 'an org keeps at least one owner: make someone else an owner first'
const owner: Me = {
  kind: 'user',
  user_id: 'u1',
  name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const member: Me = { ...owner, user_id: 'u2', name: 'Bob', orgs: [{ ...owner.orgs[0], role: 'member' }] }

let members: OrgMember[]
let invites: OrgInvite[]

function show(as: Me = owner) {
  const resolved = resolveTenant(as, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><OrgMembers /></TenantContext.Provider>
    </SWRConfig>,
  )
}

async function row(id: string) {
  const name = id.startsWith('inv_') ? 'Pending invites' : 'Members'
  const cell = await waitFor(() => within(screen.getByRole('table', { name })).getByText(id))
  return within(cell.closest('tr')!)
}

function confirm(label: string) {
  return userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: label }))
}

beforeEach(() => {
  router.replace.mockReset()
  members = [
    { org_id: 'org_a', user_id: 'u1', name: 'Ada Lovelace', role: 'owner', joined_at: at },
    { org_id: 'org_a', user_id: 'u2', name: 'Bob Stone', role: 'member', joined_at: at },
  ]
  invites = [{ id: 'inv_1', org_id: 'org_a', role: 'member', created_by: 'u1', created_by_name: 'Ada Lovelace', created_at: at, expires_at: expires }]
  vi.mocked(fetchMe).mockResolvedValue(owner)
  vi.mocked(fetchOrgMembers).mockImplementation(async () => ({ items: [...members], total: members.length }))
  vi.mocked(fetchOrgInvites).mockImplementation(async () => ({ items: [...invites], total: invites.length }))
  vi.mocked(createOrgInvite).mockImplementation(async (orgID, role) => {
    const invite: OrgInvite = { id: 'inv_2', org_id: orgID, role, created_by: 'u1', created_by_name: 'Ada Lovelace', created_at: at, expires_at: expires }
    invites = [invite, ...invites]
    return { ...invite, token: TOKEN }
  })
  vi.mocked(revokeOrgInvite).mockImplementation(async id => { invites = invites.filter(i => i.id !== id) })
  vi.mocked(setOrgMemberRole).mockImplementation(async (_, userID, role) => {
    members = members.map(m => (m.user_id === userID ? { ...m, role } : m))
    return members.find(m => m.user_id === userID)!
  })
  vi.mocked(removeOrgMember).mockImplementation(async (_, userID) => { members = members.filter(m => m.user_id !== userID) })
})

describe('an owner', () => {
  it('sees each member by name with their role, and which one they are', async () => {
    members = [...members, { org_id: 'org_a', user_id: 'u3', name: '', role: 'member', joined_at: at }]
    show()
    const own = await row('Ada Lovelace')
    expect(own.getByText('Ada Lovelace').getAttribute('title')).toBe('u1')
    expect(own.queryByText('u1')).toBeNull()
    // One whose identity provider states no name is shown by id.
    expect((await row('u3')).getByLabelText('Role of u3')).toBeTruthy()
    expect(own.getByText('you')).toBeTruthy()
    expect(own.getByLabelText('Role of Ada Lovelace')).toHaveProperty('textContent', 'owner')
    expect(own.getByRole('button', { name: 'Leave' })).toBeTruthy()
    const other = await row('Bob Stone')
    expect(other.queryByText('you')).toBeNull()
    expect(other.getByLabelText('Role of Bob Stone')).toHaveProperty('textContent', 'member')
    expect(other.getByRole('button', { name: 'Remove' })).toBeTruthy()
  })

  it('changes a role after being asked, and reads the list again', async () => {
    show()
    await choose((await row('Bob Stone')).getByLabelText('Role of Bob Stone'), 'owner')
    expect(setOrgMemberRole).not.toHaveBeenCalled()
    expect(within(screen.getByRole('dialog')).getByText('Make Bob Stone an owner of Lab?')).toBeTruthy()
    await confirm('Make owner')
    expect(setOrgMemberRole).toHaveBeenCalledExactlyOnceWith('org_a', 'u2', 'owner')
    await waitFor(() => expect(screen.getByLabelText('Role of Bob Stone')).toHaveProperty('textContent', 'owner'))
    expect(fetchMe).not.toHaveBeenCalled()
  })

  it('leaves a role as it was when the answer is no', async () => {
    show()
    await choose((await row('Bob Stone')).getByLabelText('Role of Bob Stone'), 'owner')
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(setOrgMemberRole).not.toHaveBeenCalled()
    expect(screen.getByLabelText('Role of Bob Stone')).toHaveProperty('textContent', 'member')
  })

  it('is told in the API\'s words that the last owner cannot be demoted', async () => {
    vi.mocked(setOrgMemberRole).mockRejectedValue(new ApiError(409, LAST_OWNER, 'last_owner'))
    show()
    await choose((await row('Ada Lovelace')).getByLabelText('Role of Ada Lovelace'), 'member')
    expect(within(screen.getByRole('dialog')).getByText('Make yourself a member of Lab?')).toBeTruthy()
    await confirm('Make member')
    expect(await screen.findByText(LAST_OWNER)).toBeTruthy()
    expect(screen.getByLabelText('Role of Ada Lovelace')).toHaveProperty('textContent', 'owner')
    expect(fetchMe).not.toHaveBeenCalled()
  })

  it('reads the account again after demoting themselves, which is where the page takes their role from', async () => {
    members = [...members, { org_id: 'org_a', user_id: 'u3', name: '', role: 'owner', joined_at: at }]
    show()
    await choose((await row('Ada Lovelace')).getByLabelText('Role of Ada Lovelace'), 'member')
    await confirm('Make member')
    await waitFor(() => expect(fetchMe).toHaveBeenCalledTimes(1))
    expect(setOrgMemberRole).toHaveBeenCalledExactlyOnceWith('org_a', 'u1', 'member')
  })

  it('removes a member after being asked', async () => {
    show()
    await userEvent.click((await row('Bob Stone')).getByRole('button', { name: 'Remove' }))
    expect(within(screen.getByRole('dialog')).getByText('Remove Bob Stone from Lab?')).toBeTruthy()
    await confirm('Remove')
    expect(removeOrgMember).toHaveBeenCalledExactlyOnceWith('org_a', 'u2')
    await waitFor(() => expect(screen.queryByLabelText('Role of Bob Stone')).toBeNull())
    expect(router.replace).not.toHaveBeenCalled()
  })

  it('is told in the API\'s words that the last owner cannot leave, and stays', async () => {
    vi.mocked(removeOrgMember).mockRejectedValue(new ApiError(409, LAST_OWNER, 'last_owner'))
    show()
    await userEvent.click((await row('Ada Lovelace')).getByRole('button', { name: 'Leave' }))
    await confirm('Leave')
    expect(await screen.findByText(LAST_OWNER)).toBeTruthy()
    expect(router.replace).not.toHaveBeenCalled()
    expect(fetchMe).not.toHaveBeenCalled()
  })
})

describe('inviting', () => {
  it('shows the link once, on this origin with the token in the fragment', async () => {
    show()
    await screen.findByText('inv_1')
    await userEvent.click(screen.getByRole('button', { name: 'Create invite link' }))
    expect(createOrgInvite).toHaveBeenCalledExactlyOnceWith('org_a', 'member')
    const link = `${window.location.origin}/invite#${TOKEN}`
    expect(await screen.findByTestId('secret')).toHaveProperty('textContent', link)
    expect(document.body.textContent!.split(TOKEN)).toHaveLength(2)
    expect(screen.getByText(/joins Lab as a member/)).toBeTruthy()
    expect(screen.getByText(/only time it is shown/)).toBeTruthy()
  })

  it('invites with the role that was picked', async () => {
    show()
    await choose(await screen.findByLabelText('Role of the invited person'), 'Invite as owner')
    await userEvent.click(screen.getByRole('button', { name: 'Create invite link' }))
    expect(createOrgInvite).toHaveBeenCalledExactlyOnceWith('org_a', 'owner')
    expect(await screen.findByText(/joins Lab as an owner/)).toBeTruthy()
  })

  it('copies the link to the clipboard', async () => {
    const user = userEvent.setup()
    show()
    await user.click(await screen.findByRole('button', { name: 'Create invite link' }))
    await user.click(await screen.findByRole('button', { name: 'Copy' }))
    expect(await navigator.clipboard.readText()).toBe(`${window.location.origin}/invite#${TOKEN}`)
  })

  it('lists the new invite without its token, and drops the link for good once dismissed', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Create invite link' }))
    const listed = await row('inv_2')
    expect(listed.queryByText(new RegExp(TOKEN))).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'I have sent it' }))
    expect(screen.queryByTestId('secret')).toBeNull()
    expect(document.body.textContent).not.toContain(TOKEN)
  })

  it('cannot bring the link back: leaving and returning reads the list, which holds no token', async () => {
    const first = show()
    await userEvent.click(await screen.findByRole('button', { name: 'Create invite link' }))
    await screen.findByTestId('secret')
    first.unmount()

    show()
    await screen.findByText('inv_2')
    expect(screen.queryByTestId('secret')).toBeNull()
    expect(document.body.textContent).not.toContain(TOKEN)
  })

  it('shows the API\'s refusal and no link', async () => {
    vi.mocked(createOrgInvite).mockRejectedValue(new ApiError(403, 'forbidden: only an owner of the org may change it'))
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Create invite link' }))
    expect(await screen.findByText('forbidden: only an owner of the org may change it')).toBeTruthy()
    expect(screen.queryByTestId('secret')).toBeNull()
  })

  it('lists a pending invite with its role, who made it and when it expires', async () => {
    show()
    const pending = await row('inv_1')
    expect(pending.getByText('member')).toBeTruthy()
    expect(pending.getByText('Ada Lovelace')).toBeTruthy()
    expect(pending.getByText('you')).toBeTruthy()
    expect(pending.getByText(new Date(expires).toLocaleString())).toBeTruthy()
  })

  it('revokes an invite after being asked, and does nothing when the answer is no', async () => {
    show()
    await userEvent.click((await row('inv_1')).getByRole('button', { name: 'Revoke' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    expect(revokeOrgInvite).not.toHaveBeenCalled()

    await userEvent.click((await row('inv_1')).getByRole('button', { name: 'Revoke' }))
    expect(within(screen.getByRole('dialog')).getByText('Revoke invite inv_1?')).toBeTruthy()
    await confirm('Revoke')
    expect(revokeOrgInvite).toHaveBeenCalledExactlyOnceWith('inv_1')
    expect(await screen.findByText('No pending invites.')).toBeTruthy()
  })

  it('takes the link of an invite off the screen when that invite is revoked', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Create invite link' }))
    await userEvent.click((await row('inv_2')).getByRole('button', { name: 'Revoke' }))
    await confirm('Revoke')
    await waitFor(() => expect(screen.queryByTestId('secret')).toBeNull())
  })

  it('shows why a revoke failed', async () => {
    vi.mocked(revokeOrgInvite).mockRejectedValue(new ApiError(409, 'the invite was already accepted', 'invite_used'))
    show()
    await userEvent.click((await row('inv_1')).getByRole('button', { name: 'Revoke' }))
    await confirm('Revoke')
    expect(await screen.findByText('the invite was already accepted')).toBeTruthy()
  })
})

describe('a member', () => {
  it('reads the members and has nothing to change about anyone else', async () => {
    show(member)
    const other = await row('Ada Lovelace')
    expect(other.getByText('owner')).toBeTruthy()
    expect(other.queryByRole('button')).toBeNull()
    expect(screen.queryByRole('combobox')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Remove' })).toBeNull()
    expect(screen.getByText(/Only an owner can invite people, change a role or remove someone/)).toBeTruthy()
  })

  it('is offered no invite and asks the API for none', async () => {
    show(member)
    await row('Bob Stone')
    expect(screen.queryByRole('button', { name: 'Create invite link' })).toBeNull()
    expect(screen.queryByText('Invites')).toBeNull()
    expect(fetchOrgInvites).not.toHaveBeenCalled()
  })

  it('leaves after being asked, and is taken out of the org\'s pages', async () => {
    vi.mocked(fetchMe).mockResolvedValue({ ...member, orgs: [], projects: [] })
    show(member)
    await userEvent.click((await row('Bob Stone')).getByRole('button', { name: 'Leave' }))
    expect(within(screen.getByRole('dialog')).getByText('Leave Lab?')).toBeTruthy()
    expect(removeOrgMember).not.toHaveBeenCalled()
    await confirm('Leave')
    expect(removeOrgMember).toHaveBeenCalledExactlyOnceWith('org_a', 'u2')
    await waitFor(() => expect(fetchMe).toHaveBeenCalledTimes(1))
    expect(router.replace).toHaveBeenCalledExactlyOnceWith('/console')
  })
})

describe('the member list', () => {
  it('says so when it cannot be loaded', async () => {
    vi.mocked(fetchOrgMembers).mockRejectedValue(new Error('internal error'))
    show()
    expect(await screen.findByText('Could not load members: internal error')).toBeTruthy()
  })

  it('says when the org has more members than one read returns', async () => {
    vi.mocked(fetchOrgMembers).mockImplementation(async () => ({ items: [...members], total: 250 }))
    show()
    expect(await screen.findByText('Showing the first 2 of 250 members.')).toBeTruthy()
  })
})
