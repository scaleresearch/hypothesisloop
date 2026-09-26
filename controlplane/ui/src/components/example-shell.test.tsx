import { render, screen } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchMe, fetchOrgClusters, fetchPlatformExperiment } from '@/lib/api'
import { useExample, useListScope, useReadOnly } from '@/lib/example'
import { useTenant } from '@/lib/tenant'
import { AppShell } from '@/components/app-shell'
import { PlatformExperimentStatus } from '@/types'
import type { Me, PlatformExperiment } from '@/types'

const navigation = vi.hoisted(() => ({ push: vi.fn(), search: '', pathname: '/jobs/j1' }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  usePathname: () => navigation.pathname,
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', () => ({ fetchMe: vi.fn(), fetchPlatformExperiment: vi.fn(), fetchOrgClusters: vi.fn() }))
vi.mock('@/lib/auth', () => ({ HOME_PATH: '/console', LANDING_PATH: '/', signOut: vi.fn() }))
vi.mock('@/components/command-palette', () => ({ CommandPalette: () => null, CommandPaletteTrigger: () => null }))
vi.mock('@/components/project-switcher', () => ({ ProjectSwitcher: () => null }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

function experiment(more: Partial<PlatformExperiment>): PlatformExperiment {
  return {
    id: 'pe-x', name: 'Vision benchmark', project_id: 'prj_platform', status: PlatformExperimentStatus.CLOSED, shared: true,
    budget_accelerator_hours: 100, max_agents: 4, signup_count: 3, stages: [], current_stage: 1, created_at: at, ...more,
  }
}

function Page() {
  const { project } = useTenant()
  const example = useExample()
  return (
    <p>
      page in {project.name}, {useReadOnly() ? 'read only' : 'writable'}, example {example?.id ?? 'none'}, lists by {JSON.stringify(useListScope())}
    </p>
  )
}

function show(search: string, pathname = '/jobs/j1') {
  navigation.search = search
  navigation.pathname = pathname
  return render(
    <SWRConfig value={{ provider: () => new Map(), shouldRetryOnError: false, dedupingInterval: 0 }}>
      <AppShell><Page /></AppShell>
    </SWRConfig>,
  )
}

function current() {
  return screen.getAllByRole('link').filter(a => a.getAttribute('aria-current') === 'page').map(a => a.textContent)
}

beforeEach(() => {
  vi.mocked(fetchMe).mockResolvedValue(me)
  vi.mocked(fetchOrgClusters).mockResolvedValue([])
  vi.mocked(fetchPlatformExperiment).mockResolvedValue(experiment({}))
})

describe('the shell around an example', () => {
  it('shows it in the caller\'s own project, read only, under one banner', async () => {
    show('example=pe-x')
    expect(await screen.findByText('page in default, read only, example pe-x, lists by {"platform_experiment_id":"pe-x"}')).toBeTruthy()
    expect(fetchPlatformExperiment).toHaveBeenCalledWith('pe-x')
    const banner = await screen.findByText('Vision benchmark')
    expect(banner.closest('[role="note"]')!.textContent).toContain('Example, read only')
    expect(screen.getAllByRole('note')).toHaveLength(1)
  })

  it('is read only before the experiment has been read, and when it cannot be', async () => {
    vi.mocked(fetchPlatformExperiment).mockRejectedValue(new Error('platform experiment not found'))
    show('example=pe-x')
    expect(await screen.findByText(/read only, example pe-x/)).toBeTruthy()
  })

  it('marks Examples in the nav, and its other entries lead out of the example', async () => {
    show('example=pe-x&project=prj_1')
    await screen.findByText(/page in default, read only/)
    expect(current()).toEqual(['Examples'])
    expect(screen.getByRole('link', { name: 'Jobs' }).getAttribute('href')).toBe('/jobs?project=prj_1')
    expect(screen.getByRole('link', { name: 'Hypotheses' }).getAttribute('href')).toBe('/hypotheses?project=prj_1')
    expect(screen.getByRole('link', { name: 'Examples' }).getAttribute('href')).toBe('/examples?project=prj_1')
  })

  it('treats a shared experiment of the caller\'s own as theirs', async () => {
    vi.mocked(fetchPlatformExperiment).mockResolvedValue(experiment({ project_id: 'prj_1' }))
    show('example=pe-x')
    expect(await screen.findByText('page in default, writable, example none, lists by {"project_id":"prj_1"}')).toBeTruthy()
    expect(screen.queryByRole('note')).toBeNull()
    expect(current()).toEqual(['Jobs'])
  })

  it('is no example on a page that shows none, whatever the URL says', async () => {
    show('example=pe-x', '/settings')
    expect(await screen.findByText(/writable, example none/)).toBeTruthy()
    expect(fetchPlatformExperiment).not.toHaveBeenCalled()
    expect(current()).toEqual(['Settings'])
  })

  it('is the plain app with no example named', async () => {
    show('')
    expect(await screen.findByText('page in default, writable, example none, lists by {"project_id":"prj_1"}')).toBeTruthy()
    expect(screen.queryByRole('note')).toBeNull()
    expect(screen.getByRole('link', { name: 'Examples' })).toBeTruthy()
  })
})
