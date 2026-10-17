#!/usr/bin/env bash
# e2e.sh [pytest arguments]
#
# Runs the e2e suite against the stack `make dev` runs and the local k3s cluster, and leaves both
# as it found them. With no arguments it runs every test that needs no real accelerator and rents
# nothing.
#
# The tests that rent machines at real vendors (marker real_providers) run only with E2E_RENT=1,
# and with no arguments then they are all it runs. Without it every job of the run goes to the
# local cluster: no other cluster takes a job of org default while the run lasts.
#
# It holds the stack lock while it runs, as it changes which clusters take jobs.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo}"
# hl-admin, which the tests run, reads the settings the running control plane was started with.
export HYPOTHESISLOOP_CONFIG="${repo}/controlplane/settings/.hypothesisloop.dev.yaml"
. localdev/lib/auth.sh
git_host="$(lib_git_host_provider)"

lock=/tmp/hl-stack.lock
context=k3s-local
api="http://localhost:${HL_API_PORT}"

rent="${E2E_RENT:-}"
if ! curl -fsS -o /dev/null "${api}/health"; then
  echo "e2e: no control plane answers at ${api}; start it with make dev" >&2
  exit 1
fi

export API_URL="${api}" METRICS_URL="http://localhost:${HL_METRICS_PORT}"
HL_TOKEN="$(lib_hl_admin token staff)"
export HL_TOKEN

nodes_were_up="$(podman ps --filter name=fake- --format '{{.Names}}' | head -1)"
if ! mkdir "${lock}" 2>/dev/null; then
  echo "e2e: the stack is held (${lock}): $(cat "${lock}/owner" 2>/dev/null || echo 'no owner named')" >&2
  exit 1
fi
echo "make e2e (pid $$) since $(date '+%Y-%m-%d %H:%M:%S')" > "${lock}/owner"
turned_off=""
cleanup() {
  for name in ${turned_off}; do
    lib_default_org_cluster "${name}" enable || echo "e2e: could not turn ${name} on again for org default" >&2
  done
  if [[ -z "${nodes_were_up}" ]]; then
    bash "${repo}/localdev/k3s-macos/dev-nodes-down.sh" || true
  fi
  rm -rf "${lock}"
}
trap cleanup EXIT

# A job of the run placed on another cluster would run on somebody's hardware or rent a machine.
# So unless the run rents, every cluster but local is turned off for org default while it lasts.
if [[ -z "${rent}" ]]; then
  clusters="$(curl -fsS -H "Authorization: Bearer ${HL_TOKEN}" "${api}/orgs/default/clusters")"
  turned_off="$(python3 -c 'import json, sys; print(*(c["name"] for c in json.load(sys.stdin)["clusters"] if c["name"] != "local" and c["enabled"]))' <<<"${clusters}")"
  for name in ${turned_off}; do
    lib_default_org_cluster "${name}" disable
  done
fi

# The cluster's runtime is images, pushed and installed by make reload under one tag.
if [[ ! -s localdev/.image-tag ]]; then
  echo "e2e: no images were pushed to the dev registry yet; run make reload" >&2
  exit 1
fi
tag="$(cat localdev/.image-tag)"
installed="$(kubectl --context "${context}" -n hypothesisloop get deployment/hypothesisloop-cluster-agent \
  -o 'jsonpath={.spec.template.spec.containers[0].image}')"
if [[ "${installed##*:}" != "${tag}" ]]; then
  echo "e2e: cluster ${context} runs ${installed}, and the images last pushed are tagged ${tag}; run make reload" >&2
  exit 1
fi

bash localdev/k3s-macos/dev-nodes-up.sh
# A node that just joined is in the control plane's catalog only once the cluster's agent has
# reported it. A test that started before that read a catalog short of the nodes kubectl lists.
kubectl --context "${context}" -n hypothesisloop rollout status daemonset/hypothesisloop-node-agent --timeout=180s
installed="$(kubectl --context "${context}" get nodes -o json | python3 -c 'import json, sys; print(sum(int(n["status"]["allocatable"].get("nvidia.com/gpu", 0)) for n in json.load(sys.stdin)["items"]))')"
reported=""
for _ in $(seq 1 60); do
  reported="$(curl -fsS -H "Authorization: Bearer ${HL_TOKEN}" "${api}/resource-catalog/capacity" |
    python3 -c 'import json, sys; print(sum(a["total"] for c in json.load(sys.stdin)["clusters"] if c["cluster_name"] == "local" for a in c["accelerators"] or [] if a["kind"] == "accelerator"))')"
  [[ "${reported}" == "${installed}" ]] && break
  sleep 2
done
if [[ "${reported}" != "${installed}" ]]; then
  echo "e2e: the nodes of ${context} hold ${installed} accelerators and the control plane's catalog reports ${reported} for cluster local after two minutes" >&2
  exit 1
fi

# The tests that work on a real repository run when the environment or .env names the sandbox
# and the stack's git host is the GitHub App that reaches it. They skip themselves otherwise.
if [[ "${git_host}" == "github" ]]; then
  TEST_GIT_HOST_GITHUB_INSTALLATION="${TEST_GIT_HOST_GITHUB_INSTALLATION:-$(lib_env_value TEST_GIT_HOST_GITHUB_INSTALLATION)}"
  TEST_GIT_HOST_GITHUB_REPOSITORY="${TEST_GIT_HOST_GITHUB_REPOSITORY:-$(lib_env_value TEST_GIT_HOST_GITHUB_REPOSITORY)}"
  export TEST_GIT_HOST_GITHUB_INSTALLATION TEST_GIT_HOST_GITHUB_REPOSITORY
fi

# On a stack with the local git host the suite makes its repository where that server serves from.
if [[ "${git_host}" == "plain" ]]; then
  export HL_E2E_GIT_ROOT="${HL_GIT_ROOT}"
fi

if [[ $# -eq 0 && -n "${rent}" ]]; then
  set -- e2e -m real_providers
elif [[ $# -eq 0 ]]; then
  set -- e2e -m "not hardware and not real_providers"
fi
cd tests
HL_E2E_RENT="${rent}" uv run pytest "$@"
