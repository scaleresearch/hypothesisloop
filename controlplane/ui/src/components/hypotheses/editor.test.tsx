import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { useMemo, useState } from 'react'
import type { KeyedMutator } from 'swr'
import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { addHumanHypothesisLink, editHumanHypothesis, registerHumanHypothesis, removeHumanHypothesis } from '@/lib/api/hypotheses'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { GraphEditorProvider, useGraphEditor } from '@/components/hypotheses/editor'
import { AddIdeaButton, AddedNote, LinkBar } from '@/components/hypotheses/editor-controls'
import { IdeaEditor } from '@/components/hypotheses/idea-editor'
import { HypothesisPanel } from '@/components/hypotheses/panel'
import { HypothesisGraphCanvas } from '@/components/hypotheses/graph-canvas'
import { HypothesisOutline } from '@/components/hypotheses/outline'
import { Expandable } from '@/components/ui/expandable'
import { layoutGraph } from '@/lib/hypothesis-graph'
import type { GraphNode, HypothesisAction, HypothesisGraph, RegisteredHypothesis, SimilarHypothesis } from '@/types/api'

// Its dot pattern reads a size jsdom never lays out.
vi.mock('@xyflow/react', async original => ({ ...await original(), Background: () => null }))
vi.mock('@/lib/api/hypotheses', () => ({
  registerHumanHypothesis: vi.fn(), addHumanHypothesisLink: vi.fn(), editHumanHypothesis: vi.fn(), removeHumanHypothesis: vi.fn(),
}))
vi.mock('@/components/agent-label', () => ({ AgentLabel: ({ id }: { id: string }) => <span>{id}</span>, useAgentName: () => (id: string) => id }))

const at = '2026-01-01T00:00:00Z'
const ada = { kind: 'user' as const, id: 'u1', name: 'Ada' }

// As the API answers it to Ada: her own idea is hers to change, and an agent's takes her comments alone.
function idea(id: string, text: string, buildsOn: string[], agentID = ''): GraphNode {
  return {
    id, text, platform_experiment_id: 'pe-x', status: 'open', agent_id: agentID, actor: ada, status_changed_at: null,
    created_at: at, builds_on: buildsOn, finding_count: 0, comment_count: 0, child_count: 0,
    actions: agentID ? ['comment'] : ['edit', 'remove', 'link', 'comment'],
  }
}

const initial: HypothesisGraph = {
  root: 'pe-x',
  objective: { comment_count: 0, comments: [] },
  nodes: [idea('hyp-1', 'warmup helps', ['pe-x']), idea('hyp-2', 'a cosine schedule helps', ['pe-x'], 'ag-1')],
  edges: [
    { from: 'hyp-1', to: 'pe-x', type: 'builds_on', agent_id: '', actor: ada, created_at: at },
    { from: 'hyp-2', to: 'pe-x', type: 'builds_on', agent_id: 'ag-1', actor: ada, created_at: at },
  ],
}

function registered(id: string, text: string, links: Array<{ type: 'builds_on' | 'related' | 'contradicts'; to: string }>, similar: SimilarHypothesis[] = []): RegisteredHypothesis {
  return {
    ...idea(id, text, []), already_existed: false, similar,
    links: links.map(l => ({ type: l.type, hypothesis_id: l.to, text: '', status: '', agent_id: '', actor: ada, created_at: at })),
  }
}

// What reading the graph again answers, when a test changed it behind the view.
let served: HypothesisGraph | null = null

