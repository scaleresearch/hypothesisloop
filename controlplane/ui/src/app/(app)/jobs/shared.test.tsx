import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  fetchAgentsPage, fetchExperimentsPage, fetchExperimentStats, fetchOrgClusters, fetchPlatformExperiment, fetchUnsummarizedJobIDs,
} from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import JobsPage from '@/app/(app)/jobs/page'
import type { Experiment, Me, PlatformExperiment } from '@/types'

const navigation = vi.hoisted(() => ({ search: '', push: vi.fn() }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  usePathname: () => '/jobs',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', () => ({
  cancelExperiment: vi.fn(), fetchExperimentsPage: vi.fn(), fetchExperimentStats: vi.fn(),
  fetchOrgClusters: vi.fn(), fetchUnsummarizedJobIDs: vi.fn(), fetchAgentsPage: vi.fn(), fetchPlatformExperiment: vi.fn(),
}))
vi.mock('@/components/platform-experiment-select', () => ({ PlatformExperimentSelect: () => null }))
vi.mock('@/components/platform-experiment-name', () => ({ PlatformExperimentName: () => null }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const running = {
  id: 'job-00001', agent_id: 'ag-1', platform_experiment_id: 'pe-x', project_id: 'prj_platform', hypothesis_id: 'h1',
  status: 'RUNNING', capacity_tier: 'guaranteed', cluster_name: 'platform-gpu', created_at: at, updated_at: at,
  accelerator_type: 'nvidia.com/gpu.product=NVIDIA-L40', accelerator_count: 1, hypothesis: 'a claim', env: {}, priority_score: 0,
} as Experiment

function show(search: string, pe: Partial<PlatformExperiment>) {
  navigation.search = search
  vi.mocked(fetchPlatformExperiment).mockResolvedValue({ id: 'pe-x', name: 'Vision benchmark', ...pe } as PlatformExperiment)
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><JobsPage /></TenantContext.Provider>
    </SWRConfig>,
  )
}

// What each list read was last asked with. The jobs wait for the experiment to be read; the
// agent filter lists the project's agents until then.
function asked() {
  return [fetchExperimentsPage, fetchExperimentStats, fetchUnsummarizedJobIDs, fetchAgentsPage]
    .map(read => vi.mocked(read).mock.lastCall![0] as Record<string, unknown>)
}

beforeEach(() => {
  vi.mocked(fetchExperimentsPage).mockResolvedValue({ items: [running], total: 1 })
  vi.mocked(fetchExperimentStats).mockResolvedValue({ total: 1, by_status: { RUNNING: 1 } } as never)
  vi.mocked(fetchUnsummarizedJobIDs).mockResolvedValue(new Set())
  vi.mocked(fetchOrgClusters).mockResolvedValue([])
  vi.mocked(fetchAgentsPage).mockResolvedValue({ items: [], total: 0 })
})

describe('the jobs of a shared platform experiment of another org', () => {
  it('are asked for by the experiment alone: the reader\'s project would match none of them', async () => {
    show('project=prj_1&platform_experiment_id=pe-x', { project_id: 'prj_platform', shared: true })
    await screen.findByText('job-0000…')
    for (const params of asked()) {
      expect(params.platform_experiment_id).toBe('pe-x')
      expect('project_id' in params).toBe(false)
    }
    // No job list was asked for within the reader's project on the way there.
    for (const read of [fetchExperimentsPage, fetchExperimentStats, fetchUnsummarizedJobIDs]) {
      expect(vi.mocked(read).mock.calls.every(([params]) => !('project_id' in params))).toBe(true)
    }
  })

  it('cannot be cancelled, and open in the reader\'s own project', async () => {
    show('project=prj_1&platform_experiment_id=pe-x', { project_id: 'prj_platform', shared: true })
    await userEvent.click(await screen.findByText('job-0000…'))
    expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull()
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/jobs/job-00001?project=prj_1')
  })
})

describe('the jobs of a platform experiment of the caller\'s own', () => {
  it('are asked for within the selected project and can be cancelled, shared or not', async () => {
    show('platform_experiment_id=pe-x', { project_id: 'prj_1', shared: true })
    await screen.findByText('job-0000…')
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeTruthy()
    const [jobs, stats, unsummarized, agents] = asked()
    for (const params of [jobs, stats, unsummarized]) expect(params).toMatchObject({ project_id: 'prj_1', platform_experiment_id: 'pe-x' })
    expect(agents).toMatchObject({ project_id: 'prj_1' })
    expect('platform_experiment_id' in agents).toBe(false)
  })
})

describe('the jobs of the whole project', () => {
  it('read no platform experiment and can be cancelled', async () => {
    show('', {})
    await screen.findByText('job-0000…')
    expect(fetchPlatformExperiment).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeTruthy()
    expect(asked()[0]).toMatchObject({ project_id: 'prj_1' })
  })
})
