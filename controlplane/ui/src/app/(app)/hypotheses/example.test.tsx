import { render, screen } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchFindings, fetchHypothesisGraph, fetchPlatformExperiment, fetchPlatformExperimentsPage } from '@/lib/api'
import { ExampleContext } from '@/lib/example'
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
  fetchFindings: vi.fn(), fetchHypothesisGraph: vi.fn(), fetchPlatformExperiment: vi.fn(), fetchPlatformExperimentsPage: vi.fn(),
  searchHypotheses: vi.fn(), registerHumanHypothesis: vi.fn(), addHumanHypothesisComment: vi.fn(), addHumanHypothesisLink: vi.fn(),
}))
vi.mock('@/components/platform-experiment-select', () => ({ PlatformExperimentSelect: () => <span>experiment select</span> }))
vi.mock('@/components/hypothesis-graph', () => ({ HypothesisGraphCanvas: () => <div>graph canvas</div> }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const pe = { id: 'pe-x', project_id: 'prj_platform', name: 'Vision benchmark', shared: true } as PlatformExperiment
const graph = {
  root: 'pe-x',
  nodes: [{
    id: 'hyp-1', platform_experiment_id: 'pe-x', text: 'warmup helps', status: 'open', source: 'agent', agent_id: 'ag-1',
    created_at: at, builds_on: ['pe-x'], finding_count: 0, comment_count: 0, child_count: 0,
  }],
  edges: [],
} as unknown as HypothesisGraph

function show(example: boolean) {
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}>
        <ExampleContext.Provider value={example ? { id: 'pe-x', pe } : null}><HypothesesPage /></ExampleContext.Provider>
      </TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  vi.mocked(fetchPlatformExperiment).mockResolvedValue(pe)
  vi.mocked(fetchHypothesisGraph).mockResolvedValue(graph)
  vi.mocked(fetchFindings).mockResolvedValue([])
  vi.mocked(fetchPlatformExperimentsPage).mockResolvedValue({ items: [], total: 0 })
})

describe('the hypothesis graph of an example', () => {
  it('is that experiment\'s, read by its id, and asks for nothing of the caller\'s project', async () => {
    navigation.search = 'example=pe-x'
    show(true)
    expect(await screen.findByText('graph canvas')).toBeTruthy()
    expect(fetchHypothesisGraph).toHaveBeenCalledWith('pe-x')
    expect(fetchFindings).toHaveBeenCalledWith('pe-x')
    expect(fetchPlatformExperimentsPage).not.toHaveBeenCalled()
  })

  it('cannot be turned to another experiment and takes no idea', async () => {
    navigation.search = 'pe=pe-x&focus=hyp-1'
    const own = show(false)
    expect(await screen.findByRole('button', { name: '+ Add idea under the objective' })).toBeTruthy()
    expect(screen.getByRole('button', { name: '+ Add idea under this' })).toBeTruthy()
    expect(screen.getByText('experiment select')).toBeTruthy()
    own.unmount()

    navigation.search = 'example=pe-x&pe=pe-own&focus=hyp-1'
    show(true)
    expect(await screen.findByText('graph canvas')).toBeTruthy()
    expect(fetchHypothesisGraph).toHaveBeenLastCalledWith('pe-x')
    expect(screen.queryByRole('button', { name: /Add idea/ })).toBeNull()
    expect(screen.queryByText('experiment select')).toBeNull()
    expect(screen.getByRole('link', { name: 'Open detail →' }).getAttribute('href')).toBe('/hypotheses/hyp-1?example=pe-x')
  })
})
