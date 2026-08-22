#!/usr/bin/env bash
# Cluster preconditions worth checking once, before anything is submitted. Deliberately free of
# side effects (no set -e, no traps, no globals beyond the function) so both tests/run.sh and
# tests/lib/common.sh can source it without inheriting each other's shell settings.

# Reported capacity and schedulable capacity are two different things: a node keeps advertising
# its accelerators while carrying the no-workload taint, so a scenario that only checks capacity
# submits happily and then times out with an unexplained "never reached RUNNING". Fail fast with
# the actual cause instead. Skipped when kubectl isn't available (API-only/bare-metal runs).
preflight_accelerator_schedulable() {
  command -v kubectl >/dev/null || return 0
  local tainted
  tainted=$(kubectl get nodes -o json 2>/dev/null | python3 -c '
import json, sys
nodes = json.load(sys.stdin).get("items", [])
if not nodes:
    print("")
    raise SystemExit
bad = [n["metadata"]["name"] for n in nodes
       if any(t.get("key") == "hypothesisloop.io/no-workload" for t in (n["spec"].get("taints") or []))]
print(",".join(bad) if len(bad) == len(nodes) else "")
' 2>/dev/null) || return 0
  if [[ -n "$tainted" ]]; then
    echo "ERROR: every node ($tainted) carries the hypothesisloop.io/no-workload taint, so no job" >&2
    echo "       can be scheduled even though /resource-catalog/capacity still reports capacity." >&2
    echo "       Donate the node first:  source localdev/lib/node.sh && lib_attach_node <context> <node>" >&2
    return 1
  fi
  return 0
}

# The workload image lives only in each node's local containerd — nothing serves it, so a node
# that lost it fails every job with image_pull_failed and an "unschedulable" eviction, which
# reads as a scheduler bug. The usual cause is kubelet image GC: it reclaims unreferenced images
# once the image filesystem passes its high threshold, and this host shares one filesystem across
# every fake node. Fail the run with that cause instead of scattering it across scenarios.
preflight_workload_image_present() {
  command -v kubectl >/dev/null || return 0
  command -v sudo >/dev/null || return 0
  local image="${WORKLOAD_IMAGE:-localhost/hypothesisloop-workload:latest}" node missing=""
  for node in $(kubectl get nodes -o name 2>/dev/null | sed 's|node/||'); do
    sudo podman container exists "$node" 2>/dev/null || continue
    sudo podman exec "$node" ctr --address /run/k3s/containerd/containerd.sock -n k8s.io \
      images ls -q 2>/dev/null | grep -qF "$image" || missing="${missing} ${node}"
  done
  [[ -z "$missing" ]] && return 0
  echo "ERROR: ${image} is missing from each node's containerd:${missing}" >&2
  echo "       Every job placed there will fail to pull it and be evicted as unschedulable." >&2
  echo "       Usually kubelet image GC reclaiming it under disk pressure — check \`df -h /\`," >&2
  echo "       free space (podman image prune), then re-run localdev/<cluster>/reload.sh." >&2
  return 1
}
