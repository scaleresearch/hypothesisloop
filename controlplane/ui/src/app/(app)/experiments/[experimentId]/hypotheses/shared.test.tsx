import { render, screen } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchPlatformExperimentAgents } from '@/lib/api/agents'
import { fetchHypothesisGraph } from '@/lib/api/hypotheses'
import { fetchPlatformExperiment } from '@/lib/api/platform-experiments'
import type { Me } from '@/types/accounts'
import type { HypothesisGraph } from '@/types/hypotheses'
import { PlatformExperimentStatus } from '@/types/platform-experiments'
import type { PlatformExperiment } from '@/types/platform-experiments'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import HypothesesPage from '@/app/(app)/experiments/[experimentId]/hypotheses/page'

const navigation = vi.hoisted(() => ({ search: '', push: vi.fn(), replace: vi.fn() }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  usePathname: () => '/experiments/pe-x/hypotheses',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api/agents', () => ({ fetchPlatformExperimentAgents: vi.fn() }))
vi.mock('@/lib/api/hypotheses', () => ({
  fetchHypothesisGraph: vi.fn(), searchHypotheses: vi.fn(), registerHumanHypothesis: vi.fn(), addHumanHypothesisComment: vi.fn(), addHumanHypothesisLink: vi.fn(),
}))
vi.mock('@/lib/api/platform-experiments', () => ({ fetchPlatformExperiment: vi.fn() }))
vi.mock('@/components/platform-experiment-select', () => ({ PlatformExperimentSelect: () => <span>experiment select</span> }))
vi.mock('@/components/hypotheses/graph-canvas', async original => ({ ...await original(), HypothesisGraphCanvas: () => <div>graph canvas</div> }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const graph = {
  root: 'pe-x',
  objective: {},
  nodes: [{
    id: 'hyp-1', platform_experiment_id: 'pe-x', text: 'warmup helps', status: 'open', agent_id: 'ag-1',
    actor: { kind: 'user', id: 'u7', name: 'Grace Hopper' }, status_actor: null,
    created_at: at, builds_on: ['pe-x'], finding_count: 0, comment_count: 0, child_count: 0,
  }],
  edges: [],
} as unknown as HypothesisGraph

function show(search: string, pe: Partial<PlatformExperiment>) {
  navigation.search = search
  vi.mocked(fetchPlatformExperiment).mockResolvedValue({ id: 'pe-x', name: 'Vision benchmark', ...pe } as PlatformExperiment)
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><HypothesesPage params={{ experimentId: 'pe-x' }} /></TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  vi.mocked(fetchHypothesisGraph).mockResolvedValue(graph)
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([{ agent_id: 'ag-1', name: 'Researcher 1' }] as never)
})

describe('the hypothesis graph', () => {
  it('of a shared platform experiment of another org is read by its id and takes no idea', async () => {
    show('focus=hyp-1', { project_id: 'prj_platform', shared: true })
    expect(await screen.findByText('graph canvas')).toBeTruthy()
    expect(fetchHypothesisGraph).toHaveBeenCalledWith('pe-x')
    expect(screen.queryByRole('button', { name: /Add idea/ })).toBeNull()
    // It is one of the experiments of the project like any other, so the picker stays.
    expect(screen.getByText('experiment select')).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Open detail →' }).getAttribute('href')).toBe('/experiments/pe-x/hypotheses/hyp-1')
  })

  it('of one of the caller\'s own takes ideas, shared or not', async () => {
    show('focus=hyp-1', { project_id: 'prj_1', shared: true })
    expect(await screen.findByRole('button', { name: 'Add idea' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Link to…' })).toBeTruthy()
  })

  it.each([
    ['a reader of a shared experiment of another org', { project_id: 'prj_platform', shared: true, status: PlatformExperimentStatus.RUNNING }],
    ['a closed experiment of the caller\'s own', { project_id: 'prj_1', shared: false, status: PlatformExperimentStatus.CLOSED }],
  ])('shows %s no control that adds or links', async (_, pe) => {
    Element.prototype.scrollIntoView = vi.fn()
    const own = show('view=outline&focus=hyp-1', { project_id: 'prj_1', shared: false, status: PlatformExperimentStatus.RUNNING })
    await screen.findByRole('button', { name: 'Link to…' })
    expect(screen.getAllByRole('button', { name: /Add idea/ }).length).toBeGreaterThan(0)
    own.unmount()

    show('view=outline&focus=hyp-1', pe)
    await screen.findByRole('button', { name: /warmup helps/ })
    await screen.findByRole('link', { name: 'Open detail →' })
    expect(screen.queryByRole('button', { name: /Add idea/ })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Link to…' })).toBeNull()
  })
})
