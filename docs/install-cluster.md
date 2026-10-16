# Connect a cluster

A cluster is where jobs run: a Kubernetes cluster, or one bare-metal node. Each runs a small
agent that calls out to the HypothesisLoop control plane for work and reports back. Nothing
connects into the cluster, and the agent holds the only credentials to it.

Every cluster has exactly two inputs: its **runtime token** and the **control plane's
addresses**. You never type the addresses: the UI and `hl` fill them in from the control
plane's settings.

## 1. Register the cluster

In the UI, open **Compute Resources**, type a name (for example `lab-k8s` or `gpu-box-1`) and
press **Add cluster**. Pick **Kubernetes cluster** or **Bare-metal node**. The UI shows:

- the install command, with the cluster name, API address, metrics address, image registry and
  tag filled in;
- the runtime token, which the command asks for when it runs.

From a terminal, `hl cluster add NAME` prints the Kubernetes command and the token, and
`hl cluster add NAME --bare-metal` the node command.

The command holds no token. It asks for it without echo and hands it on through standard input,
so the token is in no shell history and no process list. **Show start command** in the
cluster's row brings both back.

## 2a. Kubernetes cluster

### Before you run it

- Kubernetes 1.34 or newer. The chart refuses older clusters.
- `helm` 3.17 or newer and `kubectl` access that may create namespaces, PriorityClasses and
  cluster roles (cluster admin, once).
- Every node reaches the API and metrics addresses shown in the command, and pulls images from
  the registry in it. All traffic is outbound.
- At least one node without taints, or with the `hypothesisloop.io/no-workload` taint or the
  control-plane role, for the cluster-agent.
- For accelerators, their device plugin or DRA driver. The agent discovers what the cluster
  offers and needs no accelerator settings:
  - NVIDIA: the NVIDIA GPU Operator, or its device plugin with GPU Feature Discovery (for the
    `nvidia.com/gpu.product` label).
  - AMD: the AMD GPU device plugin with its node labeller.
  - Tenstorrent: the Tenstorrent DRA driver.
  - Without any, the cluster offers its CPU cores.

### What it installs

The command is a plain `helm upgrade --install` of the chart `hypothesisloop-cluster-agent`
from the image registry. It creates:

| Where | What |
| --- | --- |
| namespace `hypothesisloop` | Deployment `hypothesisloop-cluster-agent` (1 pod), DaemonSet `hypothesisloop-node-agent` (1 pod per node, reads cgroup metrics), Secret `hypothesisloop-runtime-token` |
| namespace `hypothesisloop-jobs` | every job, as a Kubernetes Job with its pods, and per job a Service, pull Secret or ResourceClaimTemplate when it needs one |
| cluster | PriorityClasses `hypothesisloop-jobs-guaranteed` and `hypothesisloop-jobs-burst` (never preempt other workloads by default) |

Permissions:

- The cluster-agent may create and delete Jobs, pods, Services, Secrets and
  ResourceClaimTemplates in `hypothesisloop-jobs` only.
- Across the cluster it may only read nodes, pods, DRA devices and claims.
- The node-agent may only read pods in `hypothesisloop-jobs`. It runs with `hostPID` and a
  read-only mount of `/sys/fs/cgroup` to measure each job.

Resources: the agents request 10m CPU and 16 to 32 MiB of memory each.

### Steps

1. Preview: paste the command, add `--dry-run=server` at the end and run it. Helm prints every
   object and installs nothing.
2. Install: run the command and paste the runtime token when it asks for it.
3. Check: `kubectl -n hypothesisloop get pods` shows the cluster-agent and one node-agent per
   node running. The cluster shows **Connected** in the UI within a minute (`hl cluster list`).
4. If it does not connect: `kubectl -n hypothesisloop logs deploy/hypothesisloop-cluster-agent`.

