import { render, screen, waitFor } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchExperimentsPage, fetchExperimentStats, fetchOrgClusters, fetchUnsummarizedJobIDs } from '@/lib/api'
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
  fetchOrgClusters: vi.fn(), fetchUnsummarizedJobIDs: vi.fn(),
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
    id, agent_id: 'ag-1', platform_experiment_id: 'pe-1', project_id: 'prj_1', hypothesis_id: 'h1',
    status: 'RUNNING', capacity_tier: 'guaranteed', cluster_name: cluster, created_at: at, updated_at: at,
    accelerator_type: 'nvidia.com/gpu.product=NVIDIA-L40', accelerator_count: 1, hypothesis: 'a claim', env: {}, priority_score: 0,
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
  vi.mocked(fetchUnsummarizedJobIDs).mockResolvedValue(new Set())
  vi.mocked(fetchOrgClusters).mockResolvedValue([
    { name: 'lab', org_id: 'org_a', shared: false, created_at: at, owned: true, connected: true },
    { name: 'edge', org_id: 'org_a', shared: false, created_at: at, owned: true, connected: false },
    { name: 'platform-gpu', shared: true, created_at: at, owned: false, connected: true },
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
