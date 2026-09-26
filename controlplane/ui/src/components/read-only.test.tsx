import { render, screen } from '@testing-library/react'
import type { ReactNode } from 'react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  fetchExperimentStats, fetchHypothesisStats, fetchPlatformExperimentAgents, fetchPlatformExperimentQuotas,
} from '@/lib/api'
import { ExampleContext } from '@/lib/example'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { ExampleBanner } from '@/components/example-banner'
import { ExperimentAgents } from '@/components/experiment-agents'
import { AddCommentForm, AddIdeaForm, AddLinkForm } from '@/components/human-idea'
import { HypothesisPanel, ObjectiveBar } from '@/components/hypothesis-panel'
import { PlatformExperimentCard } from '@/components/platform-experiment-card'
import { PlatformExperimentStatus } from '@/types'
import type { HypothesisGraph, HypothesisListItem, HypothesisWithJobs, Me, PlatformExperiment } from '@/types'

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => '/platform-experiments/pe-x',
  useSearchParams: () => new URLSearchParams('example=pe-x'),
}))
vi.mock('@/lib/api', () => ({
  addPlatformExperimentAgents: vi.fn(), fetchPlatformExperimentAgents: vi.fn(),
  fetchPlatformExperimentAgentToken: vi.fn(), removePlatformExperimentAgent: vi.fn(),
  registerHumanHypothesis: vi.fn(), addHumanHypothesisComment: vi.fn(), addHumanHypothesisLink: vi.fn(),
  searchHypotheses: vi.fn(),
  fetchPlatformExperimentQuotas: vi.fn(), fetchExperimentStats: vi.fn(), fetchHypothesisStats: vi.fn(),
  HYPOTHESIS_STATUSES: ['open', 'confirmed', 'refuted', 'inconclusive'],
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const pe = {
  id: 'pe-x', project_id: 'prj_platform', name: 'Vision benchmark', status: PlatformExperimentStatus.OPEN, shared: true,
  budget_accelerator_hours: 100, max_agents: 4, signup_count: 2, stages: [], current_stage: 1, created_at: at,
} as PlatformExperiment
const node = {
  id: 'hyp-1', platform_experiment_id: 'pe-x', text: 'warmup helps', status: 'open', source: 'human', author: 'Ada', agent_id: '',
  created_at: at, builds_on: ['pe-x'], finding_count: 0, comment_count: 0, child_count: 0,
} as unknown as HypothesisListItem
const graph: HypothesisGraph = { root: 'pe-x', nodes: [node], edges: [] } as unknown as HypothesisGraph

// Each case renders the same thing twice: as the owner sees it, where the control must be there
// for its absence to mean anything, and inside an example.
function show(ui: ReactNode, example: boolean) {
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}>
        <ExampleContext.Provider value={example ? { id: 'pe-x', pe } : null}>{ui}</ExampleContext.Provider>
      </TenantContext.Provider>
    </SWRConfig>,
  )
}

function names(role: 'button' | 'textbox') {
  return screen.queryAllByRole(role).map(b => b.getAttribute('aria-label') ?? b.textContent)
}

beforeEach(() => {
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([
    { platform_experiment_id: 'pe-x', agent_id: 'ag-self', name: 'ag-self', role: 'competitor', hosted: false, signed_up_at: at },
  ])
  vi.mocked(fetchPlatformExperimentQuotas).mockResolvedValue([])
  vi.mocked(fetchHypothesisStats).mockResolvedValue({ total: 0, by_status: {} })
  vi.mocked(fetchExperimentStats).mockResolvedValue({ total: 0, by_status: {} } as never)
})

describe('an example shows no control that writes', () => {
  it('to its agents: no add, no token, no remove', async () => {
    const own = show(<ExperimentAgents pe={pe} />, false)
    await screen.findByRole('cell', { name: 'ag-self' })
    expect(names('button')).toEqual(expect.arrayContaining(['Add agent', 'Show token', 'Remove']))
    own.unmount()

    show(<ExperimentAgents pe={pe} />, true)
    await screen.findByRole('cell', { name: 'ag-self' })
    expect(names('button')).toEqual([])
    expect(names('textbox')).toEqual([])
  })

  it('to its hypothesis graph: no idea under the objective or under a hypothesis', () => {
    const panel = (
      <>
        <ObjectiveBar graph={graph} pe={pe} onAddIdea={() => {}} />
        <HypothesisPanel graph={graph} nodeByID={new Map([[node.id, node]])} evidence={new Map()} selected="hyp-1" onSelect={() => {}} onAddIdea={() => {}} />
      </>
    )
    const own = show(panel, false)
    expect(names('button')).toEqual(expect.arrayContaining(['+ Add idea under the objective', '+ Add idea under this']))
    own.unmount()

    show(panel, true)
    expect(names('button').filter(n => n?.includes('Add idea'))).toEqual([])
    // Reading on stays possible, inside the example.
    expect(screen.getByRole('link', { name: 'Open detail →' }).getAttribute('href')).toBe('/hypotheses/hyp-1?example=pe-x')
    expect(screen.getByRole('link', { name: 'Experiment →' }).getAttribute('href')).toBe('/platform-experiments/pe-x?example=pe-x')
  })

  it('to a hypothesis: no idea, link or comment form', () => {
    const hypothesis = { ...node, links: { out: [], in: [] } } as unknown as HypothesisWithJobs
    const forms = (
      <>
        <AddIdeaForm platformExperimentID="pe-x" parent={{ id: 'pe-x', label: 'the objective' }} onAdded={() => {}} />
        <AddLinkForm hypothesis={hypothesis} onAdded={() => {}} />
        <AddCommentForm hypothesisID="hyp-1" onAdded={() => {}} />
      </>
    )
    const own = show(forms, false)
    expect(names('button')).toEqual(expect.arrayContaining(['Add idea', 'Add comment']))
    expect(screen.getByText('Add link')).toBeTruthy()
    own.unmount()

    const example = show(forms, true)
    expect(example.container.textContent).toBe('')
  })

  it('to the experiment in the list: no edit, and it opens as an example', async () => {
    const own = show(<PlatformExperimentCard pe={pe} onEdit={() => {}} />, false)
    expect(names('button')).toContain('Edit')
    own.unmount()

    show(<PlatformExperimentCard pe={pe} />, true)
    expect(screen.getByText('Vision benchmark')).toBeTruthy()
    expect(names('button')).toEqual([])
  })
})

describe('the banner', () => {
  it('names the example on its pages and leads back to the list', () => {
    show(<ExampleBanner />, true)
    const banner = screen.getByRole('note')
    expect(banner.textContent).toContain('Example, read only')
    expect(banner.textContent).toContain('Vision benchmark')
    expect(screen.getByRole('link', { name: 'All examples' }).getAttribute('href')).toBe('/examples')
  })

  it('is absent everywhere else', () => {
    show(<ExampleBanner />, false)
    expect(screen.queryByRole('note')).toBeNull()
  })
})
