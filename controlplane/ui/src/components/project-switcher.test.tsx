import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createOrg, createProject, fetchMe } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { ProjectSwitcher } from '@/components/project-switcher'
import type { Me } from '@/types'

const navigation = vi.hoisted(() => ({ push: vi.fn(), pathname: '/projects/prj_a1/jobs' }))

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: navigation.push }),
  usePathname: () => navigation.pathname,
}))

vi.mock('@/lib/api', () => ({ createOrg: vi.fn(), createProject: vi.fn(), fetchMe: vi.fn() }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  name: 'Ada',
  orgs: [
    { id: 'org_a', name: 'Ada', role: 'owner', created_at: at },
    { id: 'org_b', name: 'Lab', role: 'member', created_at: at },
    { id: 'org_empty', name: 'No projects', role: 'member', created_at: at },
  ],
  projects: [
    { id: 'prj_a1', org_id: 'org_a', name: 'default', created_at: at },
    { id: 'prj_a2', org_id: 'org_a', name: 'vision', created_at: at },
    { id: 'prj_b1', org_id: 'org_b', name: 'shared', created_at: at },
    { id: 'prj_b2', org_id: 'org_b', name: 'second', created_at: at },
  ],
}

function show(projectID: string, children = <ProjectSwitcher />, as: Me = me) {
  const resolved = resolveTenant(as, projectID)
  if (resolved.state !== 'selected') throw new Error(`no tenant for ${projectID}`)
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}>{children}</TenantContext.Provider>
    </SWRConfig>,
  )
}

async function createByName(what: string, name: string) {
  await userEvent.click(screen.getByRole('button', { name: `+ New ${what}` }))
  await userEvent.type(screen.getByLabelText(`New ${what} name`), name)
  await userEvent.click(screen.getByRole('button', { name: 'Create' }))
}

function field(label: string) {
  return screen.getByText(label).parentElement!.querySelector('input')!
}

beforeEach(() => {
  navigation.pathname = '/projects/prj_a1/jobs'
})

describe('the org and project switcher', () => {
  it('shows the home org and project when the URL names none', () => {
    show('')
    expect(field('Organization').value).toBe('Ada')
    expect(field('Project').value).toBe('default')
  })

  it('shows the project the URL names and the org it belongs to', () => {
    show('prj_b2')
    expect(field('Organization').value).toBe('Lab')
    expect(field('Project').value).toBe('second')
  })

  it('lists the selected org\'s projects only', async () => {
    show('prj_a1')
    await userEvent.click(field('Project'))
    expect(screen.getAllByRole('option').map(o => o.textContent)).toEqual(['default', 'vision'])
  })

  it('lists every org the caller reaches a project of', async () => {
    show('prj_a1')
    await userEvent.click(field('Organization'))
    expect(screen.getAllByRole('option').map(o => o.textContent)).toEqual(['Ada', 'Lab'])
  })

  it('opens the same section of a chosen project', async () => {
    show('prj_a1')
    await userEvent.click(field('Project'))
    await userEvent.click(screen.getByRole('option', { name: 'vision' }))
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/projects/prj_a2/jobs')
  })

  it('lands a chosen org on its first project', async () => {
    show('prj_a2')
    await userEvent.click(field('Organization'))
    await userEvent.click(screen.getByRole('option', { name: 'Lab' }))
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/projects/prj_b1/jobs')
  })

  it('keeps a page of the org a page of the org', async () => {
    navigation.pathname = '/orgs/org_a/settings'
    show('prj_a1')
    await userEvent.click(field('Organization'))
    await userEvent.click(screen.getByRole('option', { name: 'Lab' }))
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/orgs/org_b/settings')
  })

  it('leaves a record of the old project for the list it belongs to, with its filters dropped', async () => {
    for (const [pathname, list] of [['/experiments/pe-1', 'experiments'], ['/experiments/pe-1/jobs/j1', 'jobs'], ['/experiments/pe-1/hypotheses/h1', 'hypotheses']]) {
      navigation.pathname = pathname
      navigation.push.mockClear()
      const view = show('prj_a1')
      await userEvent.click(field('Project'))
      await userEvent.click(screen.getByRole('option', { name: 'vision' }))
      expect(navigation.push).toHaveBeenCalledExactlyOnceWith(`/projects/prj_a2/${list}`)
      view.unmount()
    }
  })

  it('lands on the experiments from a page of no section', async () => {
    navigation.pathname = '/console'
    show('prj_a1')
    await userEvent.click(field('Project'))
    await userEvent.click(screen.getByRole('option', { name: 'vision' }))
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/projects/prj_a2/experiments')
  })

  it('does nothing when the selected project is chosen again', async () => {
    show('prj_a1')
    await userEvent.click(field('Project'))
    await userEvent.click(screen.getByRole('option', { name: 'default' }))
    expect(navigation.push).not.toHaveBeenCalled()
  })
})

