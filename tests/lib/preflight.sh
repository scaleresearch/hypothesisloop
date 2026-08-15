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
