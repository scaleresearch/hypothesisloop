import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { useMemo, useState } from 'react'
import type { KeyedMutator } from 'swr'
import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { addHumanHypothesisLink, registerHumanHypothesis } from '@/lib/api'
import type { RegisteredHypothesis } from '@/lib/api'
import { AddIdeaButton, GraphEditorProvider, IdeaEditor, LinkBar, useGraphEditor } from '@/components/graph-edit'
import { HypothesisPanel } from '@/components/hypothesis-panel'
import { HypothesisGraphCanvas } from '@/components/hypothesis-graph'
import { HypothesisOutline } from '@/components/hypothesis-outline'
import { layoutGraph } from '@/lib/hypothesis-graph'
import type { HypothesisGraph, HypothesisGraphNode } from '@/types'

// Its dot pattern reads a size jsdom never lays out.
vi.mock('@xyflow/react', async original => ({ ...await original(), Background: () => null }))
vi.mock('@/lib/api', () => ({ registerHumanHypothesis: vi.fn(), addHumanHypothesisLink: vi.fn() }))
vi.mock('@/components/agent-label', () => ({ AgentLabel: ({ id }: { id: string }) => <span>{id}</span>, useAgentName: () => (id: string) => id }))

const at = '2026-01-01T00:00:00Z'
const ada = { kind: 'user' as const, id: 'u1', name: 'Ada' }

function idea(id: string, text: string, buildsOn: string[], agentID = ''): HypothesisGraphNode {
  return {
    id, text, platform_experiment_id: 'pe-x', status: 'open', agent_id: agentID, actor: ada, status_actor: null, status_changed_at: null,
    created_at: at, builds_on: buildsOn, finding_count: 0, comment_count: 0, child_count: 0,
  }
}

const initial: HypothesisGraph = {
  root: 'pe-x',
  objective: {},
  nodes: [idea('hyp-1', 'warmup helps', ['pe-x']), idea('hyp-2', 'a cosine schedule helps', ['pe-x'], 'ag-1')],
  edges: [
    { from: 'hyp-1', to: 'pe-x', type: 'builds_on', agent_id: '', actor: ada, created_at: at },
    { from: 'hyp-2', to: 'pe-x', type: 'builds_on', agent_id: 'ag-1', actor: ada, created_at: at },
  ],
}

function registered(id: string, text: string, links: Array<{ type: 'builds_on' | 'related' | 'contradicts'; to: string }>): RegisteredHypothesis {
  return {
    ...idea(id, text, []), already_existed: false,
    links: links.map(l => ({ type: l.type, hypothesis_id: l.to, text: '', status: '', agent_id: '', actor: ada, created_at: at })),
  }
}

function Harness({ view }: { view: 'graph' | 'outline' }) {
  const [graph, setGraph] = useState(initial)
  const [selected, setSelected] = useState(initial.root)
  const mutate = (async (change?: unknown) => {
    if (typeof change === 'function') setGraph(g => change(g))
    return undefined
  }) as KeyedMutator<HypothesisGraph>
  const editor = useGraphEditor({ graph, rootLabel: 'Vision benchmark', canWrite: true, mutate, onSelect: setSelected })
  const positions = useMemo(() => layoutGraph(graph), [graph])
  const common = { graph, rootLabel: 'Vision benchmark', selected, hits: new Set<string>(), filters: { status: '', owner: '' }, onSelect: setSelected }
  return (
    <GraphEditorProvider value={editor}>
      <AddIdeaButton />
      <LinkBar />
      <IdeaEditor />
      <output aria-label="selected">{selected}</output>
      <output aria-label="links">{graph.edges.map(e => `${e.from} ${e.type} ${e.to}`).join(', ')}</output>
      {view === 'graph'
        ? <div style={{ width: 1000, height: 800 }}><HypothesisGraphCanvas {...common} positions={positions} focus="" minimap={false} /></div>
        : <HypothesisOutline {...common} />}
      {selected !== graph.root && <HypothesisPanel graph={graph} nodeByID={new Map(graph.nodes.map(n => [n.id, n]))} selected={selected} onSelect={setSelected} />}
    </GraphEditorProvider>
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
})

