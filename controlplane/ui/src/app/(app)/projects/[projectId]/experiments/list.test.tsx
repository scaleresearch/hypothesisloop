import { render, screen, within } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchAgentsPage } from '@/lib/api/agents'
import { fetchOrgClusters } from '@/lib/api/clusters'
import { fetchHypothesisStats } from '@/lib/api/hypotheses'
import { fetchExperimentStats } from '@/lib/api/jobs'
import { fetchPlatformExperiment, fetchPlatformExperimentsPage } from '@/lib/api/platform-experiments'
import { fetchPlatformExperimentQuotas } from '@/lib/api/quotas'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import PlatformExperimentsPage from '@/app/(app)/projects/[projectId]/experiments/page'
import type { Me, PlatformExperiment } from '@/types/api'

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => '/projects/prj_1/experiments',
  useSearchParams: () => new URLSearchParams(),
}))
vi.mock('@/lib/api/agents', () => ({ fetchAgentsPage: vi.fn() }))
vi.mock('@/lib/api/clusters', () => ({ fetchOrgClusters: vi.fn() }))
vi.mock('@/lib/api/hypotheses', () => ({ fetchHypothesisStats: vi.fn() }))
vi.mock('@/lib/api/jobs', () => ({ fetchExperimentStats: vi.fn() }))
vi.mock('@/lib/api/platform-experiments', () => ({ fetchPlatformExperimentsPage: vi.fn(), fetchPlatformExperiment: vi.fn(), createPlatformExperiment: vi.fn(), updatePlatformExperiment: vi.fn() }))
vi.mock('@/lib/api/quotas', () => ({ fetchPlatformExperimentQuotas: vi.fn() }))
vi.mock('@/lib/api/request', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/request')>()),
  apiSend: vi.fn(),
}))
vi.mock('@/components/repository-select', async original => ({ ...await original(), RepositorySelect: () => null }))
vi.mock('@/components/connect-github-step', () => ({ useNeedsGitHub: () => false, useGitHub: () => undefined, ConnectGitHubStep: () => null }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at, actions: ['edit', 'create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

// As the API lists it: a running one of the caller's own project is theirs to edit and close.
function experiment(id: string, name: string, projectID: string, shared: boolean): PlatformExperiment {
  const own = projectID === 'prj_1'
  return {
    id, name, project_id: projectID, status: 'running', shared,
    editable: own ? ['name', 'description', 'duration_hours'] : [], actions: own ? ['remove', 'close', 'add_agent', 'add_idea'] : [],
    budget_accelerator_hours: 100, budget_kind: 'accelerator', max_agents: 4, signup_count: 3, stages: [], current_stage: 1, created_at: at,
    description: '', metrics: [], agents: [], coordinator_in_agentsandbox: false, max_agentsandboxes: 0, report_interval_seconds: 60, starts_at: at, updated_at: at,
  }
}

function show() {
  const resolved = resolveTenant(me, 'prj_1')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><PlatformExperimentsPage /></TenantContext.Provider>
    </SWRConfig>,
  )
}

// The card of the experiment with that name.
function card(name: string) {
  return within(screen.getByText(name).closest('[role="link"]') as HTMLElement)
}

beforeEach(() => {
  vi.mocked(fetchPlatformExperimentQuotas).mockResolvedValue([])
  vi.mocked(fetchHypothesisStats).mockResolvedValue({ total: 0, by_status: {} })
  vi.mocked(fetchExperimentStats).mockResolvedValue({ total: 0, by_status: {} } as never)
  vi.mocked(fetchOrgClusters).mockResolvedValue([])
  vi.mocked(fetchAgentsPage).mockResolvedValue({ items: [], total: 0 })
})

describe('the platform experiments of a project', () => {
  it('are one list, the ones the platform shares among the project\'s own in the list\'s order', async () => {
    vi.mocked(fetchPlatformExperimentsPage).mockResolvedValue({
      items: [experiment('pe-x', 'Vision benchmark', 'prj_platform', true), experiment('pe-1', 'Mine', 'prj_1', false)],
      total: 2,
    })
    show()
    await screen.findByText('Vision benchmark')
    // One read of the project's list, with no filter on shared.
    expect(fetchPlatformExperimentsPage).toHaveBeenCalledWith({ project_id: 'prj_1', status: undefined, q: undefined, sort: '-starts_at', limit: 10, offset: 0 })
    expect(screen.getAllByRole('link').map(l => l.textContent).filter(t => t?.includes('Vision') || t?.includes('Mine')).map(t => t!.includes('Vision') ? 'shared' : 'own')).toEqual(['shared', 'own'])

    expect(card('Vision benchmark').getByText('Shared · read only')).toBeTruthy()
    expect(card('Vision benchmark').queryByRole('button', { name: 'Edit' })).toBeNull()
    expect(card('Mine').queryByText(/Shared/)).toBeNull()
    expect(card('Mine').getByRole('button', { name: 'Edit' })).toBeTruthy()
  })
})

describe('the card of a platform experiment', () => {
  it('says what was spent from the list it is in, in the unit of its budget_kind, and reads no experiment by itself', async () => {
    vi.mocked(fetchPlatformExperimentsPage).mockResolvedValue({
      items: [
        { ...experiment('pe-1', 'On accelerators', 'prj_1', false), consumed_accelerator_hours: 12.5 },
        { ...experiment('pe-2', 'On cores', 'prj_1', false), budget_kind: 'cpu', consumed_accelerator_hours: 3 },
        { ...experiment('pe-3', 'Nothing spent', 'prj_1', false), budget_kind: 'cpu', consumed_accelerator_hours: 0 },
        { ...experiment('pe-4', 'Closed on cores', 'prj_1', false), status: 'closed', budget_kind: 'cpu', consumed_accelerator_hours: 7 },
      ],
      total: 4,
    })
    show()
    expect(await screen.findByText('12.5 of 100 AccH')).toBeTruthy()
    expect(card('On cores').getByText('3 of 100 CPU hours')).toBeTruthy()
    expect(card('Nothing spent').getByText('100 CPU hours')).toBeTruthy()
    expect(card('Closed on cores').getByText('7 CPU hours spent')).toBeTruthy()
    expect(fetchPlatformExperiment).not.toHaveBeenCalled()
  })
})

describe('the subtitle of the list', () => {
  it('counts the agents that ran, and does not say they are active', async () => {
    vi.mocked(fetchAgentsPage).mockResolvedValue({ items: [], total: 5 })
    vi.mocked(fetchPlatformExperimentsPage).mockResolvedValue({ items: [], total: 0 })
    show()
    expect(await screen.findByText('5 agents ran')).toBeTruthy()
  })
})
