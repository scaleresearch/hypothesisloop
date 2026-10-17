import { render, screen } from '@testing-library/react'
import type { ReactNode } from 'react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchPlatformExperimentAgents } from '@/lib/api/agents'
import { fetchHypothesisStats } from '@/lib/api/hypotheses'
import { fetchExperimentStats } from '@/lib/api/jobs'
import { fetchPlatformExperimentQuotas } from '@/lib/api/quotas'
import { ExperimentActionsProvider, useExperimentActions } from '@/lib/experiment-actions'
import { GraphEditorProvider, useGraphEditor } from '@/components/hypotheses/editor'
import { AddIdeaButton } from '@/components/hypotheses/editor-controls'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { ExperimentAgents } from '@/components/experiment-agents'
import { AddCommentForm, AddLinkForm } from '@/components/hypotheses/human-idea'
import { HypothesisPanel, ObjectiveBar } from '@/components/hypotheses/panel'
import { PlatformExperimentCard } from '@/components/platform-experiment-card'
import type { GraphNode, HypothesisGraph, HypothesisWithJobs, JoinedAgent, Me, PlatformExperiment } from '@/types/api'

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => '/experiments/pe-x/hypotheses',
  useSearchParams: () => new URLSearchParams(),
}))
vi.mock('@/lib/api/agents', () => ({
  addPlatformExperimentAgents: vi.fn(), fetchPlatformExperimentAgents: vi.fn(), fetchPlatformExperimentAgentToken: vi.fn(), removePlatformExperimentAgent: vi.fn(),
}))
vi.mock('@/lib/api/hypotheses', () => ({
  registerHumanHypothesis: vi.fn(), addHumanHypothesisComment: vi.fn(), addHumanHypothesisLink: vi.fn(), searchHypotheses: vi.fn(), fetchHypothesisStats: vi.fn(),
}))
vi.mock('@/lib/api/jobs', () => ({ fetchExperimentStats: vi.fn() }))
vi.mock('@/lib/api/quotas', () => ({ fetchPlatformExperimentQuotas: vi.fn() }))

const at = '2026-01-01T00:00:00Z'
// A self-run agent of the experiment, as the API lists it to one who writes nothing of it.
const self: JoinedAgent = { platform_experiment_id: 'pe-x', agent_id: 'ag-self', name: 'ag-self', role: 'competitor', actor: { kind: 'user', id: 'u1', name: 'Ada' }, hosted: false, agent_type: 'generalist', model: '', hyperparameters: null, at_work: true, signed_up_at: at, actions: [] }
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at, actions: ['edit', 'create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const pe = {
  id: 'pe-x', project_id: 'prj_platform', name: 'Vision benchmark', status: 'open' as const, shared: true,
  budget_accelerator_hours: 100, max_agents: 4, signup_count: 2, stages: [], current_stage: 1, created_at: at,
  description: '', metrics: [], researchers: [], coordinator_hosted: false, max_hosted: 0, report_interval_seconds: 60, starts_at: at, updated_at: at,
  editable: [], actions: [],
} as PlatformExperiment
// The same experiment as the API answers it to its owner.
const owned: PlatformExperiment = { ...pe, project_id: 'prj_1', editable: ['name', 'description'], actions: ['remove', 'close', 'add_agent', 'add_idea'] }
const node = {
  id: 'hyp-1', platform_experiment_id: 'pe-x', text: 'warmup helps', status: 'open', actor: { kind: 'user', id: 'u1', name: 'Ada' }, agent_id: '',
  created_at: at, builds_on: ['pe-x'], finding_count: 0, comment_count: 0, child_count: 0, actions: [],
} as unknown as GraphNode
// The same idea as the API answers it to the person who wrote it.
const ownNode: GraphNode = { ...node, actions: ['edit', 'remove', 'link', 'comment'] }

// As the hypotheses page wires them: an account that only reads writes nothing.
function GraphPanels({ node }: { node: GraphNode }) {
  const actions = useExperimentActions()
  const graph = { root: 'pe-x', objective: {}, nodes: [node], edges: [] } as unknown as HypothesisGraph
  const editor = useGraphEditor({ graph, rootLabel: pe.name, canWrite: actions.includes('add_idea'), mutate: vi.fn(), onSelect: () => {} })
  return (
    <GraphEditorProvider value={editor}>
      <AddIdeaButton />
      <ObjectiveBar graph={graph} pe={pe} />
      <HypothesisPanel graph={graph} nodeByID={new Map([[node.id, node]])} selected="hyp-1" onSelect={() => {}} />
    </GraphEditorProvider>
  )
}

