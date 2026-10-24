# hypothesisloop-cluster-agent

Connects one Kubernetes cluster to HypothesisLoop. The agents only call out to the control
plane; nothing connects into the cluster.

Install it with the command the HypothesisLoop UI shows under **Compute Resources > Add
cluster** (or `hl cluster add NAME`). It fills in every required value and asks for the
cluster's runtime token. The full guide is `docs/install-cluster.md` in the HypothesisLoop
repository.

## Requirements

- Kubernetes 1.34 or newer, helm 3.17 or newer.
- Outbound access from every node to `apiURL`, `metricsURL` and `images.registry`. Pods resolve
  them with the cluster's DNS, which does not know tailnet names: name a control plane on a
  tailnet by its IP.
- A registry of plain HTTP needs `insecureRegistry.enabled=true`, which writes a containerd
  `hosts.toml` for it on every node (`values.yaml` says where).
- For accelerators, their device plugin or DRA driver (NVIDIA GPU Operator, AMD GPU device
  plugin with node labeller, Tenstorrent DRA driver).

## What it creates

- Namespace of the release (`hypothesisloop`): Deployment `hypothesisloop-cluster-agent`,
  DaemonSet `hypothesisloop-node-agent`, Secret `hypothesisloop-runtime-token`.
- Namespace `hypothesisloop-jobs`, where jobs run, and PriorityClasses
  `hypothesisloop-jobs-guaranteed` and `hypothesisloop-jobs-burst`.
- RBAC: create and delete Jobs, pods, Services, NetworkPolicies, Secrets and
  ResourceClaimTemplates in `hypothesisloop-jobs` only; read nodes, pods and DRA devices across the cluster.

## Values

Required: `clusterName`, `apiURL`, `metricsURL`, `runtimeToken`, `images.registry`,
`images.tag`. None has a default. `values.yaml` documents the optional ones, among them
`placementGroupLabel`, the node label key that names a node's placement group for jobs with
`topology.nodes_within: placement_group`.

## Verify, upgrade, uninstall

```bash
kubectl -n hypothesisloop get pods
helm test hypothesisloop-cluster-agent -n hypothesisloop --logs   # from a pod: images pull, API and metrics answer, token accepted
helm get values hypothesisloop-cluster-agent -n hypothesisloop   # this cluster's settings
helm uninstall hypothesisloop-cluster-agent -n hypothesisloop    # detaches the cluster first
kubectl delete namespace hypothesisloop
```

Upgrade by running the install command again.
