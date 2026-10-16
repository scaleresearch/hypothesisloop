// Seeds a project with platform experiments whose hypothesis graphs agents grew: through the API
// as a person and the agents that person registered, the path a real run takes. Run against the
// graph-1k stack (stack.sh up), with its env:
//   eval "$(bash localdev/graph-1k/stack.sh env)" && node localdev/graph-1k/seed.mjs > /tmp/graph-1k/seed.json
// Prints one JSON line naming the person, each experiment made and its ideas. Verdicts need jobs
// that finished, so results.mjs runs those, judges and closes the experiments marked close.
import { execFileSync } from 'node:child_process'
import { join } from 'node:path'

export const READER = { id: '8a1f3c2e-0000-4000-8000-000000000001', name: 'Graph Reader' }

const env = name => {
  const value = process.env[name]
  if (!value) throw new Error(`${name} is not set: eval "$(bash localdev/graph-1k/stack.sh env)" first`)
  return value
}

export async function tokenOf(user) {
  const res = await fetch(`${env('IDP_URL')}/test/token`, { method: 'POST', body: JSON.stringify({ user }) })
  if (!res.ok) throw new Error(`the identity provider answered ${res.status}`)
  return (await res.json()).access_token
}

export function client(token) {
  return async function call(method, path, body, accept = []) {
    const res = await fetch(`${env('API_URL')}${path}`, {
      method,
      headers: { Authorization: `Bearer ${token}`, ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    const text = await res.text()
    const value = text ? JSON.parse(text) : null
    if (!res.ok && !accept.includes(res.status)) throw new Error(`${method} ${path} answered ${res.status}: ${text.slice(0, 400)}`)
    return { status: res.status, value }
  }
}

// A small deterministic generator, so a rerun grows graphs of the same shape.
export function random(seed) {
  let s = seed >>> 0
  const next = () => {
    s = (s + 0x6d2b79f5) >>> 0
    let t = s
    t = Math.imul(t ^ (t >>> 15), t | 1)
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61)
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
  const pick = list => list[Math.floor(next() * list.length)]
  return { next, pick }
}

const CHANGES = ['raising', 'lowering', 'halving', 'doubling', 'annealing', 'warming up', 'clipping', 'freezing', 'sharing', 'decoupling', 'pruning', 'quantizing', 'distilling', 'regularizing', 'normalizing', 'reordering', 'mixing', 'resampling']
const KNOBS = ['the learning rate', 'the batch size', 'weight decay', 'dropout', 'the warmup length', 'label smoothing', 'gradient clipping', 'the attention heads', 'the MLP width', 'the patch size', 'the tokenizer vocabulary', 'the EMA decay', 'the data mixture', 'the curriculum', 'the optimizer betas', 'the positional encoding', 'the layer norm placement', 'the auxiliary loss', 'the augmentation strength', 'the cosine schedule floor']
const EFFECTS = ['improves val accuracy', 'speeds up convergence', 'stabilises late training', 'reduces overfitting on the small split', 'closes the gap to the baseline', 'removes the loss spikes', 'helps only at long horizons', 'hurts calibration', 'trades accuracy for throughput', 'matters more than model size here']
const BECAUSE = [
  'because the gradient noise scale measured in the baseline run is far below the batch size',
  'since the early layers saturate within the first thousand steps and stop learning',
  'given that the validation curve flattens while training loss keeps falling',
  'as the attention entropy collapses in the last third of training',
  'which the per-class error breakdown of the last three jobs points to',
  'because most of the remaining errors are on rare classes the sampler under-represents',
]
const DETAILS = [
  'Run it at three seeds against the control and report the spread, not only the mean.',
  'Keep every other setting at the baseline so the comparison isolates this one change.',
  'If it holds, the next step is to check whether it survives the longer schedule of stage two.',
  'Watch the throughput too: a gain that costs a third of the step rate does not pay for itself within the budget.',
  'The control job of the parent idea is the reference; compare against it rather than the original baseline.',
]

function claim(rng, parentText) {
  const short = rng.next() < 0.3
  const base = `${rng.pick(CHANGES).replace(/^./, c => c.toUpperCase())} ${rng.pick(KNOBS)} ${rng.pick(EFFECTS)}`
  if (short) return base
  const parts = [base, rng.pick(BECAUSE)]
  if (parentText && rng.next() < 0.4) parts.push(`, refining "${parentText.slice(0, 60)}"`)
  let text = parts.join(' ')
  if (rng.next() < 0.35) text += `. ${rng.pick(DETAILS)}`
  if (rng.next() < 0.15) text += ` ${rng.pick(DETAILS)}`
  return text
}

// Registers one idea as its agent would, telling it apart from those the platform says read alike.
async function register(call, peID, agentID, text, links) {
  let distinct = []
  for (let attempt = 0; attempt < 5; attempt++) {
    const { status, value } = await call('POST', '/hypotheses', {
      platform_experiment_id: peID, text, links, distinct_from: distinct, ...(agentID ? { agent_id: agentID } : {}),
    }, [409, 422])
    if (status < 300) return value
    const similar = value?.similar ?? value?.errors?.flatMap(e => e.value?.similar ?? []) ?? []
    if (similar.length === 0) throw new Error(`POST /hypotheses refused "${text}": ${JSON.stringify(value).slice(0, 400)}`)
    distinct = [...new Set([...distinct, ...similar.map(s => s.id)])]
  }
  throw new Error(`"${text}" kept reading close to others after five tries`)
}

export async function createExperiment(call, projectID, name, agents) {
  const repository = `${name.toLowerCase().replace(/[^a-z0-9]+/g, '-')}-${Date.now().toString(36)}.git`
  execFileSync('git', ['init', '-q', '--bare', join(env('STATE'), 'git', repository)])
  const { value: pe } = await call('POST', '/platform-experiments', {
    project_id: projectID,
    name,
    description: `${name}: find the change to the training recipe that most improves validation accuracy within the budget.`,
    budget_accelerator_hours: 200,
    max_agents: agents.length,
    metrics: [{ key: 'val_accuracy', direction: 'maximize' }],
    duration_hours: 7 * 24,
    repository,
  })
  for (const agent of agents) {
    await call('POST', '/agents', { id: agent, name: agent })
    await call('POST', `/platform-experiments/${pe.id}/agents`, { agent_id: agent, quota_tier: 'guaranteed' })
  }
  return pe
}

// One idea of an agent of the experiment: under the objective, under one idea or two, now and then
// related to or contradicting another. Parents are drawn favouring recent and shallow ideas, so the
// graph grows a few wide approaches with deep chains under the promising ones.
export async function addIdea(call, rng, pe, agents, placed) {
  const agent = rng.next() < 0.03 ? '' : rng.pick(agents)
  const links = []
  let parent = null
  if (placed.length < 12 || rng.next() < 0.06) {
    links.push({ type: 'builds_on', to: pe.id })
  } else {
    const recent = placed.slice(-Math.max(40, Math.floor(placed.length / 3)))
    parent = rng.next() < 0.7 ? rng.pick(recent) : rng.pick(placed)
    if (parent.depth > 9) parent = rng.pick(placed.filter(p => p.depth < 4))
    links.push({ type: 'builds_on', to: parent.id })
    if (rng.next() < 0.1) {
      const other = rng.pick(placed)
      if (other.id !== parent.id) links.push({ type: 'builds_on', to: other.id })
    }
    if (rng.next() < 0.08) links.push({ type: 'related', to: rng.pick(placed).id })
    if (rng.next() < 0.04) links.push({ type: 'contradicts', to: rng.pick(placed).id })
  }
  const unique = links.filter((l, i) => links.findIndex(m => m.type === l.type && m.to === l.to) === i)
  const text = claim(rng, parent?.text)
  const h = await register(call, pe.id, agent, text, unique)
  const depths = unique.filter(l => l.type === 'builds_on').map(l => (l.to === pe.id ? 0 : placed.find(p => p.id === l.to).depth))
  const idea = { id: h.id, agent, text, depth: Math.max(...depths) + 1 }
  placed.push(idea)
  return idea
}

export async function comment(call, rng, idea) {
  const n = rng.next() < 0.2 ? 1 + Math.floor(rng.next() * 4) : 0
  for (let i = 0; i < n; i++) {
    await call('POST', `/hypotheses/${idea.id}/comments`, { text: rng.pick(DETAILS), ...(idea.agent ? { agent_id: idea.agent } : {}) })
  }
}

async function grow(call, rng, pe, agents, count) {
  const placed = []
  for (let i = 0; i < count; i++) await addIdea(call, rng, pe, agents, placed)
  // Comments do not change the graph's shape, so they go in parallel.
  const queue = [...placed]
  await Promise.all(Array.from({ length: 8 }, async () => {
    for (let idea; (idea = queue.shift());) await comment(call, rng, idea)
  }))
  return placed
}

const PLAN = [
  { name: 'Vision transformer recipe', ideas: 1000, agents: 8, close: false },
  { name: 'Tokenizer ablations', ideas: 180, agents: 4, close: true },
  { name: 'Optimizer sweep', ideas: 45, agents: 3, close: false },
  { name: 'Data mixture for code', ideas: 12, agents: 2, close: true },
  { name: 'Long context retrieval', ideas: 0, agents: 2, close: false },
]

if (import.meta.url === `file://${process.argv[1]}`) {
  const call = client(await tokenOf(READER))
  const { value: me } = await call('GET', '/me')
  const project = me.projects[0]
  const made = []
  for (const [k, plan] of PLAN.entries()) {
    const rng = random(1000 + k)
    const agents = Array.from({ length: plan.agents }, (_, i) => `${plan.name.split(' ')[0].toLowerCase()}-researcher-${i + 1}-${Date.now().toString(36)}`)
    const pe = await createExperiment(call, project.id, plan.name, agents)
    const started = Date.now()
    const ideas = await grow(call, rng, pe, agents, plan.ideas)
    console.error(`${plan.name}: ${plan.ideas} ideas in ${((Date.now() - started) / 1000).toFixed(0)} s`)
    made.push({ id: pe.id, name: plan.name, agents, close: plan.close, ideas: ideas.map(({ id, agent }) => ({ id, agent })) })
  }
  console.log(JSON.stringify({ reader: READER, project: project.id, experiments: made }))
}