// Each case renders the same thing twice: as the owner sees it, where the control must be there
// for its absence to mean anything, and as an account that only reads the shared experiment.
function show(ui: ReactNode, experiment: PlatformExperiment) {
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}>
        <ExperimentActionsProvider value={experiment.actions}>{ui}</ExperimentActionsProvider>
      </TenantContext.Provider>
    </SWRConfig>,
  )
}

function names(role: 'button' | 'textbox') {
  return screen.queryAllByRole(role).map(b => b.getAttribute('aria-label') ?? b.textContent)
}

beforeEach(() => {
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([self])
  vi.mocked(fetchPlatformExperimentQuotas).mockResolvedValue([])
  vi.mocked(fetchHypothesisStats).mockResolvedValue({ total: 0, by_status: {} })
  vi.mocked(fetchExperimentStats).mockResolvedValue({ total: 0, by_status: {} } as never)
})

describe('a shared platform experiment of another org shows no control that writes', () => {
  it('to its agents: no add, no token, no remove', async () => {
    vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([{ ...self, actions: ['message', 'remove', 'show_token'] }])
    const own = show(<ExperimentAgents pe={owned} />, owned)
    await screen.findByRole('cell', { name: 'ag-self' })
    expect(names('button')).toEqual(expect.arrayContaining(['Add agent', 'Show token', 'Remove']))
    own.unmount()

    vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([self])
    show(<ExperimentAgents pe={pe} />, pe)
    await screen.findByRole('cell', { name: 'ag-self' })
    expect(names('button')).toEqual(['Details of ag-self'])
    expect(names('textbox')).toEqual([])
  })

  it('to its hypothesis graph: no idea under the objective or under a hypothesis', () => {
    const own = show(<GraphPanels node={ownNode} />, owned)
    expect(names('button')).toEqual(expect.arrayContaining(['Add idea', 'Link to…']))
    own.unmount()

    show(<GraphPanels node={node} />, pe)
    expect(names('button').filter(n => n?.includes('Add idea') || n?.includes('Link to'))).toEqual([])
    // Reading on stays possible, from the reader's own project.
    expect(screen.getByRole('link', { name: 'Open detail →' }).getAttribute('href')).toBe('/experiments/pe-x/hypotheses/hyp-1')
    expect(screen.getByRole('link', { name: 'Experiment →' }).getAttribute('href')).toBe('/experiments/pe-x')
  })

  it('to a hypothesis: no link or comment form', () => {
    const forms = (hypothesis: HypothesisWithJobs) => (
      <>
        <AddLinkForm hypothesis={hypothesis} onAdded={() => {}} />
        <AddCommentForm hypothesis={hypothesis} onAdded={() => {}} />
      </>
    )
    const of = (n: GraphNode) => ({ ...n, links: { out: [], in: [] } }) as unknown as HypothesisWithJobs
    const own = show(forms(of(ownNode)), owned)
    expect(names('button')).toEqual(expect.arrayContaining(['Add comment']))
    expect(screen.getByText('Add link')).toBeTruthy()
    own.unmount()

    const shared = show(forms(of(node)), pe)
    expect(shared.container.textContent).toBe('')
  })

  it('to the experiment in the list: no edit, and a badge that says why', () => {
    const own = show(<PlatformExperimentCard pe={owned} onEdit={() => {}} />, owned)
    expect(names('button')).toContain('Edit')
    expect(screen.getByText('Shared')).toBeTruthy()
    own.unmount()

    const plain = show(<PlatformExperimentCard pe={{ ...owned, shared: false }} onEdit={() => {}} />, owned)
    expect(screen.queryByText(/Shared/)).toBeNull()
    plain.unmount()

    // The card tells from the experiment itself: it sits in the list of the reader's own project.
    show(<PlatformExperimentCard pe={pe} onEdit={() => {}} />, pe)
    expect(screen.getByText('Vision benchmark')).toBeTruthy()
    expect(screen.getByText('Shared · read only')).toBeTruthy()
    expect(names('button')).toEqual([])
  })
})
