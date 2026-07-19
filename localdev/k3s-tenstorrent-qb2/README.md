# Tenstorrent k3s stack

Real-hardware counterpart to [`localdev/k3s-macos/`](../k3s-macos): where `localdev`
spins up k3s with *simulated* accelerator nodes for laptop dev, this spins up
k3s on an actual Tenstorrent host and installs
[tt-operator](https://docs.tenstorrent.com/tt-operator/latest/) so the
cluster recognizes real Tenstorrent PCIe cards and exposes them as
schedulable Kubernetes resources.

Run this on the Tenstorrent box itself (a QuietBox, a Galaxy node, anything
with `/dev/tenstorrent/*` devices) — not on a dev laptop. The two profiles
both install a native k3s systemd service, so don't run `localdev/k3s-macos/install.sh`
(Linux branch) and this on the same host at the same time; they'd fight over
the same `k3s` service.

## Usage

```bash
make tt-up       # bootstrap k3s + tt-operator (idempotent)
make tt-status   # node/device health check
make tt-stop     # pause (k3s stopped, all state preserved)
make tt-start    # resume
make tt-down     # full teardown (Helm release + k3s uninstall)
```

or run the scripts directly: `localdev/k3s-tenstorrent-qb2/install.sh`, `status.sh`,
`stop.sh`, `start.sh`, `destroy.sh`.

## What gets installed

| Component | Purpose | Enabled here? |
|---|---|---|
| k3s | The cluster itself (native systemd service, Traefik disabled) | yes |
| Node Feature Discovery | Labels nodes carrying a Tenstorrent PCI device (`feature.node.kubernetes.io/pci-1200_1e52.present=true`) | yes |
| tt-dra-driver | Publishes each card as a schedulable device via k8s [Dynamic Resource Allocation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) | yes |
| tt-fabric-manager | Resolves inter-card/inter-host fabric topology; tt-dra-driver depends on it | yes |
| tt-telemetry | Per-device health/metrics on a Prometheus endpoint | yes (collector only) |
| tt-k8s-driver-manager | Installs/manages `tt-kmd` via Kubernetes policy CRs | **no** — this host already has `tt-kmd` via `tenstorrent-dkms` (apt/DKMS); the operator would auto-detect that as host-managed and idle anyway, but disabling it skips installing a controller for a job apt already does |
| jobset / kubepmix | Multi-node JobSet lifecycle + PMIx-aware MPI wiring for distributed training across hosts; kubepmix also needs cert-manager pre-installed | **no** — a single QuietBox is one node with local cards, not a multi-host job. Re-enable both (and install cert-manager first) if this host joins a multi-node cluster |

`tt-telemetry`'s aggregator Deployment (as opposed to its per-node collector
DaemonSet) is also disabled: it polls collectors over `hostNetwork:8080`,
the same port the collector itself listens on — fine across multiple nodes,
but a same-port collision on a single-node cluster where both would land on
the one node. There's nothing to aggregate across on a single host anyway.

## Requesting a device in a pod

Verified working end-to-end on this box: two pods, each with their own
`ResourceClaim`, running concurrently — one gets `/dev/tenstorrent/0`, the
other `/dev/tenstorrent/1`, confirming DRA is doing real per-device
allocation rather than granting broad `/dev/tenstorrent` access.

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: tt-claim
spec:
  devices:
    requests:
      - name: chip
        exactly:
          deviceClassName: tenstorrent.com
---
apiVersion: v1
kind: Pod
metadata:
  name: tt-workload
spec:
  containers:
    - name: app
      image: <your-image>
      resources:
        claims:
          - name: chip
  resourceClaims:
    - name: chip
      resourceClaimName: tt-claim