describe('creating an organization', () => {
  const created = { id: 'org_new', name: 'Robotics', role: 'owner' as const, created_at: at }
  const after: Me = {
    ...me,
    orgs: [...me.orgs, created],
    projects: [...me.projects, { id: 'prj_new', org_id: 'org_new', name: 'default', created_at: at }],
  }

  beforeEach(() => {
    vi.mocked(createOrg).mockResolvedValue(created)
    vi.mocked(fetchMe).mockResolvedValue(after)
  })

  it('creates it and switches to its default project', async () => {
    show('prj_a1')
    await createByName('organization', '  Robotics ')
    expect(createOrg).toHaveBeenCalledExactlyOnceWith('Robotics')
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/projects/prj_new/jobs')
    expect(screen.queryByLabelText('New organization name')).toBeNull()
  })

  it('is open to a member, who owns what they create', async () => {
    show('prj_b1')
    expect(screen.getByRole('button', { name: '+ New organization' })).toBeTruthy()
  })

  it('shows the API\'s refusal, keeps what was typed and switches nowhere', async () => {
    vi.mocked(createOrg).mockRejectedValue(new Error('org name is longer than 100 bytes'))
    show('prj_a1')
    await createByName('organization', 'Robotics')
    expect(await screen.findByText('org name is longer than 100 bytes')).toBeTruthy()
    expect(screen.getByLabelText('New organization name')).toHaveProperty('value', 'Robotics')
    expect(navigation.push).not.toHaveBeenCalled()
    expect(fetchMe).not.toHaveBeenCalled()
  })

  it('says so when the org was created but reading the account again fails', async () => {
    vi.mocked(fetchMe).mockRejectedValue(new Error('Failed to fetch'))
    show('prj_a1')
    await createByName('organization', 'Robotics')
    expect(await screen.findByText('Failed to fetch')).toBeTruthy()
    expect(navigation.push).not.toHaveBeenCalled()
  })

  it('needs a name, and can be cancelled', async () => {
    show('prj_a1')
    await userEvent.click(screen.getByRole('button', { name: '+ New organization' }))
    expect(screen.getByRole('button', { name: 'Create' })).toHaveProperty('disabled', true)
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(screen.getByRole('button', { name: '+ New organization' })).toBeTruthy()
    expect(createOrg).not.toHaveBeenCalled()
  })
})

describe('creating a project', () => {
  const created = { id: 'prj_a3', org_id: 'org_a', name: 'speech', created_at: at }

  beforeEach(() => {
    vi.mocked(createProject).mockResolvedValue(created)
    vi.mocked(fetchMe).mockResolvedValue({ ...me, projects: [...me.projects, created] })
  })

  it('creates it in the selected org and switches to it', async () => {
    show('prj_a1')
    await createByName('project', 'speech')
    expect(createProject).toHaveBeenCalledExactlyOnceWith('org_a', 'speech')
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/projects/prj_a3/jobs')
  })

  it('shows a name that is taken as the API says it, and switches nowhere', async () => {
    vi.mocked(createProject).mockRejectedValue(new Error('project name default is already taken in this org'))
    show('prj_a1')
    await createByName('project', 'default')
    expect(await screen.findByText('project name default is already taken in this org')).toBeTruthy()
    expect(navigation.push).not.toHaveBeenCalled()
  })

  it('is not offered to a member, who may not change the org', () => {
    show('prj_b1')
    expect(screen.queryByRole('button', { name: '+ New project' })).toBeNull()
  })
})

describe('two orgs with the same name', () => {
  it('are told apart by the end of their ids', async () => {
    const twins: Me = {
      ...me,
      orgs: [...me.orgs, { id: 'org_01aaaa-zzz999', name: 'Lab', role: 'owner', created_at: at }],
      projects: [...me.projects, { id: 'prj_t', org_id: 'org_01aaaa-zzz999', name: 'default', created_at: at }],
    }
    show('prj_a1', <ProjectSwitcher />, twins)
    await userEvent.click(field('Organization'))
    expect(screen.getAllByRole('option').map(o => o.textContent)).toEqual(['Ada', 'Lab (org_b)', 'Lab (zzz999)'])
  })
})
