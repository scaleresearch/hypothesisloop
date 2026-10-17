import { render, screen, waitFor, within } from '@testing-library/react'
import { choose } from '@/test-select'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createOrgInvite, fetchMe, fetchOrgInvites, fetchOrgMembers, removeOrgMember, revokeOrgInvite, setOrgMemberRole } from '@/lib/api/accounts'
import { ApiError } from '@/lib/api/request'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { OrgMembers } from '@/components/org-members'
import type { ListedMember, Me, OrgInvite, OrgMember } from '@/types/api'

const router = vi.hoisted(() => ({ replace: vi.fn() }))

vi.mock('next/navigation', () => ({ useRouter: () => router }))
vi.mock('@/lib/api/accounts', () => ({
  createOrgInvite: vi.fn(), fetchMe: vi.fn(), fetchOrgInvites: vi.fn(), fetchOrgMembers: vi.fn(), removeOrgMember: vi.fn(), revokeOrgInvite: vi.fn(), setOrgMemberRole: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const expires = '2026-01-08T00:00:00Z'
const TOKEN = 'hli_the-secret-shown-once'
const LAST_OWNER = 'an org keeps at least one owner: make someone else an owner first'
const owner: Me = {
  kind: 'user',
  user_id: 'u1',
  name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'owner', created_at: at, actions: ['edit', 'create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const member: Me = { ...owner, user_id: 'u2', name: 'Bob', orgs: [{ ...owner.orgs[0], role: 'member', actions: ['create_api_key'] }] }

let members: OrgMember[]
let invites: OrgInvite[]

// What the API lets the one reading the list do to m: an owner changes anyone's role and removes
// anyone, a person leaves, and the org's last owner stays.
let viewer: Me
function withActions(m: OrgMember): ListedMember {
  const lastOwner = m.role === 'owner' && members.filter(o => o.role === 'owner').length === 1
  const edits = viewer.orgs[0].actions.includes('edit')
  if (lastOwner) return { ...m, actions: [] }
  return { ...m, actions: [...(edits ? ['set_role' as const] : []), ...(edits || m.user_id === viewer.user_id ? ['remove' as const] : [])] }
}

function show(as: Me = owner) {
  viewer = as
  const resolved = resolveTenant(as, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><OrgMembers /></TenantContext.Provider>
    </SWRConfig>,
  )
}

async function row(id: string) {
  // An invite has no name of its own on screen: the one pending invite is the only row of its table.
  if (id.startsWith('inv_')) {
    return within(await waitFor(() => within(screen.getByRole('table', { name: 'Pending invites' })).getAllByRole('row')[1]))
  }
  const cell = await waitFor(() => within(screen.getByRole('table', { name: 'Members' })).getByText(id))
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
  invites = [{ id: 'inv_1', org_id: 'org_a', role: 'member', actor: { kind: 'user', id: 'u1', name: 'Ada Lovelace' }, created_at: at, expires_at: expires }]
  vi.mocked(fetchMe).mockResolvedValue(owner)
  vi.mocked(fetchOrgMembers).mockImplementation(async () => ({ items: members.map(withActions), total: members.length }))
  vi.mocked(fetchOrgInvites).mockImplementation(async () => ({ items: [...invites], total: invites.length }))
  vi.mocked(createOrgInvite).mockImplementation(async (orgID, role) => {
    const invite: OrgInvite = { id: 'inv_2', org_id: orgID, role, actor: { kind: 'user', id: 'u1', name: 'Ada Lovelace' }, created_at: at, expires_at: expires }
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

  it('is offered no role to change for the last owner', async () => {
    show()
    expect((await row('Ada Lovelace')).queryByLabelText('Role of Ada Lovelace')).toBeNull()
    expect((await row('Bob Stone')).getByLabelText('Role of Bob Stone')).toBeTruthy()
  })

  it('is told in the API\'s words that the last owner cannot be demoted, when the other owner left since the list was read', async () => {
    members = [...members, { org_id: 'org_a', user_id: 'u3', name: 'Cy', role: 'owner', joined_at: at }]
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

  it('cannot leave while the only owner, and is told what to do first', async () => {
    show()
    const own = await row('Ada Lovelace')
    expect(own.getByText('Make someone else an owner before you leave')).toBeTruthy()
    expect(own.getByRole('button', { name: 'Leave' })).toHaveProperty('disabled', true)
  })

  it('can leave when another owner remains', async () => {
    members = [...members, { org_id: 'org_a', user_id: 'u3', name: 'Cy', role: 'owner', joined_at: at }]
    show()
    const own = await row('Ada Lovelace')
    expect(own.queryByText('Make someone else an owner before you leave')).toBeNull()
    expect(own.getByRole('button', { name: 'Leave' })).toHaveProperty('disabled', false)
  })

  it('is told in the API\'s words when the other owner has left meanwhile, and stays', async () => {
    members = [...members, { org_id: 'org_a', user_id: 'u3', name: 'Cy', role: 'owner', joined_at: at }]
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
    await row('inv_1')
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
    await waitFor(() => expect(within(screen.getByRole('table', { name: 'Pending invites' })).getAllByRole('row')).toHaveLength(3))
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

  it('revokes an invite after being asked, and does nothing when the answer is no', async () => {
    show()
    await userEvent.click((await row('inv_1')).getByRole('button', { name: 'Revoke' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    expect(revokeOrgInvite).not.toHaveBeenCalled()

    await userEvent.click((await row('inv_1')).getByRole('button', { name: 'Revoke' }))
    expect(within(screen.getByRole('dialog')).getByText(`Revoke the invite Ada Lovelace created ${new Date(at).toLocaleString()}?`)).toBeTruthy()
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
    vi.mocked(fetchOrgMembers).mockImplementation(async () => ({ items: members.map(withActions), total: 250 }))
    show()
    expect(await screen.findByText('Showing the first 2 of 250 members.')).toBeTruthy()
  })
})
