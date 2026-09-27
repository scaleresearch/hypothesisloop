import { render, screen } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchHypothesisGraph, fetchPlatformExperiment, fetchPlatformExperimentAgents, fetchPlatformExperimentsPage } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import HypothesesPage from '@/app/(app)/hypotheses/page'
import type { HypothesisGraph, Me, PlatformExperiment } from '@/types'

const navigation = vi.hoisted(() => ({ search: '', push: vi.fn(), replace: vi.fn() }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  usePathname: () => '/hypotheses',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', () => ({
  fetchHypothesisGraph: vi.fn(), fetchPlatformExperiment: vi.fn(), fetchPlatformExperimentAgents: vi.fn(), fetchPlatformExperimentsPage: vi.fn(),
  searchHypotheses: vi.fn(), registerHumanHypothesis: vi.fn(), addHumanHypothesisComment: vi.fn(), addHumanHypothesisLink: vi.fn(),
}))
vi.mock('@/components/platform-experiment-select', () => ({ PlatformExperimentSelect: () => <span>experiment select</span> }))
vi.mock('@/components/hypothesis-graph', async original => ({ ...await original(), HypothesisGraphCanvas: () => <div>graph canvas</div> }))

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
      <TenantContext.Provider value={resolved.tenant}><HypothesesPage /></TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  vi.mocked(fetchHypothesisGraph).mockResolvedValue(graph)
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([{ agent_id: 'ag-1', name: 'Researcher 1' }] as never)
})

describe('the hypothesis graph', () => {
  it('of a shared platform experiment of another org is read by its id and takes no idea', async () => {
    show('project=prj_1&pe=pe-x&focus=hyp-1', { project_id: 'prj_platform', shared: true })
    expect(await screen.findByText('graph canvas')).toBeTruthy()
    expect(fetchHypothesisGraph).toHaveBeenCalledWith('pe-x')
    expect(screen.queryByRole('button', { name: /Add idea/ })).toBeNull()
    // It is one of the experiments of the project like any other, so the picker stays.
    expect(screen.getByText('experiment select')).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Open detail →' }).getAttribute('href')).toBe('/hypotheses/hyp-1?project=prj_1')
  })

  it('of one of the caller\'s own takes ideas, shared or not', async () => {
    show('pe=pe-x&focus=hyp-1', { project_id: 'prj_1', shared: true })
    expect(await screen.findByRole('button', { name: '+ Add idea under the objective' })).toBeTruthy()
    expect(screen.getByRole('button', { name: '+ Add idea under this' })).toBeTruthy()
  })

  it('names who registered the selected hypothesis beside the agent it was registered for, by its name', async () => {
    show('pe=pe-x&focus=hyp-1', { project_id: 'prj_1', shared: false })
    expect((await screen.findByText('Grace Hopper')).getAttribute('title')).toBe('u7')
    expect(screen.getByText('human')).toBeTruthy()
    expect((await screen.findByText('Researcher 1')).getAttribute('title')).toBe('ag-1')
  })

  it('shows what the baseline found on the objective, which no hypothesis stands for', async () => {
    vi.mocked(fetchHypothesisGraph).mockResolvedValue({
      ...graph,
      objective: {
        finding: { id: 'f1', hypothesis_id: 'pe-x', experiment_id: 'job-base', agent_id: 'ag-0', actor: { kind: 'agent', id: 'ag-0', name: 'Baseline' }, summary: 'the stock config reaches 1.8', created_at: at },
        best_job: { id: 'job-base', metric: { name: 'val_loss', value: 1.8, last: 1.8 }, summary_due: false },
      },
    } as unknown as HypothesisGraph)
    Element.prototype.scrollIntoView = vi.fn()
    show('pe=pe-x&view=outline', { project_id: 'prj_1', shared: false })
    expect((await screen.findByRole('link', { name: 'val_loss 1.8000' })).getAttribute('href')).toBe('/jobs/job-base')
    expect(screen.getByText('the stock config reaches 1.8')).toBeTruthy()
    const root = screen.getByRole('button', { name: /Objective/ })
    expect(root.textContent).toBe('ObjectiveVision benchmarkbaseline 1.8000')
    expect(root.querySelector('[title="Baseline finding: the stock config reaches 1.8"]')).toBeTruthy()
    expect(screen.getAllByRole('listitem')).toHaveLength(2)
  })

  it('opens on the newest experiment listed for the project, which a shared one may be', async () => {
    vi.mocked(fetchPlatformExperimentsPage).mockResolvedValue({ items: [{ id: 'pe-x', project_id: 'prj_platform', shared: true } as PlatformExperiment], total: 1 })
    show('project=prj_1', { project_id: 'prj_platform', shared: true })
    await vi.waitFor(() => expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/hypotheses?pe=pe-x&project=prj_1'))
    expect(fetchPlatformExperimentsPage).toHaveBeenCalledWith({ project_id: 'prj_1', sort: '-starts_at', limit: 1 })
  })
})
