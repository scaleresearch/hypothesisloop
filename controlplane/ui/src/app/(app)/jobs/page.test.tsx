import { render, screen, waitFor, within } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchExperimentsPage, fetchExperimentStats, fetchOrgClusters } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import JobsPage from '@/app/(app)/jobs/page'
import type { Experiment, Me } from '@/types'

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => '/jobs',
  useSearchParams: () => new URLSearchParams(),
}))
vi.mock('@/lib/api', () => ({
  cancelExperiment: vi.fn(), fetchExperimentsPage: vi.fn(), fetchExperimentStats: vi.fn(),
  fetchOrgClusters: vi.fn(),
}))
vi.mock('@/components/agent-select', () => ({ AgentSelect: () => null }))
vi.mock('@/components/platform-experiment-select', () => ({ PlatformExperimentSelect: () => null }))
vi.mock('@/components/platform-experiment-name', () => ({ PlatformExperimentName: () => null }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

function job(id: string, cluster: string): Experiment {
  return {
    id, agent_id: 'ag-1', agent_name: 'Researcher 1', summary_due: false, actor: { kind: 'agent', id: 'ag-1', name: 'ag-1' }, platform_experiment_id: 'pe-1', project_id: 'prj_1', hypothesis_id: 'h1',
    status: 'RUNNING', capacity_tier: 'guaranteed', cluster_name: cluster, created_at: at, updated_at: at,
    accelerator_type: 'nvidia.com/gpu.product=NVIDIA-L40', accelerator_count: 1, hypothesis: 'a claim', env: {}, priority_score: 0,
    code_ref: 'https://github.com/acme/widgets@0123456789abcdef', branch: 'hl/pe-1/warmup-ag-1',
  } as Experiment
}

// The title of the status dot beside the cluster name in a job's row, null when there is no dot.
function dotOf(jobID: string) {
  const row = screen.getByText(`${jobID.slice(0, 8)}…`).closest('tr')!
  return row.querySelector('span[title^="Cluster"]')?.getAttribute('title') ?? null
}

beforeEach(() => {
  vi.mocked(fetchExperimentsPage).mockResolvedValue({
    items: [job('own-0001', 'lab'), job('shared-01', 'platform-gpu'), job('down-0001', 'edge'), job('gone-0001', 'retired')],
    total: 4,
  })
  vi.mocked(fetchExperimentStats).mockResolvedValue({ total: 4, by_status: { RUNNING: 4 } } as never)
  vi.mocked(fetchOrgClusters).mockResolvedValue([
    { name: 'lab', org_id: 'org_a', shared: false, enabled: true, created_at: at, owned: true, connected: true, autoscaler_enabled: false },
    { name: 'edge', org_id: 'org_a', shared: false, enabled: true, created_at: at, owned: true, connected: false, autoscaler_enabled: false },
    { name: 'platform-gpu', shared: true, enabled: true, created_at: at, owned: false, connected: true, autoscaler_enabled: false },
  ])
})

describe('the cluster of a job', () => {
  it('shows whether its agent is connected, for the org\'s own clusters and the shared ones alike', async () => {
    const resolved = resolveTenant(me, '')
    if (resolved.state !== 'selected') throw new Error('no tenant')
    render(
      <SWRConfig value={{ provider: () => new Map() }}>
        <TenantContext.Provider value={resolved.tenant}><JobsPage /></TenantContext.Provider>
      </SWRConfig>,
    )
    await waitFor(() => expect(dotOf('own-0001')).toBe('Cluster connected'))
    expect(fetchOrgClusters).toHaveBeenCalledWith('org_a')
    expect(dotOf('shared-01')).toBe('Cluster connected')
    expect(dotOf('down-0001')).toBe('Cluster disconnected')
    // A cluster the API no longer lists for the org has no known state to show.
    expect(dotOf('gone-0001')).toBeNull()
  })

  it('shows no dot, and says why, when the clusters cannot be read', async () => {
    vi.mocked(fetchOrgClusters).mockRejectedValue(new Error('Failed to fetch'))
    const resolved = resolveTenant(me, '')
    if (resolved.state !== 'selected') throw new Error('no tenant')
    render(
      <SWRConfig value={{ provider: () => new Map(), shouldRetryOnError: false }}>
        <TenantContext.Provider value={resolved.tenant}><JobsPage /></TenantContext.Provider>
      </SWRConfig>,
    )
    expect(await screen.findByText(/Could not load the clusters of Ada, so no job shows whether its cluster is connected: Failed to fetch/)).toBeTruthy()
    expect(dotOf('own-0001')).toBeNull()
  })

  it('asks for the jobs of the selected project', async () => {
    const resolved = resolveTenant(me, '')
    if (resolved.state !== 'selected') throw new Error('no tenant')
    render(
      <SWRConfig value={{ provider: () => new Map() }}>
        <TenantContext.Provider value={resolved.tenant}><JobsPage /></TenantContext.Provider>
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
        <TenantContext.Provider value={resolved.tenant}><JobsPage /></TenantContext.Provider>
      </SWRConfig>,
    )
    const row = within((await screen.findByText('base-000…')).closest('tr')!)
    expect(row.getByRole('link', { name: 'Baseline of the objective' }).getAttribute('href')).toBe('/hypotheses?pe=pe-1')
    expect(row.getByRole('link', { name: 'baseline' }).getAttribute('href')).toBe('https://github.com/acme/widgets/tree/hl/pe-1/baseline')
    expect(row.queryAllByRole('link').some(a => a.getAttribute('href')!.startsWith('/hypotheses/'))).toBe(false)
    expect(screen.getByRole('link', { name: 'a claim' }).getAttribute('href')).toBe('/hypotheses/h1')
  })
})

describe('the branch of a job', () => {
  it('is named without its experiment\'s prefix and opens in the repository its commit is in', async () => {
    vi.mocked(fetchExperimentsPage).mockResolvedValue({ items: [job('own-0001', 'lab'), { ...job('old-0001', 'lab'), branch: '' }], total: 2 })
    const resolved = resolveTenant(me, '')
    if (resolved.state !== 'selected') throw new Error('no tenant')
    render(
      <SWRConfig value={{ provider: () => new Map() }}>
        <TenantContext.Provider value={resolved.tenant}><JobsPage /></TenantContext.Provider>
      </SWRConfig>,
    )
    const branch = await screen.findByRole('link', { name: 'warmup-ag-1' })
    expect(branch.getAttribute('href')).toBe('https://github.com/acme/widgets/tree/hl/pe-1/warmup-ag-1')
    expect(branch.getAttribute('title')).toBe('hl/pe-1/warmup-ag-1')
    // A job from before branches were kept names none.
    expect(screen.getAllByRole('link', { name: 'warmup-ag-1' })).toHaveLength(1)
  })
})
