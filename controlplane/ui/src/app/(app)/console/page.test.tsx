import { render, screen, waitFor } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchAgentsPage, fetchExperimentStats, fetchOrgClusters, fetchPlatformExperimentsPage } from '@/lib/api'
import type { PlatformExperimentsParams } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import Home from '@/app/(app)/console/page'
import { PlatformExperimentStatus } from '@/types'
import type { Me, PlatformExperiment } from '@/types'

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => '/console',
  useSearchParams: () => new URLSearchParams(),
}))
vi.mock('@/lib/api', () => ({
  fetchOrgClusters: vi.fn(), fetchExperimentStats: vi.fn(), fetchAgentsPage: vi.fn(), fetchPlatformExperimentsPage: vi.fn(),
}))
vi.mock('@/components/command-palette', () => ({ CommandPaletteTrigger: () => null }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

function experiment(id: string, name: string, projectID: string): PlatformExperiment {
  return {
    id, name, project_id: projectID, status: PlatformExperimentStatus.CLOSED, shared: projectID !== 'prj_1',
    budget_accelerator_hours: 100, max_agents: 4, signup_count: 3, stages: [], current_stage: 1, created_at: at,
  }
}

// The caller's own experiments and the shared ones are two reads of the same list.
function lists(own: PlatformExperiment[], shared: PlatformExperiment[], sharedTotal = shared.length) {
  vi.mocked(fetchPlatformExperimentsPage).mockImplementation(async (params: PlatformExperimentsParams) => (
    params.shared ? { items: shared, total: sharedTotal } : { items: own, total: own.length }
  ))
}

function show() {
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map(), shouldRetryOnError: false }}>
      <TenantContext.Provider value={resolved.tenant}><Home /></TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  vi.mocked(fetchOrgClusters).mockResolvedValue([])
  vi.mocked(fetchExperimentStats).mockResolvedValue({ total: 0, by_status: {} } as never)
  vi.mocked(fetchAgentsPage).mockResolvedValue({ items: [], total: 0 })
})

describe('the home of an account with no platform experiment', () => {
  it('offers the examples to start from, each opening as an example', async () => {
    lists([], [experiment('pe-x', 'Vision benchmark', 'prj_platform'), experiment('pe-y', 'Tabular search', 'prj_platform')])
    show()
    const section = await screen.findByRole('region', { name: 'Start from an example' })
    expect(section.textContent).toContain('read only')
    expect(screen.getByRole('link', { name: /Vision benchmark/ }).getAttribute('href')).toBe('/platform-experiments/pe-x?example=pe-x')
    expect(screen.getByRole('link', { name: /Tabular search/ }).getAttribute('href')).toBe('/platform-experiments/pe-y?example=pe-y')
    expect(fetchPlatformExperimentsPage).toHaveBeenCalledWith({ shared: true, sort: '-starts_at', limit: 3 })
    expect(screen.queryByRole('link', { name: /All \d+ examples/ })).toBeNull()
  })

  it('leads to the whole list when there are more than it shows', async () => {
    lists([], [experiment('pe-x', 'Vision benchmark', 'prj_platform')], 7)
    show()
    expect((await screen.findByRole('link', { name: 'All 7 examples' })).getAttribute('href')).toBe('/examples')
  })

  it('shows no such section when there are no examples', async () => {
    lists([], [])
    show()
    expect(await screen.findByText(/is empty\. Start here\./)).toBeTruthy()
    expect(screen.queryByRole('region', { name: 'Start from an example' })).toBeNull()
  })
})

describe('the home of an account with a platform experiment of its own', () => {
  it('asks for no examples and shows none', async () => {
    lists([experiment('pe-1', 'Mine', 'prj_1')], [experiment('pe-x', 'Vision benchmark', 'prj_platform')])
    show()
    await waitFor(() => expect(fetchPlatformExperimentsPage).toHaveBeenCalled())
    // Long enough for the answer to render and anything it mounts to ask in turn.
    await new Promise(resolve => setTimeout(resolve, 20))
    expect(vi.mocked(fetchPlatformExperimentsPage).mock.calls.every(([params]) => !params.shared)).toBe(true)
    expect(screen.queryByRole('region', { name: 'Start from an example' })).toBeNull()
    expect(screen.queryByText(/Start here/)).toBeNull()
  })
})
