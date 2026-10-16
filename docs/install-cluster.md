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
  the registry in it. All traffic is outbound. Pods resolve the addresses with the cluster's
  DNS, which does not know tailnet (MagicDNS) names: a control plane on a tailnet is named by its
  IP. `hl cluster add` warns of an address only the control plane's own machine reaches
  (loopback or `host.containers.internal`).
- A registry that serves plain HTTP, as a local development registry does, needs every node's
  containerd to pull from it over HTTP. When the control plane says its registry is one
  (`runtimes.registry_plain_http`), the command pulls the chart with `--plain-http` and sets
  `insecureRegistry.enabled=true`: a DaemonSet then writes a containerd `hosts.toml` for the
  registry on every node, into `insecureRegistry.hostsDir` (k3s's by default; plain containerd
  needs `config_path` set to `/etc/containerd/certs.d` and that value). Off by default.
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

1. Preview: paste the command, add `--dry-run=server` at the end and run it. This is helm's
   own flag: the API server checks every object and helm prints them, installing nothing. It
   does not check that the cluster reaches the control plane or pulls the images; `install.sh
   --dry-run` from a checkout does (below). Add `--kube-context CONTEXT` for another cluster
   than the current one, and `--set-string kubeContext=CONTEXT` so the notes name it too.
2. Install: run the command and paste the runtime token when it asks for it.
3. Check: `kubectl -n hypothesisloop get pods` shows the cluster-agent and one node-agent per
   node running. The cluster shows **Connected** in the UI within a minute (`hl cluster list`).
   The agent logs each step of its start: the jobs namespace, syncing the cluster's nodes and
   pods (bounded at 5 minutes, then it exits naming the step), and its first contact.
4. If it does not connect: `kubectl -n hypothesisloop logs deploy/hypothesisloop-cluster-agent`.
   `hl cluster list` shows when the agent was last heard from (`last_contact_at`), what the
   control plane last refused it with (`last_error`), and the version it runs
   (`agent_version`), with `version_skew` when that is not the control plane's: run the
   command again to upgrade it.

The release's values are the cluster's one config file:
`helm get values hypothesisloop-cluster-agent -n hypothesisloop`. Optional values are documented
in the chart (`helm show values oci://REGISTRY/charts/hypothesisloop-cluster-agent`):
`jobs.preemptLowerPriorityPods`, `nodeAgent.nodeSelector`, `autoscalerEnabled`,
`detachOnUninstall`, `insecureRegistry.*`, `kubeContext` and `builder.*`. Append `--set key=value` to the command to set one.

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
  REGISTRY=registry.example.com/hypothesisloop TAG=the-tag-that-was-pushed INSTALL_FLAGS=--dry-run
```

`runtime/k8s/infra/install.sh --help` lists every setting. `install.sh --dry-run` (from make,
`INSTALL_FLAGS=--dry-run`) checks the tools and the cluster, lists the nodes, the accelerators
and the DRA devices it sees, and prints the plan, saying whether it creates the release or
upgrades it from which revision. It then runs the agent's image once, as a short-lived pod in
the namespace `default` with the token in a Secret beside it, both deleted after: the pod's
node pulls both images, and the agent asks the API's and the metrics service's `/healthz` and
an endpoint that answers only this cluster's token. Each check says ok or FAIL, and the dry run
exits non-zero when one fails. Drop the flag to install. `INSECURE_REGISTRY=true` has every node
pull from a registry of plain HTTP. `make cluster-agent-down` uninstalls.

## A cluster on another machine, attached to a local stack

The stack `make dev` runs names this machine as its own containers reach it
(`host.containers.internal` on macOS), which no other machine resolves. To attach a cluster on
another machine, for example over Tailscale, set one key in `.env`:

```bash
LOCAL_REACHABLE_ADDRESS='100.64.0.5'   # this machine's tailnet IP: tailscale ip -4
```

and run `make dev` again. The rendered settings then hand that address to every cluster, agent
and job: the API (`:8081`), the metrics service (`:8084`), the registry and its chart (`:5000`),
the object store (`:9000`) and the local git server (`:8085`). The registry is published on it
too, and `hl cluster add` prints a command that pulls the chart over plain HTTP and has every
node's containerd trust the registry. Use the IP and not a tailnet name, which pods do not
resolve. The local k3s cluster keeps reaching everything as before. Images are still pushed
with `make images`; the remote cluster runs the tag the control plane names.
