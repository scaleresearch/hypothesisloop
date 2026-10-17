import type { GraphNode, HypothesisGraph, HypothesisStatus, LinkType } from '@/types/api'

// Deterministic, so every run draws the same graph for a given size.
function random(seed: number) {
  return () => {
    seed = (seed * 1664525 + 1013904223) % 4294967296
    return seed / 4294967296
  }
}

const STATUSES: HypothesisStatus[] = ['open', 'open', 'open', 'confirmed', 'refuted', 'inconclusive']
const WORDS = 'raise lower the learning rate warmup schedule batch size weight decay dropout attention heads layer norm residual scaling optimizer momentum cosine decay tokenizer vocabulary data mixture curriculum augmentation'.split(' ')

export function fixtureGraph(n: number, seed = 1): HypothesisGraph {
  const rand = random(seed)
  const pick = <T,>(xs: T[]) => xs[Math.floor(rand() * xs.length)]
  const root = 'pe-bench'
  const actor = { kind: 'agent' as const, id: 'agent-1', name: 'agent-1' }
  const nodes: GraphNode[] = []
  for (let i = 0; i < n; i++) {
    const parent = i < 6 || rand() < 0.05 ? root : nodes[Math.floor(rand() * i)].id
    const builds_on = [parent]
    if (i > 10 && rand() < 0.08) {
      const other = nodes[Math.floor(rand() * i)].id
      if (other !== parent) builds_on.push(other)
    }
    const agent = `agent-${1 + Math.floor(rand() * 6)}`
    nodes.push({
      id: `h-${i}`,
      agent_id: agent,
      actor: { ...actor, id: agent, name: agent },
      platform_experiment_id: root,
      text: Array.from({ length: 12 + Math.floor(rand() * 20) }, () => pick(WORDS)).join(' '),
      status: pick(STATUSES),
      created_at: new Date(Date.UTC(2026, 0, 1) + i * 60_000).toISOString(),
      status_changed_at: null,
      builds_on,
      child_count: 0,
      finding_count: Math.floor(rand() * 4),
      comment_count: Math.floor(rand() * 3),
      actions: [],
      best_job: rand() < 0.4 ? { id: `job-${i}`, metric: { name: 'val_loss', value: 2 + rand(), basis: 'min' } } : undefined,
    })
  }
  const edges: HypothesisGraph['edges'] = nodes.flatMap(h => h.builds_on.map(to => ({
    from: h.id, to, type: 'builds_on' as LinkType, agent_id: h.agent_id, actor: h.actor, created_at: h.created_at,
  })))
  for (let i = 0; i < n / 5; i++) {
    const a = pick(nodes)
    const b = pick(nodes)
    if (a.id !== b.id) edges.push({ from: a.id, to: b.id, type: rand() < 0.5 ? 'related' : 'contradicts', agent_id: a.agent_id, actor: a.actor, created_at: a.created_at })
  }
  return { root, objective: { comment_count: 0, comments: [] }, nodes, edges }
}