function Harness({ view, graph: start = initial }: { view: 'graph' | 'outline'; graph?: HypothesisGraph }) {
  const [graph, setGraph] = useState(start)
  const [selected, setSelected] = useState(initial.root)
  const mutate = (async (change?: unknown) => {
    if (typeof change === 'function') setGraph(g => change(g))
    else if (served) setGraph(served)
    return undefined
  }) as KeyedMutator<HypothesisGraph>
  const editor = useGraphEditor({ graph, rootLabel: 'Vision benchmark', canWrite: true, mutate, onSelect: setSelected })
  const positions = useMemo(() => layoutGraph(graph), [graph])
  const common = { graph, rootLabel: 'Vision benchmark', selected, hits: new Set<string>(), filters: { status: '', owner: '' }, onSelect: setSelected }
  const resolved = resolveTenant({
    kind: 'user', user_id: 'u1',
    orgs: [{ id: 'org_a', name: 'Ada', role: 'member', created_at: at }],
    projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
  }, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return (
    <TenantContext.Provider value={resolved.tenant}>
    <GraphEditorProvider value={editor}>
    <Expandable label="Hypothesis graph" className="contents">
      <AddIdeaButton />
      <LinkBar />
      <IdeaEditor />
      <AddedNote />
      <output aria-label="selected">{selected}</output>
      <output aria-label="links">{graph.edges.map(e => `${e.from} ${e.type} ${e.to}`).join(', ')}</output>
      {view === 'graph'
        ? <div style={{ width: 1000, height: 800 }}><HypothesisGraphCanvas {...common} positions={positions} focus="" minimap={false} /></div>
        : <HypothesisOutline {...common} />}
      <section aria-label="Side panel">
        {selected !== graph.root && <HypothesisPanel graph={graph} nodeByID={new Map(graph.nodes.map(n => [n.id, n]))} selected={selected} onSelect={setSelected} />}
      </section>
    </Expandable>
    </GraphEditorProvider>
    </TenantContext.Provider>
  )
}

const node = (text: string) => screen.getByText(text, { selector: '.react-flow__node *' })
const nodeBox = (text: string) => node(text).closest('.react-flow__node') as HTMLElement

beforeAll(() => {
  // React Flow measures its pane and nodes; jsdom lays nothing out.
  globalThis.DOMMatrixReadOnly ??= class { m22 = 1; constructor() {} } as unknown as typeof DOMMatrixReadOnly
  Object.defineProperties(HTMLElement.prototype, {
    offsetWidth: { configurable: true, get() { return 1000 } },
    offsetHeight: { configurable: true, get() { return 800 } },
  })
  Element.prototype.scrollIntoView = vi.fn()
  // Edges are drawn from the handles each node declares, and a labelled one measures its text.
  ;(SVGElement.prototype as unknown as SVGGraphicsElement).getBBox = () => ({ x: 0, y: 0, width: 40, height: 12 }) as DOMRect
})

beforeEach(() => {
  served = null
  vi.mocked(registerHumanHypothesis).mockReset()
  vi.mocked(addHumanHypothesisLink).mockReset()
})

async function write(text: string) {
  fireEvent.change(await screen.findByRole('textbox', { name: 'Your idea' }), { target: { value: text } })
}

describe('on the graph', () => {
  it('adds a foundational idea under the objective, drawn in place', async () => {
    vi.mocked(registerHumanHypothesis).mockResolvedValue(registered('hyp-3', 'label smoothing helps', [{ type: 'builds_on', to: 'pe-x' }]))
    render(<Harness view="graph" />)
    fireEvent.click(await screen.findByRole('button', { name: 'Add idea' }))
    const editor = screen.getByRole('group', { name: 'New idea' })
    expect(within(editor).getByText('A new approach to: Vision benchmark')).toBeTruthy()
    await write('label smoothing helps')
    fireEvent.click(within(editor).getByRole('button', { name: 'Add idea' }))
    await waitFor(() => expect(screen.getByLabelText('selected').textContent).toBe('hyp-3'))
    expect(registerHumanHypothesis).toHaveBeenCalledExactlyOnceWith('pe-x', 'label smoothing helps', [{ type: 'builds_on', to: 'pe-x' }])
    expect(node('label smoothing helps')).toBeTruthy()
    expect(screen.queryByRole('group', { name: 'New idea' })).toBeNull()
  })

  it('opens in the whole window, where Escape cancels an idea first and then restores the page', async () => {
    render(<Harness view="graph" />)
    fireEvent.click(await screen.findByRole('button', { name: 'Open in the whole window' }))
    const whole = screen.getByLabelText('Hypothesis graph')
    expect(whole.className).toContain('fixed')
    fireEvent.click(within(whole).getByRole('button', { name: 'Add idea' }))
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(screen.queryByRole('group', { name: 'New idea' })).toBeNull()
    expect(screen.getByLabelText('Hypothesis graph')).toBe(whole)
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(screen.queryByLabelText('Hypothesis graph')).toBeNull()
    expect(screen.getByRole('button', { name: 'Open in the whole window' })).toBeTruthy()
  })

  it('adds an idea under a hypothesis, with an extra link picked by clicking another node', async () => {
    vi.mocked(registerHumanHypothesis).mockResolvedValue(registered('hyp-3', 'a longer warmup', [
      { type: 'builds_on', to: 'hyp-1' }, { type: 'contradicts', to: 'hyp-2' },
    ]))
    render(<Harness view="graph" />)
    fireEvent.click(node('warmup helps'))
    const plus = within(nodeBox('warmup helps')).getByRole('button', { name: 'Add an idea refining this one' })
    expect(plus.style.transform).toBe('scale(calc(1 / var(--zoom, 1)))')
    fireEvent.click(plus)
    const editor = screen.getByRole('group', { name: 'New idea' })
    expect(editor.closest('.react-flow')).toBeNull()
    expect(within(editor).getByText('warmup helps')).toBeTruthy()
    await write('a longer warmup')
    fireEvent.click(within(editor).getByRole('button', { name: '+ Add a link' }))
    fireEvent.click(node('a cosine schedule helps'))
    const added = within(editor).getByRole('listitem', { name: 'Link to a cosine schedule helps' })
    fireEvent.click(within(added).getByRole('button', { name: 'contradicts' }))
    expect(within(added).getByRole('button', { name: 'contradicts' }).getAttribute('aria-pressed')).toBe('true')
    fireEvent.click(within(editor).getByRole('button', { name: 'Add idea' }))
    await waitFor(() => expect(screen.getByLabelText('selected').textContent).toBe('hyp-3'))
    expect(registerHumanHypothesis).toHaveBeenCalledExactlyOnceWith('pe-x', 'a longer warmup', [
      { type: 'builds_on', to: 'hyp-1' }, { type: 'contradicts', to: 'hyp-2' },
    ])
    expect(node('a longer warmup')).toBeTruthy()
  })

  it('moves the target to the node whose + is clicked, keeping the text, and back to the objective from the toolbar', async () => {
    render(<Harness view="graph" />)
    fireEvent.click(within(nodeBox('warmup helps')).getByRole('button', { name: 'Add an idea refining this one' }))
    const editor = screen.getByRole('group', { name: 'New idea' })
    await write('a longer warmup')
    fireEvent.click(within(editor).getByRole('button', { name: '+ Add a link' }))
    fireEvent.click(node('a cosine schedule helps'))
    const links = () => within(editor).getAllByRole('listitem').map(li => li.getAttribute('aria-label'))
    expect(links()).toEqual(['Link to warmup helps', 'Link to a cosine schedule helps'])

    fireEvent.click(within(nodeBox('a cosine schedule helps')).getByRole('button', { name: 'Add an idea refining this one' }))
    expect(links()).toEqual(['Link to a cosine schedule helps'])
    expect(within(editor).getByRole('button', { name: 'refines' }).getAttribute('aria-pressed')).toBe('true')
    expect((within(editor).getByRole('textbox', { name: 'Your idea' }) as HTMLTextAreaElement).value).toBe('a longer warmup')

    fireEvent.click(within(nodeBox('warmup helps')).getByRole('button', { name: 'Add an idea refining this one' }))
    expect(links()).toEqual(['Link to warmup helps'])

    const toolbar = screen.getAllByRole('button', { name: 'Add idea' }).find(b => !editor.contains(b))!
    fireEvent.click(toolbar)
    expect(links()).toEqual(['Link to Objective: Vision benchmark'])
    expect((within(editor).getByRole('textbox', { name: 'Your idea' }) as HTMLTextAreaElement).value).toBe('a longer warmup')
  })

  it('links the objective as a new approach, a hypothesis by a chosen kind, and needs one it refines', async () => {
    vi.mocked(registerHumanHypothesis).mockResolvedValue(registered('hyp-3', 'label smoothing helps', [{ type: 'builds_on', to: 'hyp-1' }]))
    render(<Harness view="graph" />)
    fireEvent.click(await screen.findByRole('button', { name: 'Add idea' }))
    const editor = screen.getByRole('group', { name: 'New idea' })
    await write('label smoothing helps')
    const objective = within(editor).getByRole('listitem', { name: 'Link to Objective: Vision benchmark' })
    expect(within(objective).getByText('A new approach to: Vision benchmark')).toBeTruthy()
    expect(within(objective).getAllByRole('button').map(b => b.getAttribute('aria-label'))).toEqual(['Remove link to Objective: Vision benchmark'])

    fireEvent.click(within(editor).getByRole('button', { name: '+ Add a link' }))
    fireEvent.click(node('warmup helps'))
    const other = within(editor).getByRole('listitem', { name: 'Link to warmup helps' })
    expect(within(other).getAllByRole('button').map(b => b.textContent)).toEqual(['', 'refines', 'related', 'contradicts'])
    expect(within(other).getByText('Mine extends or narrows that idea.')).toBeTruthy()
    fireEvent.click(within(other).getByRole('button', { name: 'related' }))
    expect(within(other).getByText('Mine touches the same thing.')).toBeTruthy()
    fireEvent.click(within(objective).getByRole('button', { name: 'Remove link to Objective: Vision benchmark' }))
    const add = within(editor).getByRole('button', { name: 'Add idea' }) as HTMLButtonElement
    expect(add.disabled).toBe(true)
    expect(within(editor).getByText('It needs a link to the objective or to an idea it refines.')).toBeTruthy()

    fireEvent.click(within(other).getByRole('button', { name: 'refines' }))
    expect(add.disabled).toBe(false)
    fireEvent.click(add)
    await waitFor(() => expect(registerHumanHypothesis).toHaveBeenCalledExactlyOnceWith('pe-x', 'label smoothing helps', [{ type: 'builds_on', to: 'hyp-1' }]))
  })

  it('shows the refusal of the platform', async () => {
    vi.mocked(registerHumanHypothesis).mockRejectedValue(new Error('invalid link: at least one builds_on link is required'))
    render(<Harness view="graph" />)
    fireEvent.click(await screen.findByRole('button', { name: 'Add idea' }))
    await write('label smoothing helps')
    fireEvent.click(within(screen.getByRole('group', { name: 'New idea' })).getByRole('button', { name: 'Add idea' }))
    expect(await screen.findByText('invalid link: at least one builds_on link is required')).toBeTruthy()
  })

  it('links two nodes by clicking the second', async () => {
    vi.mocked(addHumanHypothesisLink).mockResolvedValue([
      { type: 'builds_on', hypothesis_id: 'pe-x', text: '', status: '', agent_id: '', actor: ada, created_at: at },
      { type: 'related', hypothesis_id: 'hyp-2', text: '', status: 'open', agent_id: '', actor: ada, created_at: at },
    ])
    render(<Harness view="graph" />)
    fireEvent.click(node('warmup helps'))
    fireEvent.click(await screen.findByRole('button', { name: 'Link to…' }))
    expect(screen.getByRole('button', { name: 'related' }).getAttribute('aria-pressed')).toBe('true')
    fireEvent.click(node('a cosine schedule helps'))
    await waitFor(() => expect(screen.getByLabelText('links').textContent).toContain('hyp-1 related hyp-2'))
    expect(addHumanHypothesisLink).toHaveBeenCalledExactlyOnceWith('hyp-1', 'related', 'hyp-2')
    expect(screen.queryByRole('button', { name: 'related' })).toBeNull()
    expect(screen.getByLabelText('selected').textContent).toBe('hyp-1')
  })

  it('says why a link cannot be made, and makes none', async () => {
    render(<Harness view="graph" />)
    fireEvent.click(node('a cosine schedule helps'))
    fireEvent.click(await screen.findByRole('button', { name: 'Link to…' }))
    fireEvent.click(screen.getByRole('button', { name: 'refines' }))
    fireEvent.click(node('warmup helps'))
    expect(await screen.findByText('Only the agent that owns this idea links from it.')).toBeTruthy()
    expect(addHumanHypothesisLink).not.toHaveBeenCalled()
  })
})

describe('an idea that reads close to others', () => {
  const close = (id: string, text: string, status: 'open' | 'refuted', reason: 'close_text' | 'refuted_parent') =>
    ({ id, actor: ada, builds_on: ['pe-x'], status, text, reason })

  it('is added at once, then names the ones it reads close to', async () => {
    vi.mocked(registerHumanHypothesis).mockResolvedValue(registered('hyp-3', 'warmup helps a lot', [{ type: 'builds_on', to: 'pe-x' }], [
      close('hyp-1', 'warmup helps', 'open', 'close_text'),
      close('hyp-0', 'warmup hurts', 'refuted', 'refuted_parent'),
    ]))
    render(<Harness view="outline" />)
    fireEvent.click(screen.getByRole('button', { name: 'Add idea' }))
    await write('warmup helps a lot')
    fireEvent.click(within(screen.getByRole('group', { name: 'New idea' })).getByRole('button', { name: 'Add idea' }))
    await waitFor(() => expect(screen.getByLabelText('selected').textContent).toBe('hyp-3'))
    expect(registerHumanHypothesis).toHaveBeenCalledExactlyOnceWith('pe-x', 'warmup helps a lot', [{ type: 'builds_on', to: 'pe-x' }])
    expect(screen.queryByRole('group', { name: 'New idea' })).toBeNull()

    const note = screen.getByRole('note', { name: 'Similar ideas' })
    expect(within(note).getByText('Similar ideas already on the graph:')).toBeTruthy()
    expect(within(note).getByText('Yours refines a refuted idea.')).toBeTruthy()
    fireEvent.click(within(note).getByRole('button', { name: 'warmup helps' }))
    expect(screen.getByLabelText('selected').textContent).toBe('hyp-1')
    fireEvent.click(within(note).getByRole('button', { name: 'Dismiss' }))
    expect(screen.queryByRole('note', { name: 'Similar ideas' })).toBeNull()
  })

  it('says nothing when it reads close to none', async () => {
    vi.mocked(registerHumanHypothesis).mockResolvedValue(registered('hyp-3', 'mixup helps', [{ type: 'builds_on', to: 'pe-x' }]))
    render(<Harness view="outline" />)
    fireEvent.click(screen.getByRole('button', { name: 'Add idea' }))
    await write('mixup helps')
    fireEvent.click(within(screen.getByRole('group', { name: 'New idea' })).getByRole('button', { name: 'Add idea' }))
    await waitFor(() => expect(screen.getByLabelText('selected').textContent).toBe('hyp-3'))
    expect(screen.queryByRole('note', { name: 'Similar ideas' })).toBeNull()
  })
})

describe('in the outline', () => {
  it('adds an idea from the + of a row, listed under it', async () => {
    vi.mocked(registerHumanHypothesis).mockResolvedValue(registered('hyp-3', 'a longer warmup', [{ type: 'builds_on', to: 'hyp-1' }]))
    render(<Harness view="outline" />)
    fireEvent.click(screen.getByRole('button', { name: /warmup helps/ }))
    fireEvent.click(within(screen.getByRole('button', { name: /warmup helps/ }).closest('li')!).getByRole('button', { name: 'Add an idea refining this one' }))
    const editor = screen.getByRole('group', { name: 'New idea' })
    await write('a longer warmup')
    fireEvent.click(within(editor).getByRole('button', { name: 'Add idea' }))
    await waitFor(() => expect(screen.getByLabelText('selected').textContent).toBe('hyp-3'))
    expect(registerHumanHypothesis).toHaveBeenCalledExactlyOnceWith('pe-x', 'a longer warmup', [{ type: 'builds_on', to: 'hyp-1' }])
    const rows = screen.getAllByRole('listitem').map(li => li.textContent)
    expect(rows.findIndex(r => r?.includes('a longer warmup'))).toBe(rows.findIndex(r => r?.includes('warmup helps')) + 1)
  })

  it('links a row to an idea found by its text', async () => {
    vi.mocked(addHumanHypothesisLink).mockResolvedValue([
      { type: 'contradicts', hypothesis_id: 'hyp-2', text: '', status: 'open', agent_id: '', actor: ada, created_at: at },
    ])
    render(<Harness view="outline" />)
    fireEvent.click(screen.getByRole('button', { name: /warmup helps/ }))
    fireEvent.click(screen.getByRole('button', { name: 'Link to…' }))
    fireEvent.click(screen.getByRole('button', { name: 'contradicts' }))
    const search = screen.getByRole('searchbox', { name: 'Find an idea by its text' })
    fireEvent.change(search, { target: { value: 'cosine' } })
    fireEvent.click(within(search.parentElement!).getByRole('button', { name: /a cosine schedule helps/ }))
    await waitFor(() => expect(addHumanHypothesisLink).toHaveBeenCalledExactlyOnceWith('hyp-1', 'contradicts', 'hyp-2'))
  })
})

describe('a person\'s own idea', () => {
  const bob = { kind: 'user' as const, id: 'u2', name: 'Bob' }
  // Bob's idea as the API answers it to a member, and to an owner of the org, who removes it.
  const withBob = (actions: HypothesisAction[]): HypothesisGraph => ({
    ...initial,
    nodes: [...initial.nodes, { ...idea('hyp-4', 'mixup helps', ['pe-x']), actor: bob, actions }],
    edges: [...initial.edges, { from: 'hyp-4', to: 'pe-x', type: 'builds_on', agent_id: '', actor: bob, created_at: at }],
  })
  const panel = () => screen.getByRole('region', { name: 'Side panel' })

  it('is edited inline by its author, and the graph read again', async () => {
    vi.mocked(editHumanHypothesis).mockResolvedValue({} as never)
    served = { ...initial, nodes: initial.nodes.map(n => (n.id === 'hyp-1' ? { ...n, text: 'a short warmup helps' } : n)) }
    render(<Harness view="outline" />)
    fireEvent.click(screen.getByRole('button', { name: /warmup helps/ }))
    fireEvent.click(within(panel()).getByRole('button', { name: 'Edit' }))
    fireEvent.change(within(panel()).getByRole('textbox', { name: 'Text of the idea' }), { target: { value: 'a short warmup helps' } })
    fireEvent.click(within(panel()).getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(within(panel()).getByText('a short warmup helps')).toBeTruthy())
    expect(editHumanHypothesis).toHaveBeenCalledExactlyOnceWith('hyp-1', 'a short warmup helps')
    expect(within(panel()).queryByRole('textbox')).toBeNull()
  })

  it('is removed after a confirmation, and the platform\'s refusal is shown as it says it', async () => {
    vi.mocked(removeHumanHypothesis).mockRejectedValueOnce(new Error('hyp-1 is part of the record: job j1 tested it. Add a comment to it instead'))
    render(<Harness view="outline" />)
    fireEvent.click(screen.getByRole('button', { name: /warmup helps/ }))
    fireEvent.click(within(panel()).getByRole('button', { name: 'Remove' }))
    expect(within(panel()).getByText('Remove this idea and its comments?')).toBeTruthy()
    fireEvent.click(within(panel()).getByRole('button', { name: 'Remove' }))
    expect(await within(panel()).findByText('hyp-1 is part of the record: job j1 tested it. Add a comment to it instead')).toBeTruthy()

    vi.mocked(removeHumanHypothesis).mockResolvedValueOnce(undefined)
    served = { ...initial, nodes: initial.nodes.filter(n => n.id !== 'hyp-1'), edges: initial.edges.filter(e => e.from !== 'hyp-1') }
    fireEvent.click(within(panel()).getByRole('button', { name: 'Remove' }))
    fireEvent.click(within(panel()).getByRole('button', { name: 'Remove' }))
    await waitFor(() => expect(screen.getByLabelText('selected').textContent).toBe(''))
    expect(removeHumanHypothesis).toHaveBeenLastCalledWith('hyp-1')
    expect(screen.queryByRole('button', { name: /warmup helps/ })).toBeNull()
  })

  it('is changed by nobody else: an owner of its org only removes it, and an agent\'s idea takes neither', () => {
    const { unmount } = render(<Harness view="outline" graph={withBob(['link', 'comment'])} />)
    fireEvent.click(screen.getByRole('button', { name: /mixup helps/ }))
    expect(within(panel()).queryByRole('button', { name: 'Edit' })).toBeNull()
    expect(within(panel()).queryByRole('button', { name: 'Remove' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: /a cosine schedule helps/ }))
    expect(within(panel()).queryByRole('button', { name: 'Remove' })).toBeNull()
    unmount()

    render(<Harness view="outline" graph={withBob(['remove', 'link', 'comment'])} />)
    fireEvent.click(screen.getByRole('button', { name: /mixup helps/ }))
    expect(within(panel()).queryByRole('button', { name: 'Edit' })).toBeNull()
    expect(within(panel()).getByRole('button', { name: 'Remove' })).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: /a cosine schedule helps/ }))
    expect(within(panel()).queryByRole('button', { name: 'Remove' })).toBeNull()
  })

  it('is left as it is once another idea links to it', () => {
    render(<Harness view="outline" graph={{
      ...initial,
      nodes: initial.nodes.map(n => (n.id === 'hyp-1' ? { ...n, actions: ['link', 'comment'] } : n)),
      edges: [...initial.edges, { from: 'hyp-2', to: 'hyp-1', type: 'related', agent_id: 'ag-1', actor: ada, created_at: at }],
    }} />)
    fireEvent.click(screen.getByRole('button', { name: /^open warmup helps/ }))
    expect(within(panel()).queryByRole('button', { name: 'Edit' })).toBeNull()
  })
})
