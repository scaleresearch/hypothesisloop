import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  fetchAgentsPage, fetchExperimentsPage, fetchExperimentStats, fetchOrgClusters, fetchPlatformExperiment,
} from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { JobsList } from '@/components/jobs-list'
import type { Experiment, Me, PlatformExperiment } from '@/types'

const navigation = vi.hoisted(() => ({ search: '', push: vi.fn() }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  usePathname: () => '/projects/prj_1/jobs',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', () => ({
  cancelExperiment: vi.fn(), fetchExperimentsPage: vi.fn(), fetchExperimentStats: vi.fn(),
  fetchOrgClusters: vi.fn(), fetchAgentsPage: vi.fn(), fetchPlatformExperiment: vi.fn(),
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
  id: 'job-00001', agent_id: 'ag-1', agent_name: 'Researcher 1', summary_due: false, actor: { kind: 'key', id: 'key_1', name: 'ci' }, platform_experiment_id: 'pe-x', project_id: 'prj_platform', hypothesis_id: 'h1',
  status: 'RUNNING', capacity_tier: 'guaranteed', cluster_name: 'platform-gpu', created_at: at, updated_at: at,
  accelerator_type: 'nvidia.com/gpu.product=NVIDIA-L40', accelerator_count: 1, hypothesis: 'a claim', env: {}, priority_score: 0,
  code_ref: 'https://github.com/acme/widgets@0123456789abcdef', branch: 'hl/pe-x/warmup-ag-1',
} as Experiment

function show(search: string, pe: Partial<PlatformExperiment>, experimentID?: string) {
  navigation.search = search
  vi.mocked(fetchPlatformExperiment).mockResolvedValue({ id: 'pe-x', name: 'Vision benchmark', ...pe } as PlatformExperiment)
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><JobsList experimentID={experimentID} /></TenantContext.Provider>
    </SWRConfig>,
  )
}

// What each list read was last asked with. The jobs wait for the experiment to be read; the
// agent filter lists the project's agents until then.
function asked() {
  return [fetchExperimentsPage, fetchExperimentStats, fetchAgentsPage]
    .map(read => vi.mocked(read).mock.lastCall![0] as Record<string, unknown>)
}

beforeEach(() => {
  vi.mocked(fetchExperimentsPage).mockResolvedValue({ items: [running], total: 1 })
  vi.mocked(fetchExperimentStats).mockResolvedValue({ total: 1, by_status: { RUNNING: 1 } } as never)
  vi.mocked(fetchOrgClusters).mockResolvedValue([])
  vi.mocked(fetchAgentsPage).mockResolvedValue({ items: [], total: 0 })
})

describe('the jobs of a shared platform experiment of another org', () => {
  it('are asked for by the experiment alone: the reader\'s project would match none of them', async () => {
    show('', { project_id: 'prj_platform', shared: true }, 'pe-x')
    await screen.findByText('job-0000…')
    for (const params of asked()) {
      expect(params.platform_experiment_id).toBe('pe-x')
      expect('project_id' in params).toBe(false)
    }
    // No job list was asked for within the reader's project on the way there.
    for (const read of [fetchExperimentsPage, fetchExperimentStats]) {
      expect(vi.mocked(read).mock.calls.every(([params]) => !('project_id' in params))).toBe(true)
    }
  })

  it('cannot be cancelled, and open under their experiment', async () => {
    show('', { project_id: 'prj_platform', shared: true }, 'pe-x')
    await userEvent.click(await screen.findByText('job-0000…'))
    expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull()
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/experiments/pe-x/jobs/job-00001')
  })
})

describe('the jobs of a platform experiment of the caller\'s own', () => {
  it('are asked for within the selected project and can be cancelled, shared or not', async () => {
    show('', { project_id: 'prj_1', shared: true }, 'pe-x')
    await screen.findByText('job-0000…')
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeTruthy()
    const [jobs, stats, agents] = asked()
    for (const params of [jobs, stats]) expect(params).toMatchObject({ project_id: 'prj_1', platform_experiment_id: 'pe-x' })
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

  it('are counted per status whatever status is listed, and each count leads to its jobs', async () => {
    show('status=RUNNING', {})
    await screen.findByText('job-0000…')
    const [jobs, stats] = asked()
    expect(jobs.status).toBe('RUNNING')
    expect('status' in stats).toBe(false)
    expect(screen.getByRole('link', { name: /Evicted/ }).getAttribute('href')).toBe('/projects/prj_1/jobs?status=EVICTED')
    expect(screen.getByRole('link', { name: /All/ }).getAttribute('href')).toBe('/projects/prj_1/jobs')
    expect(screen.getByRole('link', { current: true }).textContent).toContain('Running')
  })
})

describe('the jobs of one platform experiment', () => {
  it('are their own page, and each status of them a filter on it', async () => {
    show('status=RUNNING', { project_id: 'prj_1', shared: false }, 'pe-x')
    await screen.findByText('job-0000…')
    expect(screen.getByRole('link', { name: /Evicted/ }).getAttribute('href')).toBe('/experiments/pe-x/jobs?status=EVICTED')
    expect(screen.getByRole('link', { name: /All/ }).getAttribute('href')).toBe('/experiments/pe-x/jobs')
  })
})