beforeEach(() => {
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
    expect(registerHumanHypothesis).toHaveBeenCalledExactlyOnceWith('pe-x', 'label smoothing helps', [{ type: 'builds_on', to: 'pe-x' }], [])
    expect(node('label smoothing helps')).toBeTruthy()
    expect(screen.queryByRole('group', { name: 'New idea' })).toBeNull()
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
    expect(screen.queryByRole('button', { name: 'Add an idea refining this one' })).toBeNull()
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
    ], [])
    expect(node('a longer warmup')).toBeTruthy()
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
    await waitFor(() => expect(registerHumanHypothesis).toHaveBeenCalledExactlyOnceWith('pe-x', 'label smoothing helps', [{ type: 'builds_on', to: 'hyp-1' }], []))
  })

  it('shows the refusal of the platform', async () => {
    vi.mocked(registerHumanHypothesis).mockRejectedValue(new Error('invalid link: at least one builds_on link is required'))
    render(<Harness view="graph" />)
    fireEvent.click(await screen.findByRole('button', { name: 'Add idea' }))
    await write('label smoothing helps')
    fireEvent.click(screen.getByRole('button', { name: 'Add idea' }))
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
  const similar = [
    { id: 'hyp-1', status: 'open' as const, text: 'warmup helps', reason: 'close_text' as const },
    { id: 'hyp-2', status: 'open' as const, text: 'a cosine schedule helps', reason: 'close_text' as const },
  ]

  it('is linked to one and kept distinct from the other', async () => {
    vi.mocked(registerHumanHypothesis)
      .mockResolvedValueOnce({ similar })
      .mockResolvedValueOnce(registered('hyp-3', 'warmup helps a lot', [{ type: 'builds_on', to: 'pe-x' }, { type: 'related', to: 'hyp-1' }]))
    render(<Harness view="outline" />)
    fireEvent.click(screen.getByRole('button', { name: 'Add idea' }))
    await write('warmup helps a lot')
    const editor = screen.getByRole('group', { name: 'New idea' })
    fireEvent.click(within(editor).getByRole('button', { name: 'Add idea' }))
    const first = await within(editor).findByRole('group', { name: 'Choice for warmup helps' })
    expect((within(editor).getByRole('button', { name: 'Add idea' }) as HTMLButtonElement).disabled).toBe(true)
    fireEvent.click(within(first).getByRole('button', { name: 'Link to it' }))
    fireEvent.click(within(within(editor).getByRole('group', { name: 'Choice for a cosine schedule helps' })).getByRole('button', { name: 'Keep mine distinct' }))
    fireEvent.click(within(editor).getByRole('button', { name: 'Add idea' }))
    await waitFor(() => expect(screen.getByLabelText('selected').textContent).toBe('hyp-3'))
    expect(vi.mocked(registerHumanHypothesis).mock.calls[1]).toEqual(['pe-x', 'warmup helps a lot', [
      { type: 'builds_on', to: 'pe-x' }, { type: 'related', to: 'hyp-1' },
    ], ['hyp-2']])
  })

  it('is kept distinct from all in one step, or dropped for the one already there', async () => {
    vi.mocked(registerHumanHypothesis).mockResolvedValue({ similar })
    render(<Harness view="outline" />)
    fireEvent.click(screen.getByRole('button', { name: 'Add idea' }))
    await write('warmup helps a lot')
    fireEvent.click(screen.getByRole('button', { name: 'Add idea' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Keep mine distinct from all' }))
    await waitFor(() => expect(registerHumanHypothesis).toHaveBeenCalledTimes(2))
    expect(vi.mocked(registerHumanHypothesis).mock.calls[1][3]).toEqual(['hyp-1', 'hyp-2'])

    fireEvent.click(screen.getAllByRole('button', { name: 'Show it' })[1])
    expect(screen.getByLabelText('selected').textContent).toBe('hyp-2')
    expect(screen.queryByRole('group', { name: 'New idea' })).toBeNull()
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
    expect(registerHumanHypothesis).toHaveBeenCalledExactlyOnceWith('pe-x', 'a longer warmup', [{ type: 'builds_on', to: 'hyp-1' }], [])
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
