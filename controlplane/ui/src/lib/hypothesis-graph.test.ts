import { describe, expect, it } from 'vitest'
import { COLUMN, ROW, extendLayout, layoutGraph, linkPlanner, objectiveFinding } from '@/lib/hypothesis-graph'
import type { HypothesisGraph, HypothesisGraphNode } from '@/types'

const at = '2026-01-01T00:00:00Z'
const actor = { kind: 'user' as const, id: 'u1', name: 'Ada' }

function idea(id: string, buildsOn: string[], agentID = '', created = at): HypothesisGraphNode {
  return {
    id, text: id, platform_experiment_id: 'pe', status: 'open', agent_id: agentID, actor, status_actor: null, status_changed_at: null,
    created_at: created, builds_on: buildsOn, finding_count: 0, comment_count: 0, child_count: 0,
  }
}

function graphOf(nodes: HypothesisGraphNode[], cross: Array<[string, 'related' | 'contradicts', string]> = []): HypothesisGraph {
  return {
    root: 'pe',
    objective: {},
    nodes,
    edges: [
      ...nodes.flatMap(n => n.builds_on.map(to => ({ from: n.id, to, type: 'builds_on' as const, agent_id: n.agent_id, actor, created_at: at }))),
      ...cross.map(([from, type, to]) => ({ from, to, type, agent_id: '', actor, created_at: at })),
    ],
  }
}

describe('a link a person makes', () => {
  const graph = graphOf([idea('a', ['pe']), idea('b', ['a']), idea('c', ['pe'], 'ag-1'), idea('d', ['pe'], 'ag-2')], [['a', 'related', 'd']])
  const plan = linkPlanner(graph)

  it('leaves an idea no agent owns as asked', () => {
    expect(plan('a', 'c', 'builds_on')).toEqual({ from: 'a', to: 'c' })
    expect(plan('b', 'c', 'contradicts')).toEqual({ from: 'b', to: 'c' })
  })

  it('from an agent\'s idea, reads both ways and is stored on the free end', () => {
    expect(plan('c', 'a', 'related')).toEqual({ from: 'a', to: 'c' })
    expect(plan('c', 'a', 'builds_on')).toEqual({ refused: 'Only the agent that owns this idea links from it.' })
    expect(plan('c', 'd', 'related')).toEqual({ refused: 'Only the agent that owns this idea links from it.' })
  })

  it('makes no cycle, no second copy, and nothing but builds on to the objective', () => {
    expect(plan('a', 'b', 'builds_on')).toEqual({ refused: 'That idea already refines this one.' })
    expect(plan('d', 'a', 'related')).toEqual({ refused: 'These two ideas are already linked this way.' })
    expect(plan('a', 'pe', 'related')).toEqual({ refused: 'An idea links to the objective only as a new approach to it.' })
    expect(plan('a', 'a', 'related')).toEqual({ refused: 'An idea does not link to itself.' })
  })
})

describe('a node that joins the graph', () => {
  it('leaves every node drawn where it was and takes the free column nearest its parent, a row under it', () => {
    const before = graphOf([idea('a', ['pe']), idea('b', ['pe']), idea('a1', ['a'])])
    const laid = layoutGraph(before)
    const after = extendLayout(graphOf([...before.nodes, idea('a2', ['a'], '', '2026-02-01T00:00:00Z')]), laid)
    laid.forEach((p, id) => expect(after.get(id)).toEqual(p))
    const a = laid.get('a')!
    expect(after.get('a2')).toEqual({ x: a.x + COLUMN, y: a.y + ROW })
  })

  it('hangs under its deepest parent', () => {
    const before = graphOf([idea('a', ['pe']), idea('a1', ['a'])])
    const laid = layoutGraph(before)
    const after = extendLayout(graphOf([...before.nodes, idea('x', ['pe', 'a1'])]), laid)
    expect(after.get('x')!.y).toBe(laid.get('a1')!.y + ROW)
  })
})

describe('objectiveFinding', () => {
  const metric = { name: 'ratio', value: 1.5, basis: 'raw' }
  const reference = { id: 'f2', hypothesis_id: 'pe', experiment_id: 'job-ref', agent_id: 'b', actor, summary: 'Reference', created_at: at }

  it('shows the baseline job write-up over a newer one on the objective', () => {
    expect(objectiveFinding({ finding: reference, best_job: { id: 'job-control', metric, summary: 'Control' } }))
      .toEqual({ job: 'job-control', summary: 'Control', baseline: true })
  })

  it('names the newest finding as such while the baseline job has none', () => {
    expect(objectiveFinding({ finding: reference, best_job: { id: 'job-control', metric } }))
      .toEqual({ job: 'job-ref', summary: 'Reference', baseline: false })
  })
})
