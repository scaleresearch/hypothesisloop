import type { Evidence, Hypothesis, HypothesisGraph, HypothesisLinkType, HypothesisListItem } from '@/types'
import { experimentPath, withQuery } from '@/lib/routes'

export const NODE_WIDTH = 280
export const NODE_HEIGHT = 112

export const HUMAN_OWNER = 'human'

/** The value the agent filter matches: the agent whose hypothesis it is, or HUMAN_OWNER for one a person registered as their own. */
function ownerKey(h: Hypothesis): string {
  return h.agent_id || (h.actor.kind === 'user' ? HUMAN_OWNER : '')
}

export interface GraphFilters {
  status: string
  owner: string
}

export function matchesFilters(h: HypothesisListItem, f: GraphFilters): boolean {
  return (!f.status || h.status === f.status) && (!f.owner || ownerKey(h) === f.owner)
}

export interface Position {
  x: number
  y: number
}

export const COLUMN = NODE_WIDTH + 24
export const ROW = NODE_HEIGHT + 64

interface Contour {
  /** Leftmost and rightmost column per rank, relative to the subtree's root. */
  left: number[]
  right: number[]
}

// Linear in nodes times depth, unlike a general layered layout, so thousands of nodes lay out
// instantly. A node's rank is its longest builds_on path from the objective, keeping every parent
// above its children. It hangs under its deepest parent, each subtree packed against its left
// siblings as tightly as their per-rank extents allow, and a parent is centred over its children.
// A node not reached from the objective (a builds_on cycle) gets no position and is not drawn.
export function layoutGraph(graph: HypothesisGraph): Map<string, Position> {
  const children = new Map<string, string[]>()
  const pending = new Map<string, number>()
  for (const n of graph.nodes) {
    pending.set(n.id, n.builds_on.length)
    for (const p of n.builds_on) {
      if (!children.has(p)) children.set(p, [])
      children.get(p)!.push(n.id)
    }
  }
  const rank = new Map([[graph.root, 0]])
  const treeParent = new Map<string, string>()
  const order = [graph.root]
  for (let i = 0; i < order.length; i++) {
    const id = order[i]
    for (const c of children.get(id) ?? []) {
      if (!rank.has(c) || rank.get(id)! + 1 > rank.get(c)!) {
        rank.set(c, rank.get(id)! + 1)
        treeParent.set(c, id)
      }
      pending.set(c, pending.get(c)! - 1)
      if (pending.get(c) === 0) order.push(c)
    }
  }

  const treeChildren = new Map<string, string[]>()
  for (const n of [...graph.nodes].sort((a, b) => a.created_at.localeCompare(b.created_at))) {
    const p = treeParent.get(n.id)
    if (p === undefined || pending.get(n.id) !== 0) continue
    if (!treeChildren.has(p)) treeChildren.set(p, [])
    treeChildren.get(p)!.push(n.id)
  }

  const offset = new Map<string, number>()
  function place(id: string): Contour {
    const kids = treeChildren.get(id) ?? []
    const left: number[] = []
    const right: number[] = []
    for (const kid of kids) {
      const c = place(kid)
      let shift = 0
      c.left.forEach((l, r) => {
        if (right[r] !== undefined) shift = Math.max(shift, right[r] + 1 - l)
      })
      offset.set(kid, shift)
      c.left.forEach((l, r) => { left[r] ??= l + shift })
      c.right.forEach((x, r) => { right[r] = x + shift })
    }
    const centre = kids.length === 0 ? 0 : (offset.get(kids[0])! + offset.get(kids[kids.length - 1])!) / 2
    for (const kid of kids) offset.set(kid, offset.get(kid)! - centre)
    const own = { left: left.map(l => l - centre), right: right.map(x => x - centre) }
    own.left[rank.get(id)!] = 0
    own.right[rank.get(id)!] = 0
    return own
  }
  place(graph.root)

  const positions = new Map([[graph.root, { x: 0, y: 0 }]])
  for (const id of order) {
    for (const kid of treeChildren.get(id) ?? []) {
      positions.set(kid, { x: positions.get(id)!.x + offset.get(kid)! * COLUMN, y: rank.get(kid)! * ROW })
    }
  }
  return positions
}

