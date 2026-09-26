import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchAgentsPage, fetchExperimentsPage, fetchExperimentStats, fetchOrgClusters, fetchUnsummarizedJobIDs } from '@/lib/api'
import { ExampleContext } from '@/lib/example'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import JobsPage from '@/app/(app)/jobs/page'
import type { Experiment, Me } from '@/types'

const navigation = vi.hoisted(() => ({ search: '', push: vi.fn() }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  usePathname: () => '/jobs',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', () => ({
  cancelExperiment: vi.fn(), fetchExperimentsPage: vi.fn(), fetchExperimentStats: vi.fn(),
  fetchOrgClusters: vi.fn(), fetchUnsummarizedJobIDs: vi.fn(), fetchAgentsPage: vi.fn(),
}))
vi.mock('@/components/platform-experiment-select', () => ({ PlatformExperimentSelect: () => <span>experiment select</span> }))
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

function show(example: boolean) {
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}>
        <ExampleContext.Provider value={example ? { id: 'pe-x' } : null}><JobsPage /></ExampleContext.Provider>
      </TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  navigation.search = ''
  vi.mocked(fetchExperimentsPage).mockResolvedValue({ items: [running], total: 1 })
  vi.mocked(fetchExperimentStats).mockResolvedValue({ total: 1, by_status: { RUNNING: 1 } } as never)
  vi.mocked(fetchUnsummarizedJobIDs).mockResolvedValue(new Set())
  vi.mocked(fetchOrgClusters).mockResolvedValue([])
  vi.mocked(fetchAgentsPage).mockResolvedValue({ items: [], total: 0 })
})

describe('the jobs of an example', () => {
  it('are asked for by its platform experiment alone, with no project of the caller\'s', async () => {
    // Whatever the URL filters by, the list is the example's.
    navigation.search = 'example=pe-x&platform_experiment_id=pe-own'
    show(true)
    await screen.findByText('job-0000…')
    for (const read of [fetchExperimentsPage, fetchExperimentStats, fetchUnsummarizedJobIDs, fetchAgentsPage]) {
      const asked = vi.mocked(read).mock.calls[0][0] as Record<string, unknown>
      expect(asked.platform_experiment_id).toBe('pe-x')
      expect('project_id' in asked).toBe(false)
    }
  })

  it('cannot be cancelled, and the list cannot be turned to another experiment', async () => {
    const own = show(false)
    await screen.findByText('job-0000…')
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeTruthy()
    expect(screen.getByText('experiment select')).toBeTruthy()
    expect(vi.mocked(fetchExperimentsPage).mock.calls[0][0]).toMatchObject({ project_id: 'prj_1' })
    own.unmount()

    show(true)
    await screen.findByText('job-0000…')
    expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull()
    expect(screen.queryByText('experiment select')).toBeNull()
  })

  it('open inside the example', async () => {
    navigation.search = 'example=pe-x'
    show(true)
    await userEvent.click(await screen.findByText('job-0000…'))
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/jobs/job-00001?example=pe-x')
  })
})
