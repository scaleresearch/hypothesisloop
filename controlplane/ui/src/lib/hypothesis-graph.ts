import dagre from '@dagrejs/dagre'
import type { Hypothesis, HypothesisGraph, HypothesisLinkType, HypothesisListItem } from '@/types'

export const NODE_WIDTH = 240
export const NODE_HEIGHT = 92

export const HUMAN_OWNER = 'human'

/** The value the agent filter matches: the registering agent, or HUMAN_OWNER for any person. */
export function ownerKey(h: Hypothesis): string {
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

// Only builds_on edges rank the layout: cross links would pull branches out of their hierarchy.
export function layoutGraph(graph: HypothesisGraph): Map<string, Position> {
  const g = new dagre.graphlib.Graph()
  g.setGraph({ rankdir: 'TB', nodesep: 24, ranksep: 64 })
  g.setDefaultEdgeLabel(() => ({}))
  g.setNode(graph.root, { width: NODE_WIDTH, height: NODE_HEIGHT })
  for (const n of graph.nodes) g.setNode(n.id, { width: NODE_WIDTH, height: NODE_HEIGHT })
  for (const e of graph.edges) {
    if (e.type === 'builds_on') g.setEdge(e.to, e.from)
  }
  dagre.layout(g)
  const positions = new Map<string, Position>()
  for (const id of g.nodes()) {
    const n = g.node(id)
    positions.set(id, { x: n.x - NODE_WIDTH / 2, y: n.y - NODE_HEIGHT / 2 })
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
