import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createProject, fetchMe, renameOrg, renameProject } from '@/lib/api/accounts'
import { resolveOrgTenant, resolveTenant, TenantContext } from '@/lib/tenant'
import SettingsPage from '@/app/(app)/orgs/[orgId]/settings/page'
import type { Me } from '@/types/api'

vi.mock('@/lib/api/accounts', () => ({ createProject: vi.fn(), fetchMe: vi.fn(), renameOrg: vi.fn(), renameProject: vi.fn() }))
vi.mock('@/components/api-keys', () => ({ APIKeys: () => <p>api keys</p> }))
vi.mock('@/components/org-members', () => ({ OrgMembers: () => <p>members</p> }))
vi.mock('@/components/org-github', () => ({ OrgGitHub: () => <p>github accounts</p> }))

const at = '2026-01-01T00:00:00Z'
const owner: Me = {
  kind: 'user',
  user_id: 'u1',
  name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at, actions: ['edit', 'create_api_key'] }],
  projects: [
    { id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at },
    { id: 'prj_2', org_id: 'org_a', name: 'vision', created_at: at },
  ],
}

function show(as: Me = owner, orgID = '') {
  const resolved = orgID ? resolveOrgTenant(as, orgID) : resolveTenant(as, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><SettingsPage /></TenantContext.Provider>
    </SWRConfig>,
  )
}

function form(label: string) {
  const input = screen.getByLabelText(label) as HTMLInputElement
  return { input, button: input.closest('form')!.querySelector('button')! }
}

async function rename(label: string, to: string) {
  const { input, button } = form(label)
  await userEvent.clear(input)
  await userEvent.type(input, to)
  await userEvent.click(button)
}

beforeEach(() => {
  vi.mocked(fetchMe).mockResolvedValue(owner)
  vi.mocked(renameOrg).mockResolvedValue({ ...owner.orgs[0], name: 'Ada Lab' })
  vi.mocked(renameProject).mockResolvedValue({ ...owner.projects[1], name: 'speech' })
  vi.mocked(createProject).mockResolvedValue({ id: 'prj_3', org_id: 'org_a', name: 'speech', created_at: at })
})

describe('renaming', () => {
  it('renames the org and reads the account again, which is where every page takes names from', async () => {
    show()
    await rename('Organization name', 'Ada Lab')
    expect(renameOrg).toHaveBeenCalledExactlyOnceWith('org_a', 'Ada Lab')
    expect(fetchMe).toHaveBeenCalledTimes(1)
  })

  it('renames a project', async () => {
    show()
    await rename('Name of project vision', 'speech')
    expect(renameProject).toHaveBeenCalledExactlyOnceWith('prj_2', 'speech')
    expect(fetchMe).toHaveBeenCalledTimes(1)
  })

  it('shows a project name that is taken as the API says it, and keeps what was typed', async () => {
    vi.mocked(renameProject).mockRejectedValue(new Error('project name default is already taken in this org'))
    show()
    await rename('Name of project vision', 'default')
    expect(await screen.findByText('project name default is already taken in this org')).toBeTruthy()
    expect(form('Name of project vision').input.value).toBe('default')
    expect(fetchMe).not.toHaveBeenCalled()
  })

  it('shows why an org rename failed when the API is down', async () => {
    vi.mocked(renameOrg).mockRejectedValue(new Error('Failed to fetch'))
    show()
    await rename('Organization name', 'Ada Lab')
    expect(await screen.findByText('Failed to fetch')).toBeTruthy()
    expect(form('Organization name').input.value).toBe('Ada Lab')
  })

  it('has nothing to save while the name is unchanged or empty', async () => {
    show()
    expect(form('Organization name').button).toHaveProperty('disabled', true)
    await userEvent.clear(form('Organization name').input)
    expect(form('Organization name').button).toHaveProperty('disabled', true)
  })
})

describe('creating a project', () => {
  it('creates it in the org and clears the field', async () => {
    show()
    await rename('New project name', 'speech')
    expect(createProject).toHaveBeenCalledExactlyOnceWith('org_a', 'speech')
    expect(form('New project name').input.value).toBe('')
  })

  it('shows a taken name as the API says it', async () => {
    vi.mocked(createProject).mockRejectedValue(new Error('project name vision is already taken in this org'))
    show()
    await rename('New project name', 'vision')
    expect(await screen.findByText('project name vision is already taken in this org')).toBeTruthy()
  })
})

describe('a member of the org', () => {
  it('sees its members and where its clusters are, and that the rest is an owner\'s, with nothing to change', () => {
    show({ ...owner, orgs: [{ ...owner.orgs[0], role: 'member', actions: ['create_api_key'] }] })
    expect(screen.getByText(/Only an owner can rename it or manage its projects, API keys and GitHub accounts/)).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Compute Resources' }).getAttribute('href')).toBe('/orgs/org_a/clusters')
    expect(screen.getByText('members')).toBeTruthy()
    expect(screen.queryByText('api keys')).toBeNull()
    expect(screen.queryByLabelText('Organization name')).toBeNull()
    expect(screen.queryByLabelText('New project name')).toBeNull()
  })
})

describe('the GitHub section', () => {
  it('is a card of the page, which a link can lead to, for an owner and a member alike', () => {
    show()
    expect(screen.getByText('github accounts').closest('#github')).not.toBeNull()
  })
})

describe('your organizations', () => {
  // A person's home org, which they own, and one they were invited into as a member.
  const invited: Me = {
    ...owner,
    orgs: [owner.orgs[0], { id: 'org_lab', name: 'Lab', role: 'member', created_at: at, actions: ['create_api_key'] }],
    projects: [...owner.projects, { id: 'prj_lab', org_id: 'org_lab', name: 'default', created_at: at }],
  }
  const rows = () => within(screen.getByRole('table', { name: 'Your organizations' })).getAllByRole('row').slice(1)

  it('lists every org the person belongs to with their role in it and marks the one these settings are of', () => {
    show(invited, 'org_lab')
    expect(rows().map(r => r.textContent)).toEqual([
      expect.stringMatching(/^Adaorg_aowner2.*Switch$/),
      expect.stringMatching(/^Laborg_labmember1.*Current$/),
    ])
    expect(rows()[1].getAttribute('aria-current')).toBe('true')
    expect(screen.getByText('Settings of Lab, where you are member.')).toBeTruthy()
  })

  it('switches to another org in one click, to the settings of that org', () => {
    show(invited, 'org_lab')
    expect(screen.getByRole('link', { name: 'Switch to Ada' }).getAttribute('href')).toBe('/orgs/org_a/settings')
    expect(screen.queryByRole('link', { name: 'Switch to Lab' })).toBeNull()
  })

  it('tells apart two orgs of one name by the end of their ids', () => {
    show({ ...invited, orgs: [invited.orgs[0], { ...invited.orgs[1], name: 'Ada' }] })
    expect(screen.getByRole('link', { name: 'Switch to Ada (rg_lab)' })).toBeTruthy()
  })

  it('lists for staff only the orgs they belong to', () => {
    show({ ...invited, kind: 'staff', orgs: [invited.orgs[0], { ...invited.orgs[1], role: undefined }] })
    expect(rows()).toHaveLength(1)
    expect(screen.queryByText('org_lab')).toBeNull()
  })
})
