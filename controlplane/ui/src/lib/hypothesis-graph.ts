import type { Hypothesis, HypothesisGraph, HypothesisLinkType, HypothesisListItem } from '@/types'

export const NODE_WIDTH = 240
export const NODE_HEIGHT = 92

export const HUMAN_OWNER = 'human'

/** The value the agent filter matches: the registering agent, or HUMAN_OWNER for any person. */
function ownerKey(h: Hypothesis): string {
  return h.source === 'human' ? HUMAN_OWNER : h.agent_id
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

const COLUMN = NODE_WIDTH + 24
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
    { label: 'Builds on', items: out.filter(l => l.type === 'builds_on') },
    { label: 'Built on by', items: incoming.filter(l => l.type === 'builds_on') },
    { label: 'Contradicts', items: [...out, ...incoming].filter(l => l.type === 'contradicts') },
    { label: 'Related', items: [...out, ...incoming].filter(l => l.type === 'related') },
  ].filter(g => g.items.length > 0)
}

export function graphHref(platformExperimentID: string, focus?: string): string {
  const params = new URLSearchParams({ pe: platformExperimentID })
  if (focus) params.set('focus', focus)
  return `/hypotheses?${params}`
}
