# Kubernetes Execution Layer

**Scope:** how scheduling decisions made by the control plane (see `controlplane/docs/scheduling.md`) turn into actual Kubernetes Jobs, and which component holds cluster credentials.
Code: `cluster/cmd/cluster-agent`, `cluster/cmd/node-agent`, `controlplane/shared/workload`, `controlplane/shared/queuebackend`, `cluster/infra/`.

There is **no Kueue** in this system. All quota enforcement, ordering, fairness, and preemption logic lives in the control plane (`controlplane/docs/scheduling.md`); this layer's only job is: given a desired set of Jobs, converge a real cluster to match it, and report status back.

---

## Split of responsibility: control plane vs. cluster-agent

The control plane (`controlplane/cmd/control-service`, `.../metrics-service`) **never holds a kubeconfig and never dials a cluster API server.** Its view of "the cluster" is `queuebackend.Backend` (`controlplane/shared/queuebackend/queue_backend.go`), which only reads/writes Postgres: it maintains a desired-state list of Jobs per named cluster and returns static per-flavor capacity numbers from `openresearch.yaml` (not live cluster state).

**`cluster-agent`** (`cluster/cmd/cluster-agent/main.go`) is the only component anywhere in the system with real Kubernetes credentials. It runs a kubelet-style pull/reconcile/report loop, entirely outbound from inside the target cluster:
1. `GET /internal/clusters/{name}/desired-state` from the control plane every ~2s.
2. Diffs against locally-listed managed Jobs; creates/deletes native `batchv1.Job` objects to converge. Idempotent — no leader election needed.
3. Separately polls local Job phases and `POST`s status back to `/internal/clusters/{name}/status` every ~3s.

This supports multiple target clusters: `controlplane/settings/clusters.yaml` registers clusters, `db.ClusterQueueStore` tracks per-cluster heartbeats, and `workload.ClusterSet` routes each experiment's Jobs (`domain.Experiment.ClusterName`) to the right one. None of this multi-cluster machinery existed in the original Kueue-based design.

`node-agent` is unchanged in role: a DaemonSet reading cgroup v2 CPU stats per pod, pushed to the metrics service (see `controlplane/docs/observability.md`). It runs alongside `cluster-agent` but has no Job-management privileges.

RBAC for `cluster-agent` (`cluster/infra/cluster-agent-deployment.yaml`): namespaces (get/create), configmaps (get), priorityclasses (get/create/update), jobs (get/list/watch/create/delete) — no cluster-admin, no CRD access.

---

## GPU types & capacity

5 GPU types, same T4h rates as `controlplane/docs/quota-and-experiments.md`, but defined purely as control-plane config in `controlplane/settings/openresearch.yaml` under `gpu_types[]` — no ResourceFlavor CRDs. Each entry carries `flavor`, `cluster_gpus` (physical inventory used for the static capacity check in `queuebackend.Backend.GetFlavorCapacity`), `node_label_value`, and optional `node_label_key` / `resource_name` / `taint_key` for multi-vendor node pools. Per-cluster resource defaults live in `cluster/infra/job-defaults-configmap.yaml`.