```

## Attaching the openresearch cluster-agent (verified end-to-end)

This folder only sets up the Tenstorrent device stack. Attaching it to the
openresearch control plane so real jobs schedule onto it end-to-end has been
verified working — `localdev/k3s-tenstorrent-qb2/e2e-test.sh` drives the whole thing through
the platform's own `POST /experiments` API (never a hand-written pod/
ResourceClaim YAML) and confirms a genuine `tenstorrent.com` device gets
allocated. This exercises a new first-class scheduling mode, not a one-off
hack — see "How Tenstorrent devices are scheduled" below.

1. **Register the accelerator type.** `controlplane/settings/openresearch.yaml`
   already carries a `Tenstorrent-Blackhole` entry with
   `allocation_mode: dra` and `device_class_name: tenstorrent.com` — no
   `node_label_value`/`resource_name`/`taint_key` needed, unlike every other
   (device-plugin-based) entry in that catalog. `flavor:` must equal
   `domain.AcceleratorType.FlavorName()` — the admission loop computes "what
   a job needs" from that derived name, not by reading the config's own
   `flavor:` field back, so a mismatch here silently means jobs queue
   forever with `shortage` in control-service's logs.

2. **Register the cluster.** `controlplane/settings/clusters.yaml` has a
   `tt-quietbox` entry (`context: k3s-tt`). Only `name` is actually read by
   control-service/metrics-service today — neither dials into a cluster
   directly (see `cluster/docs/execution-layer.md`).

3. **Build + bring up the control plane** (from repo root; this repo's
   Makefile assumes `podman`, substitute `docker`/`docker compose` if that's
   what's installed):
   ```bash
   go mod vendor   # Dockerfiles COPY vendor/, not go.mod-resolved deps
   docker build -f controlplane/build/Dockerfile.control-service -t openresearch-control-service .
   docker build -f controlplane/build/Dockerfile.metrics-service -t openresearch-metrics-service .
   docker build -f cluster/build/Dockerfile.cluster-agent -t localhost/openresearch-cluster-agent .
   docker build -f cluster/build/Dockerfile.node-agent -t localhost/openresearch-node-agent .
   docker compose -f controlplane/infra/docker-compose.yaml up -d
   ```
   (`controlplane/settings/openresearch.yaml` and `clusters.yaml` are baked
   into the control-service/metrics-service/cluster-agent images at build
   time — re-build+redeploy after editing either.)

4. **Attach the cluster-agent to `k3s-tt`.** It needs real outbound URLs
   (this bare-metal host's own LAN IP — `host.docker.internal`, the
   install.sh default, only resolves inside a podman-VM/Docker-Desktop
   network namespace, not a native k3s pod's):
   ```bash
   docker save localhost/openresearch-cluster-agent:latest localhost/openresearch-node-agent:latest \
     | sudo k3s ctr images import -
   CLUSTER_NAME=tt-quietbox KUBE_CONTEXT=k3s-tt \
     CONTROLPLANE_URL=http://<this-host-LAN-IP>:8082 \
     REGISTRY_URL=http://<this-host-LAN-IP>:8083 \
     bash cluster/infra/install.sh
   ```

5. **Build + import the test workload image, then run the e2e test:**
   ```bash
   docker build -f tests/workloads/tenstorrent/Dockerfile.train \
     -t localhost/openresearch-tenstorrent-workload tests/workloads/tenstorrent/
   docker save localhost/openresearch-tenstorrent-workload:latest | sudo k3s ctr images import -
   bash localdev/k3s-tenstorrent-qb2/e2e-test.sh
   ```
   Verified output: the job admits onto `cluster_name=tt-quietbox`, a real
   pod runs on the QuietBox, its `ResourceClaim`'s
   `status.allocation.devices.results` shows a genuine
   `{device: tt-0, driver: tenstorrent.com, pool: tt-quietbox}` allocation,
   and the pod actually runs real bf16 matmuls on that silicon (see "The
   workload itself" below) — confirmed COMPLETED, with real measured
   `tflops_measured`/`latency_ms` metrics recorded, across four separate
   runs including two concurrent submissions that landed on distinct
   devices (`tt-0` and `tt-1`) simultaneously.

## The workload itself: real compute, not a stub

`tests/workloads/tenstorrent/train.py` is **not** the synthetic-metrics
simulator every other accelerator type's test workload
(`tests/workloads/generic/train.py`) uses — it runs inside Tenstorrent's own official
release image
(`ghcr.io/tenstorrent/tt-metal/tt-metalium-ubuntu-22.04-release-amd64:latest-rc`,
pulled from `ghcr.io`, not built in-house) and:

1. Opens the one Tenstorrent device DRA allocated to this pod via `ttnn.open_device`.
2. Runs a real bf16 matmul sweep (`n = 256, 512, 1024, 2048, 4096`) with a
   warmup pass, then times 10 timed iterations per size with
   `ttnn.synchronize_device` bracketing the timing window (so the reported
   latency is real device time, not just kernel-launch overhead).
3. Pushes the real numbers — `tflops_measured`, `latency_ms` — through the
   same `POST /registry/experiments/{id}/metrics` API every other workload
   uses, alongside a `val_accuracy` value repurposed as a monotonically
   non-decreasing "best TFLOPS so far, normalized" (see the script's doc
   comment for why: the test harness's platform experiments hard-declare
   `val_accuracy` as the one required contract metric, and a benchmark sweep
   isn't naturally monotonic the way accuracy curves are expected to be —
   this keeps it contract-compliant without fabricating a training curve).

Measured on this QuietBox (Blackhole, KMD 2.10.0, firmware 19.11.0): up to
**~180 TFLOPS bf16** at `n=4096`, scaling from ~2 TFLOPS at `n=256` — real
numbers from real silicon, not simulated.

Two non-obvious fixes were needed to get here, both baked into
`Dockerfile.train`/`job.yaml` and worth knowing if you extend this workload:

- **Single-chip fabric init.** This QuietBox's cards are p300 boards (2
  ASICs per physical board); DRA allocates one ASIC per pod, so only one
  `/dev/tenstorrent/N` node is ever visible inside the container. ttnn's
  default fabric/mesh initialization expects to see a whole board and fails
  with `Custom fabric mesh graph descriptor path must be specified for
  CUSTOM cluster type` the moment it sees only one chip of a two-chip board.
  Fixed by pointing `TT_MESH_GRAPH_DESC_PATH` at the vendor's own
  single-chip 1x1 mesh descriptor (shipped in the same image, normally used
  for single-ASIC p100 boards) — it accurately describes "one isolated
  chip, no fabric routing to a sibling."
- **Memory sizing.** The real tt-metal runtime's kernel JIT compilation and
  dispatch buffers need real memory — `256Mi` (a synthetic-stub-sized
  default) gets silently `OOMKilled` every run; `job.yaml` requests `4Gi`.

## How Tenstorrent devices are scheduled

Every other accelerator type in this platform (`T4`, `H100`, the AMD
example, ...) is scheduled the classic way: a device-plugin advertises a
plain k8s extended resource (`nvidia.com/gpu`), and the backend requests a
quantity of it plus a `nodeAffinity` on a vendor product label. Tenstorrent's
own stack (this folder) instead advertises devices via Kubernetes **Dynamic
Resource Allocation** (`resource.k8s.io`) — `ResourceSlice`s under
`DeviceClass: tenstorrent.com`, individually allocatable per pod.

Rather than bolt on a one-off Tenstorrent code path, the control plane grew
a second, general `allocation_mode` per accelerator type
(`controlplane/shared/config/types.go`'s `AllocationMode` doc has the full
rationale):

- `resource` (default) — today's device-plugin/nodeAffinity/taint model,
  unchanged for every existing NVIDIA/AMD entry.
- `dra` — the backend creates a `ResourceClaimTemplate` requesting
  `device_class_name` (quantity = `accelerator_count`) and attaches it to
  the pod via `PodResourceClaims`/`Container.Resources.Claims` instead of an
  extended resource. No node label/resource name/taint needed — the DRA
  scheduler plugin and the vendor's kubelet plugin handle device selection
  and placement natively.

This means: no DSL/API change was needed (agents still just say
`accelerator_type: Tenstorrent-Blackhole, accelerator_count: 1` — see
`controlplane/docs/*.md`), a future AMD *DRA* driver or JobSet-managed
multi-host DRA setup is a config entry away rather than new code, and the
resource.k8s.io object itself is created via `client-go`'s dynamic/
unstructured client (not a version-pinned typed client) specifically so it
tracks whatever DRA API version the target cluster serves (`v1` here, on
k3s 1.36) independent of this repo's `k8s.io/api` pin.

## Troubleshooting

- **`ImagePullBackOff` in `tt-operator-system`**: nodes need outbound access
  to `ghcr.io` (directly or via a mirror).
- **No `ResourceSlice`s show up** (`kubectl get resourceslices`): the DRA
  driver only publishes devices once fabric topology resolves — check
  `tt-fabric-manager` pod logs. On a single-host box with no staged fabric
  config this is a known rough edge upstream (see tt-dra-driver docs), not
  necessarily a misconfiguration here.
- **`tt-smi` on the host disagrees with what's in Kubernetes**: `tt-smi -ls`
  (from `~/.tenstorrent-venv` if not on `PATH`) is ground truth for what the
  driver sees; `status.sh` prints both side by side.