The release's values are the cluster's one config file:
`helm get values hypothesisloop-cluster-agent -n hypothesisloop`. Optional values are documented
in the chart (`helm show values oci://REGISTRY/charts/hypothesisloop-cluster-agent`):
`jobs.preemptLowerPriorityPods`, `nodeAgent.nodeSelector`, `autoscalerEnabled`,
`detachOnUninstall` and `builder.*`. Append `--set key=value` to the command to set one.

### Upgrade and uninstall

- Upgrade: run the command again. The UI always shows the command of the version the control
  plane runs.
- Uninstall:

  ```bash
  helm uninstall hypothesisloop-cluster-agent -n hypothesisloop
  kubectl delete namespace hypothesisloop
  ```

  A hook first removes the cluster from HypothesisLoop, which sends its jobs back to the queue,
  then helm removes `hypothesisloop-jobs`, the PriorityClasses and the agents. If the control
  plane cannot be reached the uninstall fails and says so; `--no-hooks` forces it.

## 2b. Bare-metal node

The node runs one agent, which starts jobs as containers through docker or podman. Each node is
a cluster of its own. No Kubernetes is involved.

### Before you run it

- Linux (amd64 or arm64, glibc 2.17 or newer) with systemd, and root.
- docker or podman. With podman alone the installer enables `podman.socket`.
- NVIDIA GPUs: the NVIDIA driver and the NVIDIA Container Toolkit.
- Tenstorrent cards: the tt-kmd driver, and 1 GiB hugepages for jobs that ask for them.
- The node reaches the API address in the command and pulls from the registry in it. All
  traffic is outbound.

### What it installs

The command copies the `bare-agent` binary out of its image (no container runs) and runs
`bare-agent install`, which checks the node, checks that the platform accepts the token, and
only then writes:

| Path | What |
| --- | --- |
| `/usr/local/bin/bare-agent` | the agent |
| `/etc/hypothesisloop/bare-agent.env` | the node's one config file: `CLUSTER_NAME`, `API_URL`, `RUNTIME_TOKEN`, mode 0600 |
| `/etc/systemd/system/hypothesisloop-agent.service` | the unit, started at boot and restarted on failure |

The agent runs as root, since the container engine is root-equivalent anyway. It opens no port.
Job containers get only the devices and directories their job names and are never privileged.

### Steps

1. Preview: add `--dry-run` at the end of the command and run it on the node. It checks the
   engine, accelerators, storage and token, and prints the files and unit it would write.
2. Install: run the command and paste the runtime token when it asks for it.
3. Check: the node shows **Connected** in the UI within a minute.
   Logs: `journalctl -u hypothesisloop-agent -f`.

Running the command again is safe: it replaces the three files in one step and restarts the
agent. Pressing Enter at the token prompt keeps the installed token.

### Upgrade and uninstall

- Upgrade: run the command of the newer version.
- Uninstall, on the node:

  ```bash
  sudo bare-agent detach      # removes the cluster from HypothesisLoop, stops its jobs and the agent
  sudo bare-agent uninstall   # removes the three files
  ```

  Pulled job images stay; `docker image prune` (or podman's) removes them.

## Removing a cluster from the platform side

`hl cluster remove NAME`, or **Advanced > Remove cluster** in the UI, sends the cluster's jobs
back to the queue and refuses its token from then on. Its agent stops its jobs and stays
stopped. Uninstall it as above.

## From a checkout

Operators with a checkout install the same chart from `runtime/k8s/infra/chart`:

```bash
RUNTIME_TOKEN="$(hl cluster token my-cluster)" make cluster-agent-up CLUSTER=my-cluster \
  API_URL=https://api.example.com METRICS_URL=https://metrics.example.com \
  REGISTRY=registry.example.com/hypothesisloop TAG=the-tag-that-was-pushed DRY_RUN=true
```

`runtime/k8s/infra/install.sh --help` lists every setting. `DRY_RUN=true` checks the tools and
the cluster, lists the accelerators it sees and prints the plan; drop it to install.
`make cluster-agent-down` uninstalls.
