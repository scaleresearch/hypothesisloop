// A cluster runtime of the reader's org with one node of 64 GPUs. It reports its capacity,
// runs every job the platform wants on it for a few seconds and reports it running, then
// succeeded, pushing the job's metric with the job's own token as a real workload does. The value
// pushed is the job's env GRAPH_1K_VAL_ACCURACY (results.mjs sets it), 0.5 when the job has none.
// Started by stack.sh up and kept running.
//   node cluster.mjs
import { READER, client, tokenOf } from './seed.mjs'

const NAME = 'graph-1k'
const NODE = 'graph-1k-node-1'
const GPU = 'nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3'
const GPUS = 64
const RUN_MS = 4000
const TICK_MS = 2000

const person = client(await tokenOf(READER))
const { value: me } = await person('GET', '/me')
const org = me.orgs[0].id
// The runtime token is answered only when the cluster is registered, so one left by an earlier
// run is registered again. The list is not asked first: on a fresh metrics store it fails until
// some cluster has reported.
let { status, value: registered } = await person('POST', `/orgs/${org}/clusters`, { name: NAME }, [409])
if (status === 409) {
  await person('DELETE', `/orgs/${org}/clusters/${NAME}`)
  ;({ value: registered } = await person('POST', `/orgs/${org}/clusters`, { name: NAME }))
}
const runtime = client(registered.runtime_token)

const resources = { cpu_millicores: 64000, memory_bytes: 512 * 2 ** 30, storage_bytes: 2 * 2 ** 40 }

function report(gpusFree) {
  return {
    cluster_id: `id-${NAME}`,
    cpu_available_cores: 64, cpu_total_cores: 64,
    ram_available_bytes: resources.memory_bytes, ram_total_bytes: resources.memory_bytes,
    storage_available_bytes: resources.storage_bytes, storage_total_bytes: resources.storage_bytes,
    accelerator_available_by_type: { [GPU]: gpusFree }, accelerator_total_by_type: { [GPU]: GPUS },
    accelerator_available_by_node: { [NODE]: { [GPU]: gpusFree } },
    node_resources_by_node: { [NODE]: resources }, node_resources_total_by_node: { [NODE]: resources },
    node_labels_by_node: { [NODE]: { 'nvidia.com/gpu.product': 'NVIDIA-H100-80GB-HBM3' } },
    multi_node_capable: false, autoscaler_enabled: false,
  }
}

// What runs, by job id: its attempt, when it started, the metric samples pushed so far, when it ended.
const running = new Map()

async function pushMetric(job, run, step) {
  const env = job.resolved_job?.env ?? job.job?.env ?? {}
  const value = Number(env.GRAPH_1K_VAL_ACCURACY ?? 0.5)
  const res = await fetch(`${process.env.API_URL}/experiments/${job.id}/metrics`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${job.token}`, 'Content-Type': 'application/json' },
    // Rises to its value, so nothing reads the series as declining.
    body: JSON.stringify({ metric_name: 'val_accuracy', fraction_complete: step / 3, metric_value: value - 0.002 * (3 - step) }),
  })
  if (!res.ok) console.error(`metric of ${job.id} step ${step}: ${res.status} ${await res.text()}`)
  run.pushed = step
}

for (;;) {
  const now = Date.now()
  const busy = [...running.values()].filter(r => !r.endedAt).reduce((n, r) => n + r.gpus, 0)
  try {
    const { value: answer } = await runtime('POST', `/internal/clusters/${NAME}/reconcile`, report(Math.max(0, GPUS - busy)))
    const desired = new Map(answer.experiments.map(j => [j.id, j]))
    for (const id of running.keys()) if (!desired.has(id)) running.delete(id)
    for (const job of desired.values()) {
      let run = running.get(job.id)
      if (!run || run.attempt !== job.attempt_count) {
        run = { attempt: job.attempt_count, startedAt: new Date(now), pushed: 0, endedAt: null, gpus: job.accelerator_count || 0 }
        running.set(job.id, run)
      }
      if (run.endedAt) continue
      const step = Math.min(3, Math.floor(((now - run.startedAt.getTime()) / RUN_MS) * 3) + 1)
      while (run.pushed < step) await pushMetric(job, run, run.pushed + 1)
      if (now - run.startedAt.getTime() >= RUN_MS && run.pushed === 3) run.endedAt = new Date(now)
    }
    const reports = [...running.entries()].map(([id, run]) => ({
      experiment_id: id,
      phase: run.endedAt ? 'succeeded' : 'running',
      admitted_accelerator_type: GPU,
      admitted_node: NODE,
      attempt: run.attempt,
      ...(run.endedAt ? { started_at: run.startedAt.toISOString(), ended_at: run.endedAt.toISOString() } : {}),
    }))
    await runtime('POST', `/internal/clusters/${NAME}/status`, { reports })
  } catch (error) {
    console.error(error.message)
  }
  await new Promise(resolve => setTimeout(resolve, TICK_MS))
}
