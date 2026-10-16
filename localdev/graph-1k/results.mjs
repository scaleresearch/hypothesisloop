// Gives the experiments seed.mjs made the results a run leaves: each measures its baseline and
// starts, then about half of the agent-owned ideas get a job that the graph-1k cluster runs to
// completion with a val_accuracy around the baseline's, most of those a verdict and some a
// finding, and the experiments the plan closes are closed. Through the API, as the person and
// the agents that person registered. Run after seed.mjs, with the stack's env and its output:
//   node localdev/graph-1k/results.mjs /tmp/graph-1k/seed.json
import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { READER, client, random, tokenOf } from './seed.mjs'

const GPU = 'nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3'
const BASELINE_ACCURACY = 0.78
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms))

async function until(what, check, seconds) {
  const deadline = Date.now() + seconds * 1000
  for (;;) {
    const seen = await check()
    if (seen.done) return seen.value
    if (seen.failed || Date.now() > deadline) throw new Error(`${what}: ${JSON.stringify(seen.value).slice(0, 400)}`)
    await sleep(1000)
  }
}

function jobBody(pe, agent, hypothesisID, accuracy) {
  const id = `job-${Math.random().toString(16).slice(2, 10)}`
  return {
    id,
    metadata: {
      agent_id: agent,
      platform_experiment_id: pe.id,
      ...(hypothesisID ? { hypothesis_id: hypothesisID, theory: 'the change moves val_accuracy against the control' } : {}),
      // Each idea's change is a commit of its own, which no repository holds: the cluster clones nothing.
      code_ref: `${pe.repository_url}@${hypothesisID ? createHash('sha1').update(hypothesisID).digest('hex') : 'a'.repeat(40)}`,
      branch: hypothesisID ? `hl/${pe.id}/${agent}/${hypothesisID.replace(/-/g, '').slice(-6)}` : `hl/${pe.id}/baseline`,
      config_hash: '',
      data_ref: '',
      capacity_tier: 'guaranteed',
      estimated_duration_hours: 0.005,
    },
    job: {
      image: 'host.containers.internal:5000/hypothesisloop-workload:graph-1k',
      cpu: '250m',
      memory: '128Mi',
      storage: '512Mi',
      max_retries: 0,
      accelerator_count: 1,
      accelerator_type: GPU,
      env: { GRAPH_1K_VAL_ACCURACY: accuracy.toFixed(4) },
    },
  }
}

async function finished(call, jobID) {
  return until(`job ${jobID} to complete`, async () => {
    // On a fresh metrics store a job is answered 500 until some job was first reported running,
    // which creates the table its node is read from.
    const { status, value } = await call('GET', `/experiments/${jobID}`, undefined, [500])
    if (status === 500) return { done: false, failed: false, value }
    return { done: value.status === 'COMPLETED', failed: ['FAILED', 'EVICTED', 'REJECTED', 'CANCELLED'].includes(value.status), value }
  }, 600)
}

// The baseline's completed job, measured now unless an earlier run of this script measured it.
async function measureBaseline(call, pe, stamp) {
  if (pe.baseline_job_id) return pe.baseline_job_id
  const { value: joined } = await call('GET', `/platform-experiments/${pe.id}/agents`)
  let agent = joined.agents.find(a => a.role === 'baseline')?.agent_id
  let jobID
  if (agent) {
    const { value: jobs } = await call('GET', `/experiments?platform_experiment_id=${pe.id}&agent_id=${agent}`)
    jobID = jobs[0]?.id
  } else {
    agent = `baseline-${pe.id.slice(-6)}-${stamp}`
    await call('POST', '/agents', { id: agent, name: agent })
    await call('POST', `/platform-experiments/${pe.id}/agents`, { agent_id: agent, role: 'baseline', quota_tier: 'guaranteed' })
  }
  if (!jobID) {
    const body = jobBody(pe, agent, '', BASELINE_ACCURACY)
    await call('POST', '/experiments', body)
    jobID = body.id
  }
  await finished(call, jobID)
  const body = { id: jobID }
  await call('POST', `/experiments/${body.id}/summary`, { summary: `The control ran unchanged as ${body.id}: val_accuracy ${BASELINE_ACCURACY}.` })
  const description = `${pe.description}\n\nBASELINE\n  config: the control, unchanged\n  code_ref: ${pe.repository_url}@${'a'.repeat(40)}\n  measured: ${body.id}\n`
  await call('PATCH', `/platform-experiments/${pe.id}`, { description, baseline_job_id: body.id })
  await until(`${pe.id} to run`, async () => {
    const { value } = await call('GET', `/platform-experiments/${pe.id}`)
    return { done: value.status === 'running', failed: value.status === 'closed', value: [value.status, value.status_reason] }
  }, 180)
  return body.id
}

