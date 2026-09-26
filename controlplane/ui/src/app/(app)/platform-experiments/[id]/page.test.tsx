import { render, screen } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import * as api from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import PlatformExperimentDetailPage from '@/app/(app)/platform-experiments/[id]/page'
import { PlatformExperimentStatus } from '@/types'
import type { Me, PlatformExperiment } from '@/types'

const navigation = vi.hoisted(() => ({ search: '', push: vi.fn(), replace: vi.fn() }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  usePathname: () => '/platform-experiments/pe-x',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', () => ({
  fetchPlatformExperiment: vi.fn(), fetchPlatformExperimentQuotas: vi.fn(), fetchPlatformExperimentResults: vi.fn(),
  fetchExperimentsPage: vi.fn(), fetchExperimentStats: vi.fn(), fetchStages: vi.fn(),
  fetchExperimentMetrics: vi.fn(), fetchPlatformExperimentTimeseries: vi.fn(), fetchHypothesisStats: vi.fn(),
  fetchFindings: vi.fn(),
  addPlatformExperimentAgents: vi.fn(), fetchPlatformExperimentAgents: vi.fn(),
  fetchPlatformExperimentAgentToken: vi.fn(), removePlatformExperimentAgent: vi.fn(),
  MAX_LIST_PAGE_SIZE: 200,
  HYPOTHESIS_STATUSES: ['open', 'confirmed', 'refuted', 'inconclusive'],
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

function show(search: string, more: Partial<PlatformExperiment>) {
  navigation.search = search
  vi.mocked(api.fetchPlatformExperiment).mockResolvedValue({
    id: 'pe-x', name: 'Vision benchmark', project_id: 'prj_platform', status: PlatformExperimentStatus.OPEN, shared: true,
    budget_accelerator_hours: 100, max_agents: 4, signup_count: 1, stages: [], current_stage: 1, created_at: at, ...more,
  })
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><PlatformExperimentDetailPage params={{ id: 'pe-x' }} /></TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  vi.mocked(api.fetchPlatformExperimentQuotas).mockResolvedValue([])
  vi.mocked(api.fetchPlatformExperimentResults).mockResolvedValue({ metrics: [], bests: [] })
  vi.mocked(api.fetchExperimentsPage).mockResolvedValue({ items: [], total: 0 })
  vi.mocked(api.fetchExperimentStats).mockResolvedValue({ total: 0, by_status: {} } as never)
  vi.mocked(api.fetchStages).mockResolvedValue({ stages: [], current_stage: 1, progress: 0, next_boundary_progress: 1, advances: [], active_agents: [], cut_agents: [] })
  vi.mocked(api.fetchHypothesisStats).mockResolvedValue({ total: 0, by_status: {} })
  vi.mocked(api.fetchFindings).mockResolvedValue([])
  vi.mocked(api.fetchPlatformExperimentAgents).mockResolvedValue([
    { platform_experiment_id: 'pe-x', agent_id: 'ag-self', name: 'ag-self', role: 'competitor', hosted: false, signed_up_at: at },
  ])
})

describe('a shared platform experiment of another org', () => {
  it('opens in the reader\'s own project, with one note that it is shared and read only', async () => {
    show('project=prj_1', {})
    expect(await screen.findByRole('heading', { name: 'Vision benchmark' })).toBeTruthy()
    expect(screen.getByText('Shared · read only')).toBeTruthy()
    expect(screen.getAllByRole('note')).toHaveLength(1)
    expect(screen.getByRole('note').textContent).toMatch(/shares this experiment with every account.*change nothing/)
    // The URL keeps the reader's project: the experiment's is not theirs to be in.
    expect(navigation.replace).not.toHaveBeenCalled()
  })

  it('says why a closed one closed: its end time, or who closed it early', async () => {
    const closed = { status: PlatformExperimentStatus.CLOSED, closed_at: at }
    const first = show('project=prj_1', { ...closed, close_reason: 'end_time_reached' })
    expect(await screen.findByText('reached its end time')).toBeTruthy()
    first.unmount()
    show('project=prj_1', { ...closed, close_reason: 'closed_by_request', closed_by: { kind: 'user', id: 'u2', name: 'Grace' } })
    expect((await screen.findByText(/closed early by/)).textContent).toContain('Grace')
    expect(screen.queryByText('reached its end time')).toBeNull()
  })

  it('shows its agents and no control that writes', async () => {
    show('project=prj_1', {})
    expect(await screen.findByRole('cell', { name: 'ag-self' })).toBeTruthy()
    for (const control of ['Add agent', 'Show token', 'Remove']) {
      expect(screen.queryByRole('button', { name: control })).toBeNull()
    }
  })

  it('reads its lists within the experiment\'s own project, never the reader\'s', async () => {
    show('project=prj_1', {})
    await screen.findByRole('heading', { name: 'Vision benchmark' })
    for (const read of [api.fetchExperimentsPage, api.fetchExperimentStats]) {
      await vi.waitFor(() => expect(read).toHaveBeenCalled())
      for (const [params] of vi.mocked(read).mock.calls) {
        expect(params).toMatchObject({ project_id: 'prj_platform', platform_experiment_id: 'pe-x' })
      }
    }
  })
})

describe('a shared platform experiment of the caller\'s own org', () => {
  it('is theirs to change, marked shared, with no read-only note, and puts its project in the URL', async () => {
    show('', { project_id: 'prj_1' })
    expect(await screen.findByRole('button', { name: 'Add agent' })).toBeTruthy()
    expect(await screen.findByRole('button', { name: 'Show token' })).toBeTruthy()
    expect(screen.getByText('Shared')).toBeTruthy()
    expect(screen.queryByRole('note')).toBeNull()
    expect(navigation.replace).toHaveBeenCalledWith('/platform-experiments/pe-x?project=prj_1')
  })
})

describe('a platform experiment nobody shared', () => {
  it('carries no mark', async () => {
    show('project=prj_1', { project_id: 'prj_1', shared: false })
    expect(await screen.findByRole('button', { name: 'Add agent' })).toBeTruthy()
    expect(screen.queryByText(/Shared/)).toBeNull()
  })
})
