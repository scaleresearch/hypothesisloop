import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createProject, fetchMe, renameOrg, renameProject } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import SettingsPage from '@/app/(app)/settings/page'
import type { Me } from '@/types'

vi.mock('@/lib/api', () => ({ createProject: vi.fn(), fetchMe: vi.fn(), renameOrg: vi.fn(), renameProject: vi.fn() }))
vi.mock('@/components/api-keys', () => ({ APIKeys: () => <p>api keys</p> }))
vi.mock('@/components/org-clusters', () => ({ OrgClusters: () => <p>clusters</p> }))

const at = '2026-01-01T00:00:00Z'
const owner: Me = {
  kind: 'user',
  user_id: 'u1',
  name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [
    { id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at },
    { id: 'prj_2', org_id: 'org_a', name: 'vision', created_at: at },
  ],
}

function show(as: Me = owner) {
  const resolved = resolveTenant(as, '')
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
  it('sees its clusters and that the rest is an owner\'s, with nothing to change', () => {
    show({ ...owner, orgs: [{ ...owner.orgs[0], role: 'member' }] })
    expect(screen.getByText(/Only an owner can rename it or manage its projects, API keys and clusters/)).toBeTruthy()
    expect(screen.getByText('clusters')).toBeTruthy()
    expect(screen.queryByText('api keys')).toBeNull()
    expect(screen.queryByLabelText('Organization name')).toBeNull()
    expect(screen.queryByLabelText('New project name')).toBeNull()
  })
})

describe('the repository section', () => {
  it('is a marked place with nothing to operate', () => {
    show()
    const section = screen.getByText('Repository').closest('.wa-pod')!
    expect(section.textContent).toContain('Not available yet.')
    expect(section.querySelector('button, input, a')).toBeNull()
  })
})
