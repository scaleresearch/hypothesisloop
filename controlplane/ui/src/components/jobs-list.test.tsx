import { render, screen, within } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchOrgClusters } from '@/lib/api/clusters'
import { fetchExperimentsPage, fetchExperimentStats } from '@/lib/api/jobs'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { JobsList } from '@/components/jobs-list'
import type { Experiment, Me } from '@/types/api'

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => '/projects/prj_1/jobs',
  useSearchParams: () => new URLSearchParams(),
}))
vi.mock('@/lib/api/clusters', () => ({ fetchOrgClusters: vi.fn() }))
vi.mock('@/lib/api/jobs', () => ({ cancelExperiment: vi.fn(), fetchExperimentsPage: vi.fn(), fetchExperimentStats: vi.fn() }))
vi.mock('@/components/agent-select', () => ({ AgentSelect: () => null }))
vi.mock('@/components/platform-experiment-select', () => ({ PlatformExperimentSelect: () => null }))
vi.mock('@/components/platform-experiment-name', () => ({ PlatformExperimentName: () => null }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at, actions: ['edit', 'create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

function job(id: string, cluster: string): Experiment {
  return {
    id, agent_id: 'ag-1', agent_name: 'Researcher 1', actor: { kind: 'agent', id: 'ag-1', name: 'ag-1' }, platform_experiment_id: 'pe-1', project_id: 'prj_1', hypothesis_id: 'h1',
    status: 'RUNNING', capacity_tier: 'guaranteed', cluster_name: cluster, created_at: at, updated_at: at,
    accelerator_type: 'nvidia.com/gpu.product=NVIDIA-L40', accelerator_count: 1, hypothesis: 'a claim', env: {}, priority_score: 0,
    code_ref: 'https://github.com/acme/widgets@0123456789abcdef', branch: 'hl/pe-1/warmup-ag-1', actions: ['cancel'],
  } as unknown as Experiment
}

beforeEach(() => {
  vi.mocked(fetchExperimentsPage).mockResolvedValue({
    items: [job('own-0001', 'lab'), job('shared-01', 'platform-gpu'), job('down-0001', 'edge'), job('gone-0001', 'retired')],
    total: 4,
  })
  vi.mocked(fetchExperimentStats).mockResolvedValue({ total: 4, by_status: { RUNNING: 4 } } as never)
  vi.mocked(fetchOrgClusters).mockResolvedValue([
    { name: 'lab', org_id: 'org_a', enabled: true, created_at: at, owned: true, connected: true, autoscaler_enabled: false, actions: [] },
    { name: 'edge', org_id: 'org_a', enabled: true, created_at: at, owned: true, connected: false, autoscaler_enabled: false, actions: [] },
    { name: 'platform-gpu', enabled: true, created_at: at, owned: false, connected: true, autoscaler_enabled: false, actions: [] },
  ])
})

describe('the cluster of a job', () => {
  it('asks for the jobs of the selected project', async () => {
    const resolved = resolveTenant(me, '')
    if (resolved.state !== 'selected') throw new Error('no tenant')
    render(
      <SWRConfig value={{ provider: () => new Map() }}>
        <TenantContext.Provider value={resolved.tenant}><JobsList /></TenantContext.Provider>
      </SWRConfig>,
    )
    await screen.findByText('own-0001…')
    expect(vi.mocked(fetchExperimentsPage).mock.calls[0][0]).toMatchObject({ project_id: 'prj_1' })
    expect(vi.mocked(fetchExperimentStats).mock.calls[0][0]).toMatchObject({ project_id: 'prj_1' })
  })
})

describe('the job of a baseline agent', () => {
  it('tests no hypothesis: it is named the baseline of the objective and opens the graph, on its own branch', async () => {
    vi.mocked(fetchExperimentsPage).mockResolvedValue({
      items: [{ ...job('base-0001', 'lab'), hypothesis_id: undefined, objective: true, hypothesis: '', branch: 'hl/pe-1/baseline' }, job('own-0001', 'lab')],
      total: 2,
    })
    const resolved = resolveTenant(me, '')
    if (resolved.state !== 'selected') throw new Error('no tenant')
    render(
      <SWRConfig value={{ provider: () => new Map() }}>
        <TenantContext.Provider value={resolved.tenant}><JobsList /></TenantContext.Provider>
      </SWRConfig>,
    )
    const row = within((await screen.findByText('base-000…')).closest('tr')!)
    expect(row.getByRole('link', { name: 'Baseline of the objective' }).getAttribute('href')).toBe('/experiments/pe-1/hypotheses')
    expect(row.getByRole('link', { name: 'Baseline of the objective' }).closest('td')!.getAttribute('title')).toBe('Baseline of the objective\nbranch hl/pe-1/baseline')
    expect(row.queryAllByRole('link').some(a => a.getAttribute('href')!.includes('/hypotheses/'))).toBe(false)
    expect(screen.getByRole('link', { name: 'a claim' }).getAttribute('href')).toBe('/experiments/pe-1/hypotheses/h1')
  })
})

describe('the branch of a job', () => {
  it('is said in full where the row says what its job tests', async () => {
    vi.mocked(fetchExperimentsPage).mockResolvedValue({ items: [job('own-0001', 'lab'), { ...job('old-0001', 'lab'), branch: '' }], total: 2 })
    const resolved = resolveTenant(me, '')
    if (resolved.state !== 'selected') throw new Error('no tenant')
    render(
      <SWRConfig value={{ provider: () => new Map() }}>
        <TenantContext.Provider value={resolved.tenant}><JobsList /></TenantContext.Provider>
      </SWRConfig>,
    )
    const tested = (id: string) => within(screen.getByText(id).closest('tr')!).getByRole('link', { name: 'a claim' }).closest('td')!.getAttribute('title')
    await screen.findByText('own-0001…')
    expect(tested('own-0001…')).toBe('a claim\nbranch hl/pe-1/warmup-ag-1')
    // A job from before branches were kept names none.
    expect(tested('old-0001…')).toBe('a claim')
  })
})