async function testIdea(call, rng, pe, idea, control) {
  // Most changes do about as well as the control; a few help clearly, a few hurt.
  const accuracy = Math.min(0.97, Math.max(0.3, BASELINE_ACCURACY + (rng.next() - 0.55) * 0.12 + (rng.next() < 0.1 ? 0.06 : 0)))
  const runs = rng.next() < 0.3 ? 2 : 1
  for (let i = 0; i < runs; i++) {
    const body = jobBody(pe, idea.agent, idea.id, accuracy + (i ? (rng.next() - 0.5) * 0.01 : 0))
    await call('POST', '/experiments', body)
    await finished(call, body.id)
    if (rng.next() < 0.4) {
      await call('POST', `/experiments/${body.id}/summary`, {
        summary: `val_accuracy ${accuracy.toFixed(4)} against the control's ${BASELINE_ACCURACY}: ${accuracy > BASELINE_ACCURACY ? 'better' : 'worse'} by ${Math.abs(accuracy - BASELINE_ACCURACY).toFixed(4)}.`,
      })
    }
  }
  const r = rng.next()
  if (r < 0.1) return
  const status = r < 0.1 + 0.9 * 0.45 ? 'confirmed' : r < 0.1 + 0.9 * 0.8 ? 'refuted' : 'inconclusive'
  await call('POST', `/hypotheses/${idea.id}/status`, {
    status,
    agent_id: idea.agent,
    ...(status === 'inconclusive' ? {} : {
      against: control,
      despite: 'the seeded run compares one job of this idea with the baseline control job',
    }),
  })
}

async function pool(items, width, work) {
  const queue = [...items]
  await Promise.all(Array.from({ length: width }, async () => {
    for (let item; (item = queue.shift());) await work(item)
  }))
}

const file = process.argv[2] ?? '/tmp/graph-1k/seed.json'
let seeded
while (!(seeded = readFileSync(file, 'utf8').trim())) await sleep(5000)
const { experiments } = JSON.parse(seeded)
const call = client(await tokenOf(READER))
const stamp = Date.now().toString(36)
for (const [k, plan] of experiments.entries()) {
  const started = Date.now()
  const { value: pe } = await call('GET', `/platform-experiments/${plan.id}`)
  const control = await measureBaseline(call, pe, stamp)
  const rng = random(5000 + k)
  // The owner is read from the graph: an idea that read the same as an existing one was answered
  // with that one, which may be another agent's. One already tested by an earlier run is skipped.
  const { value: graph } = await call('GET', `/hypotheses/graph?platform_experiment_id=${plan.id}`)
  const tested = graph.nodes
    .filter(n => n.agent_id && rng.next() < 0.5 && !n.best_job && n.status === 'open')
    .map(n => ({ id: n.id, agent: n.agent_id }))
  let failed = 0
  await pool(tested, 24, idea => testIdea(call, rng, pe, idea, control).catch(error => {
    failed++
    console.error(`idea ${idea.id}: ${error.message}`)
  }))
  if (failed) console.error(`${plan.name}: ${failed} of ${tested.length} ideas could not be tested`)
  if (plan.close) await call('POST', `/platform-experiments/${plan.id}/close`, { top_results: [] })
  console.error(`${plan.name}: baseline ${control}, ${tested.length} ideas tested in ${((Date.now() - started) / 1000).toFixed(0)} s${plan.close ? ', closed' : ''}`)
}
