import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchExperimentStats, fetchHypothesisStats, fetchPlatformExperimentQuotas, fetchPlatformExperimentsPage } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import ExamplesPage from '@/app/(app)/examples/page'
import { PlatformExperimentStatus } from '@/types'
import type { Me, PlatformExperiment } from '@/types'

const navigation = vi.hoisted(() => ({ push: vi.fn() }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  usePathname: () => '/examples',
  useSearchParams: () => new URLSearchParams('project=prj_1'),
}))
vi.mock('@/lib/api', () => ({
  fetchPlatformExperimentsPage: vi.fn(), fetchPlatformExperimentQuotas: vi.fn(),
  fetchExperimentStats: vi.fn(), fetchHypothesisStats: vi.fn(),
  HYPOTHESIS_STATUSES: ['open', 'confirmed', 'refuted', 'inconclusive'],
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const staff: Me = {
  kind: 'staff',
  orgs: [...me.orgs, { id: 'org_p', name: 'Platform', created_at: at }],
  projects: [...me.projects, { id: 'prj_platform', org_id: 'org_p', name: 'default', created_at: at }],
}

function example(id: string, name: string): PlatformExperiment {
  return {
    id, name, project_id: 'prj_platform', status: PlatformExperimentStatus.CLOSED, shared: true,
    budget_accelerator_hours: 100, max_agents: 4, signup_count: 3, stages: [], current_stage: 1, created_at: at,
  }
}

function show(who: Me = me) {
  const resolved = resolveTenant(who, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><ExamplesPage /></TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  vi.mocked(fetchPlatformExperimentsPage).mockResolvedValue({ items: [example('pe-x', 'Vision benchmark'), example('pe-y', 'Tabular search')], total: 2 })
  vi.mocked(fetchPlatformExperimentQuotas).mockResolvedValue([])
  vi.mocked(fetchHypothesisStats).mockResolvedValue({ total: 0, by_status: {} })
  vi.mocked(fetchExperimentStats).mockResolvedValue({ total: 0, by_status: {} } as never)
})

describe('the examples', () => {
  it('are the shared platform experiments, asked for with no project', async () => {
    show()
    expect(await screen.findByText('Vision benchmark')).toBeTruthy()
    expect(screen.getByText('Tabular search')).toBeTruthy()
    expect(fetchPlatformExperimentsPage).toHaveBeenCalledWith({ shared: true, sort: '-starts_at', limit: 10, offset: 0 })
  })

  it('read their figures inside the example\'s own project, never the caller\'s', async () => {
    show()
    await screen.findByText('Vision benchmark')
    expect(fetchExperimentStats).toHaveBeenCalledWith({ project_id: 'prj_platform', platform_experiment_id: 'pe-x', group_by: 'hypothesis' })
  })

  it('open as examples, in the caller\'s own project, and offer no edit', async () => {
    show()
    await userEvent.click(await screen.findByText('Vision benchmark'))
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/platform-experiments/pe-x?example=pe-x&project=prj_1')
    expect(screen.queryByRole('button', { name: 'Edit' })).toBeNull()
  })

  it('open for staff, who reach them, as any platform experiment and with no control added', async () => {
    show(staff)
    await userEvent.click(await screen.findByText('Vision benchmark'))
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/platform-experiments/pe-x?project=prj_1')
    expect(screen.queryAllByRole('button')).toEqual([])
  })

  it('say so when there are none', async () => {
    vi.mocked(fetchPlatformExperimentsPage).mockResolvedValue({ items: [], total: 0 })
    show()
    expect(await screen.findByText(/There are no examples yet/)).toBeTruthy()
  })

  it('say so when they cannot be read', async () => {
    vi.mocked(fetchPlatformExperimentsPage).mockRejectedValue(new Error('Failed to fetch'))
    show()
    expect(await screen.findByText(/Could not load the examples: Failed to fetch/)).toBeTruthy()
  })

  it('page through more than one page of them', async () => {
    vi.mocked(fetchPlatformExperimentsPage).mockResolvedValue({ items: [example('pe-x', 'Vision benchmark')], total: 11 })
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Next ›' }))
    expect(fetchPlatformExperimentsPage).toHaveBeenLastCalledWith({ shared: true, sort: '-starts_at', limit: 10, offset: 10 })
  })
})