// Keeps every node already placed where it is and puts each new one a row under its deepest
// parent, in the free column nearest to it, so a node that joins never moves the rest.
export function extendLayout(graph: HypothesisGraph, previous: Map<string, Position>): Map<string, Position> {
  const positions = new Map([[graph.root, previous.get(graph.root) ?? { x: 0, y: 0 }]])
  for (const n of graph.nodes) {
    const p = previous.get(n.id)
    if (p) positions.set(n.id, p)
  }
  let pending = graph.nodes.filter(n => !positions.has(n.id)).sort((a, b) => a.created_at.localeCompare(b.created_at))
  while (pending.length > 0) {
    const waiting = pending.filter(n => n.builds_on.length === 0 || n.builds_on.some(p => !positions.has(p)))
    if (waiting.length === pending.length) break
    for (const n of pending) {
      if (waiting.includes(n)) continue
      const parent = n.builds_on.map(p => positions.get(p)!).reduce((a, b) => (b.y > a.y ? b : a))
      const y = parent.y + ROW
      const taken = Array.from(positions.values()).filter(p => p.y === y).map(p => p.x)
      for (let k = 0; ; k++) {
        const x = [parent.x + k * COLUMN, parent.x - k * COLUMN].find(c => taken.every(t => Math.abs(t - c) >= COLUMN))
        if (x !== undefined) {
          positions.set(n.id, { x, y })
          break
        }
      }
    }
    pending = waiting
  }
  return positions
}

export const LINK_KINDS: HypothesisLinkType[] = ['builds_on', 'related', 'contradicts']

/** Each kind of link in plain words, read as "this idea <label> that one", the same in every view. */
export const LINK_KIND_LABELS: Record<HypothesisLinkType, string> = {
  builds_on: 'refines',
  related: 'related',
  contradicts: 'contradicts',
}

/** What each kind of link says about the idea it leaves. */
export const LINK_KIND_HINTS: Record<HypothesisLinkType, string> = {
  builds_on: 'extends or narrows that idea',
  related: 'touches the same thing',
  contradicts: 'expects the opposite outcome',
}

/** The kinds of link a hypothesis may take to `to`: the objective is only built on. */
export function allowedLinkKinds(graph: HypothesisGraph, to: string): HypothesisLinkType[] {
  return to === graph.root ? ['builds_on'] : LINK_KINDS
}

export type LinkPlan = { from: string; to: string } | { refused: string }

// What the API takes from a person linking `from` to `to`: a link is stored on the idea it leaves,
// and only an idea no agent owns takes one from a person. A related or contradicts link reads the
// same both ways, so from an agent's idea it is stored on the other end when that one is free.
export function linkPlanner(graph: HypothesisGraph): (from: string, to: string, kind: HypothesisLinkType) => LinkPlan {
  const byID = new Map(graph.nodes.map(n => [n.id, n]))
  const edges = new Set(graph.edges.map(e => `${e.type}:${e.from}:${e.to}`))
  const children = new Map<string, string[]>()
  for (const n of graph.nodes) {
    for (const p of n.builds_on) children.set(p, [...(children.get(p) ?? []), n.id])
  }
  let below: { of: string; ids: Set<string> } | undefined
  function descendants(id: string): Set<string> {
    if (below?.of === id) return below.ids
    const ids = new Set<string>()
    const stack = [...(children.get(id) ?? [])]
    while (stack.length > 0) {
      const c = stack.pop()!
      if (ids.has(c)) continue
      ids.add(c)
      stack.push(...(children.get(c) ?? []))
    }
    below = { of: id, ids }
    return ids
  }
  return (from, to, kind) => {
    const source = byID.get(from)
    if (!source) return { refused: 'Links start at an idea.' }
    if (from === to) return { refused: 'An idea does not link to itself.' }
    if (to === graph.root && kind !== 'builds_on') return { refused: 'An idea links to the objective only as a new approach to it.' }
    if (to !== graph.root && !byID.has(to)) return { refused: 'That idea is not in this graph.' }
    if (edges.has(`${kind}:${from}:${to}`) || (kind !== 'builds_on' && edges.has(`${kind}:${to}:${from}`))) {
      return { refused: 'These two ideas are already linked this way.' }
    }
    if (kind === 'builds_on' && descendants(from).has(to)) return { refused: 'That idea already refines this one.' }
    if (!source.agent_id) return { from, to }
    if (kind !== 'builds_on' && to !== graph.root && !byID.get(to)!.agent_id) return { from: to, to: from }
    return { refused: 'Only the agent that owns this idea links from it.' }
  }
}

