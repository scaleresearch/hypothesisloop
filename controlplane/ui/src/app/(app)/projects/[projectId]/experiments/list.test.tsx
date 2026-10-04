import { render, screen, within } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchAgentsPage, fetchExperimentStats, fetchHypothesisStats, fetchOrgClusters, fetchPlatformExperimentQuotas, fetchPlatformExperimentsPage } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import PlatformExperimentsPage from '@/app/(app)/projects/[projectId]/experiments/page'
import { PlatformExperimentStatus } from '@/types'
import type { Me, PlatformExperiment } from '@/types'

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => '/projects/prj_1/experiments',
  useSearchParams: () => new URLSearchParams(),
}))
vi.mock('@/lib/api', () => ({
  fetchPlatformExperimentsPage: vi.fn(), fetchPlatformExperimentQuotas: vi.fn(),
  fetchExperimentStats: vi.fn(), fetchHypothesisStats: vi.fn(), fetchOrgClusters: vi.fn(), fetchAgentsPage: vi.fn(), apiSend: vi.fn(),
  createPlatformExperiment: vi.fn(), updatePlatformExperiment: vi.fn(),
  HYPOTHESIS_STATUSES: ['open', 'confirmed', 'refuted', 'inconclusive'],
}))
vi.mock('@/components/repository-select', () => ({ RepositorySelect: () => null }))
vi.mock('@/components/connect-github-step', () => ({ useNeedsGitHub: () => false, useGitHub: () => undefined, ConnectGitHubStep: () => null }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

function experiment(id: string, name: string, projectID: string, shared: boolean): PlatformExperiment {
  return {
    id, name, project_id: projectID, status: PlatformExperimentStatus.RUNNING, shared,
    budget_accelerator_hours: 100, max_agents: 4, signup_count: 3, stages: [], current_stage: 1, created_at: at,
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
