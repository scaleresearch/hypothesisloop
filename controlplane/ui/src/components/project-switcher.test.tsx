import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { ProjectSwitcher } from '@/components/project-switcher'
import { Link } from '@/components/ui/link'
import type { Me } from '@/types'

const navigation = vi.hoisted(() => ({ push: vi.fn(), pathname: '/jobs', search: '' }))

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: navigation.push }),
  usePathname: () => navigation.pathname,
  useSearchParams: () => new URLSearchParams(navigation.search),
}))

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

function show(projectID: string, children = <ProjectSwitcher />) {
  navigation.search = projectID ? `project=${projectID}` : ''
  const resolved = resolveTenant(me, projectID)
  if (resolved.state !== 'selected') throw new Error(`no tenant for ${projectID}`)
  return render(<TenantContext.Provider value={resolved.tenant}>{children}</TenantContext.Provider>)
}

function field(label: string) {
  return screen.getByText(label).parentElement!.querySelector('input')!
}

beforeEach(() => {
  navigation.pathname = '/jobs'
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

  it('puts a chosen project in the URL of the current page', async () => {
    show('prj_a1')
    await userEvent.click(field('Project'))
    await userEvent.click(screen.getByRole('option', { name: 'vision' }))
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/jobs?project=prj_a2')
  })

  it('lands a chosen org on its first project', async () => {
    show('prj_a2')
    await userEvent.click(field('Organization'))
    await userEvent.click(screen.getByRole('option', { name: 'Lab' }))
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/jobs?project=prj_b1')
  })

  it('leaves a record of the old project for the list it belongs to, with its filters dropped', async () => {
    navigation.pathname = '/platform-experiments/pe-1'
    show('prj_a1')
    await userEvent.click(field('Project'))
    await userEvent.click(screen.getByRole('option', { name: 'vision' }))
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/platform-experiments?project=prj_a2')
  })

  it('stays on the home page when switching there', async () => {
    navigation.pathname = '/'
    show('prj_a1')
    await userEvent.click(field('Project'))
    await userEvent.click(screen.getByRole('option', { name: 'vision' }))
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/?project=prj_a2')
  })

  it('does nothing when the selected project is chosen again', async () => {
    show('prj_a1')
    await userEvent.click(field('Project'))
    await userEvent.click(screen.getByRole('option', { name: 'default' }))
    expect(navigation.push).not.toHaveBeenCalled()
  })
})

describe('in-app links', () => {
  it('carry the selected project, beside the link\'s own query', () => {
    show('prj_b1', <Link href="/jobs?status=RUNNING">jobs</Link>)
    expect(screen.getByRole('link', { name: 'jobs' }).getAttribute('href')).toBe('/jobs?status=RUNNING&project=prj_b1')
  })

  it('are left alone when the URL names no project', () => {
    show('', <Link href="/jobs/j1">job</Link>)
    expect(screen.getByRole('link', { name: 'job' }).getAttribute('href')).toBe('/jobs/j1')
  })
})