type GraphEdge = HypothesisGraph['edges'][number]

/** The graph with a just-registered idea in it, drawn before the next read brings it. */
export function withHypothesis(graph: HypothesisGraph, node: HypothesisGraph['nodes'][number], edges: GraphEdge[]): HypothesisGraph {
  if (graph.nodes.some(n => n.id === node.id)) return graph
  const parents = new Set(node.builds_on)
  return {
    ...graph,
    nodes: [...graph.nodes.map(n => (parents.has(n.id) ? { ...n, child_count: n.child_count + 1 } : n)), node],
    edges: [...graph.edges, ...edges],
  }
}

export function withEdge(graph: HypothesisGraph, edge: GraphEdge): HypothesisGraph {
  if (graph.edges.some(e => e.type === edge.type && e.from === edge.from && e.to === edge.to)) return graph
  return {
    ...graph,
    nodes: edge.type === 'builds_on'
      ? graph.nodes.map(n => (n.id === edge.from ? { ...n, builds_on: [...n.builds_on, edge.to] } : n.id === edge.to ? { ...n, child_count: n.child_count + 1 } : n))
      : graph.nodes,
    edges: [...graph.edges, edge],
  }
}

/** The ideas whose text holds every word of the query, for picking one without a round trip. */
export function findIdeas(graph: HypothesisGraph, query: string): HypothesisListItem[] {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean)
  return graph.nodes.filter(n => {
    const text = n.text.toLowerCase()
    return words.every(w => text.includes(w))
  })
}

export interface OutlineRow {
  node: HypothesisListItem
  depth: number
  /** builds_on targets beyond the one this row is nested under. */
  alsoBuildsOn: string[]
}

// A DAG flattened to a tree: each hypothesis sits once, under its first builds_on target, so the
// outline stays linear in size however many ideas combine.
export function outlineRows(graph: HypothesisGraph): OutlineRow[] {
  const children = new Map<string, HypothesisListItem[]>()
  for (const n of graph.nodes) {
    const parent = n.builds_on[0]
    children.set(parent, [...(children.get(parent) ?? []), n])
  }
  const rows: OutlineRow[] = []
  function visit(parent: string, depth: number) {
    const kids = [...(children.get(parent) ?? [])].sort((a, b) => a.created_at.localeCompare(b.created_at))
    for (const node of kids) {
      rows.push({ node, depth, alsoBuildsOn: node.builds_on.slice(1) })
      visit(node.id, depth + 1)
    }
  }
  visit(graph.root, 0)
  return rows
}

export interface LinkGroup<T> {
  label: string
  items: T[]
}

// Contradiction and relatedness are symmetric claims, so their two directions share a group.
export function linkGroups<T extends { type: HypothesisLinkType }>(out: T[], incoming: T[]): LinkGroup<T>[] {
  return [
    { label: 'Refines', items: out.filter(l => l.type === 'builds_on') },
    { label: 'Refined by', items: incoming.filter(l => l.type === 'builds_on') },
    { label: 'Contradicts', items: [...out, ...incoming].filter(l => l.type === 'contradicts') },
    { label: 'Related', items: [...out, ...incoming].filter(l => l.type === 'related') },
  ].filter(g => g.items.length > 0)
}

export function graphHref(platformExperimentID: string, focus?: string): string {
  return withQuery(experimentPath(platformExperimentID, 'hypotheses'), { focus })
}

/**
 * The write-up to show for the objective: that of the baseline job, else the newest filed on the
 * objective, which may be a reference job's.
 */
export function objectiveFinding(objective: Evidence): { job: string; summary: string; baseline: boolean } | null {
  const { best_job: best, finding } = objective
  if (best?.summary) return { job: best.id, summary: best.summary, baseline: true }
  if (finding) return { job: finding.experiment_id, summary: finding.summary, baseline: finding.experiment_id === best?.id }
  return null
}