Because capacity is a static config number rather than a live query, physical oversubscription is prevented purely by convention (burst's virtual 2× ledger overcommit relies on burst jobs being lower-priority and preemptable — see below), not by a resource-gate CRD watching real usage.

---

## Priority instead of ClusterQueues

Two tiers map to two native `scheduling.k8s.io/v1` `PriorityClass` objects, created idempotently by `JobWorkloadClient.ensurePriorityClasses` (`controlplane/shared/workload/workload_client.go:426`):

| | `openresearch-guaranteed` | `openresearch-burst` |
|---|---|---|
| value | 1,000,000 | 100,000 |

There are no Cohorts, ClusterQueues, or LocalQueues — the control plane's own ordering and preemption logic (`controlplane/docs/scheduling.md`) is the only admission gate; the PriorityClass values only affect the Kubernetes scheduler's pod-eviction tie-breaking, not admission itself. The control plane deletes and recreates Jobs directly for preemption/eviction — it does not rely on the native Kubernetes preemption path.

---

## Job construction

`JobWorkloadClient.BuildJob` (`controlplane/shared/workload/workload_client.go:701`) builds the native `batchv1.Job`, including features absent from the old Kueue-based design:

- Multi-node distributed jobs (`NumNodes`, Indexed completion mode, per-rank env vars, headless Service for pod DNS). `parallelism == completions` on Indexed Jobs is exactly the shape k8s 1.36's native gang scheduling targets (Job controller auto-creates `Workload`/`PodGroup`, in-tree scheduler admits/binds the whole gang atomically) — see "Gang scheduling" below. No control-plane code change needed to use it, only cluster-level feature gates.
- `TopologySpec` for spread-across-hosts or same-zone pod affinity.
- `ShmSize` shared-memory volume.
- Per-GPU-type node affinity/taints/resource names for multi-vendor clusters.
- Merge of `cluster/infra/job-defaults-configmap.yaml` for per-cluster resource defaults.
- `RestartPolicy=OnFailure`, `BackoffLimit=scheduler.job_backoff_limit` (default 3) — native crash-loop handling; the job watcher marks the job `FAILED` and refunds unused T4h once Kubernetes gives up retrying.

### Flavor substitution (currently dormant)

`JobWatcher.onRunning` (`controlplane/services/scheduler/job_watcher.go:207`) contains logic to debit a cost difference if the admitted GPU type differs from the requested one — a carryover from the days a resource gate could substitute an interchangeable flavor (e.g. H100→H200). Today `GetAdmittedGPUType` is a passthrough (`workload_client.go:604-612`) and nothing in the native-Jobs path ever substitutes flavors, so this is effectively dead code unless a future `Backend` implementation adds real substitution.

### Gang scheduling — native k8s 1.36, not Volcano

The synthesis doc's original gang-scheduling recommendation (`competetors/SYNTHESIS_GAPS_AND_PLAN.md` item #11) was to narrowly adopt Volcano's `vc-scheduler` + `PodGroup` CRD purely as an atomic-admission gate. That's superseded: k8s 1.36 added the same capability in-tree — the Job controller auto-creates a `Workload`/`PodGroup` for any parallel Indexed Job where `parallelism == completions` and sets `spec.schedulingGroup` on every Pod (`WorkloadWithJob`, alpha), and the in-tree scheduler evaluates/binds the whole gang atomically instead of pod-by-pod (`GangScheduling`, alpha — a *separate* gate `WorkloadWithJob` depends on but doesn't imply; without it the `Workload`/`PodGroup` objects get created but the scheduler still admits pods one at a time). `BuildJob` already produces exactly that shape for `NumNodes > 1` jobs (see above), so **no control-plane code is needed** — only cluster-level feature gates (`GenericWorkload`, `WorkloadWithJob`, `GangScheduling` on kube-apiserver/kube-controller-manager/kube-scheduler, plus the `scheduling.k8s.io/v1alpha2` API group). `localdev/install.sh` enables these for local dev (pinned to k3s `v1.36.2+k3s1`). For real clusters, the same flags need to be set wherever `cluster-agent` targets — track that as a per-cluster provisioning prerequisite, not code.

It's still alpha and off by default upstream — don't depend on it for anything beyond local dev / an early real-cluster trial until it graduates, and re-verify the feature-gate names against the target k8s version before enabling in a real cluster.

---

## Setup

`cluster/infra/install.sh` / `destroy.sh` apply/tear down the `cluster-agent` Deployment, `node-agent` DaemonSet, and `job-defaults-configmap.yaml` — no Kueue installation step, no ResourceFlavor/ClusterQueue manifests exist anywhere in this repo.
