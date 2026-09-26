#!/usr/bin/env bash
# Platform-agnostic core of "make a k3s node advertise a fake accelerator": patches the
# nvidia.com/gpu extended resource onto an *already-registered* node's status and labels it
# with a model name, via plain kubectl — no assumption about how that node came to exist (a
# nested container on macOS, or a real hardware node like box-a that just has spare
# CPU/memory to also carry fake GPU slots for the portable suite alongside its real DRA
# device). Callers that need a brand-new node (localdev/k3s-macos/dev-nodes-up.sh) create
# the node first, then call this; callers with an existing node just call this directly.
#
# Usage: fake-gpu-node.sh <context> <node> <gpu-count> <vendor-label-value> [cpu-alloc-milli] [mem-alloc-ki]
# The last two are optional — only needed to also cap capacity/allocatable down to a
# container's real cgroup quota (see dev-nodes-up.sh's own comment on why that matters);
# omit them to leave the node's real cpu/memory capacity untouched.
#
# With cpu-alloc-milli the node also publishes a CPU class: cpu.hypothesisloop.dev/core, one unit
# per whole core of that allocation, labelled cpu.hypothesisloop.dev/class=dev-node. A job with
# no accelerator asks for cores of it (a catalog type of kind cpu), since a job that holds no
# priced unit is refused. Without cpu-alloc-milli, the case of a real hardware node, no class is
# published: what that node offers is its operator's to say (the cluster agent's cpuClass).
set -euo pipefail

CONTEXT="$1" NODE="$2" GPU_COUNT="$3" LABEL_VALUE="$4"
CPU_ALLOC_MILLI="${5:-}" MEM_ALLOC_KI="${6:-}"

PATCH="[
  {\"op\":\"add\",\"path\":\"/status/capacity/nvidia.com~1gpu\",\"value\":\"${GPU_COUNT}\"},
  {\"op\":\"add\",\"path\":\"/status/allocatable/nvidia.com~1gpu\",\"value\":\"${GPU_COUNT}\"}"
[[ -n "${CPU_ALLOC_MILLI}" ]] && PATCH+=",{\"op\":\"add\",\"path\":\"/status/allocatable/cpu\",\"value\":\"${CPU_ALLOC_MILLI}m\"}"
[[ -n "${MEM_ALLOC_KI}" ]] && PATCH+=",{\"op\":\"add\",\"path\":\"/status/allocatable/memory\",\"value\":\"${MEM_ALLOC_KI}Ki\"}"
CPU_CLASS_CORES=""
if [[ -n "${CPU_ALLOC_MILLI}" ]]; then
  CPU_CLASS_CORES=$(( CPU_ALLOC_MILLI / 1000 ))
  PATCH+=",{\"op\":\"add\",\"path\":\"/status/capacity/cpu.hypothesisloop.dev~1core\",\"value\":\"${CPU_CLASS_CORES}\"}"
  PATCH+=",{\"op\":\"add\",\"path\":\"/status/allocatable/cpu.hypothesisloop.dev~1core\",\"value\":\"${CPU_CLASS_CORES}\"}"
fi
PATCH+="]"

# "add" on an existing key replaces its value (RFC 6902 semantics for object members), so
# re-running this against an already-patched node is safe.
kubectl --context "${CONTEXT}" patch node "${NODE}" --subresource=status --type=json -p "${PATCH}" >/dev/null
kubectl --context "${CONTEXT}" label node "${NODE}" "nvidia.com/gpu.product=${LABEL_VALUE}" --overwrite >/dev/null
if [[ -n "${CPU_CLASS_CORES}" ]]; then
  kubectl --context "${CONTEXT}" label node "${NODE}" "cpu.hypothesisloop.dev/class=dev-node" --overwrite >/dev/null
fi
echo "    ${NODE}: nvidia.com/gpu=${GPU_COUNT} (${LABEL_VALUE})${CPU_CLASS_CORES:+, cpu.hypothesisloop.dev/core=${CPU_CLASS_CORES} (dev-node)}"
