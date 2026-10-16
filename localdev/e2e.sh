#!/usr/bin/env bash
# e2e.sh [pytest arguments]
#
# Runs the e2e suite against the stack `make dev` runs and the local k3s cluster, and leaves both
# as it found them. With no arguments it runs every test that needs no real accelerator.
#
# It refuses a stack that rents (make dev-hosted). The hosted agent tests ask the API as the
# launcher the control plane gives hosted agents to: against a stack where a real launcher asks
# under that name, each would end the machines the other reported, and an agent a test hosts
# would be rented. A job a test submits could likewise be placed on the shared cloud cluster.
#
# It holds the stack lock while it runs: the scheduler tests need the cluster's whole capacity
# and stop the cluster's agents, so nothing else may use the stack meanwhile.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo}"
# hl-admin, which the tests run, reads the settings the running control plane was started with.
export HYPOTHESISLOOP_CONFIG="${repo}/controlplane/settings/.hypothesisloop.dev.yaml"
. localdev/lib/auth.sh
git_host="$(lib_git_host_provider)"

lock=/tmp/hl-stack.lock
context=k3s-local
api="http://localhost:8081"
registry="$(. localdev/lib/node.sh && lib_registry_publish_ip):5000"

bash localdev/dev.sh require-local
if ! curl -fsS -o /dev/null "${api}/health"; then
  echo "e2e: no control plane answers at ${api}; start it with make dev" >&2
  exit 1
fi

export API_URL="${api}" METRICS_URL="http://localhost:8084"
HL_TOKEN="$(lib_hl_admin token staff)"
export HL_TOKEN

# The suite reads the capacity of every cluster as its own and submits jobs any cluster its org
# may use could take, so the local cluster must be the only connected one that takes them: a job
# placed on another would run on somebody's hardware, or rent some. A cluster that stays
# connected is kept out by turning it off for the suite's org, which places no job there.
others="$(curl -fsS -H "Authorization: Bearer ${HL_TOKEN}" "${api}/orgs/default/clusters" |
  python3 -c 'import json, sys; print(" ".join(c["name"] for c in json.load(sys.stdin)["clusters"] if c["connected"] and c["enabled"] and c["name"] != "local"))')"
if [[ -n "${others}" ]]; then
  echo "e2e: other clusters are connected to this control plane and take the jobs of org default (${others}); the suite's jobs could be placed on them. Stop their agents, or turn each off for the run: hl cluster disable NAME --org default (and hl cluster enable afterwards)" >&2
  exit 1
fi

# The tenancy tests make orgs that own no cluster and read what such an org is told. A cluster the
# platform shares with every org gives each of them one.
shared="$(curl -fsS -H "Authorization: Bearer ${HL_TOKEN}" "${api}/orgs/default/clusters" |
  python3 -c 'import json, sys; print(" ".join(c["name"] for c in json.load(sys.stdin)["clusters"] if c["shared"]))')"
if [[ -n "${shared}" ]]; then
  echo "e2e: the platform shares clusters with every org (${shared}), so no org of the suite is one without a cluster. End the sharing for the run: hl-admin cluster share NAME off" >&2
  exit 1
fi

# The cluster's runtime and the workload the tests' jobs run are images, pushed and installed by
# make reload under one tag. A test job names that tag, so both must be there under it.
if [[ ! -s localdev/.image-tag ]]; then
  echo "e2e: no images were pushed to the dev registry yet; run make reload" >&2
  exit 1
fi
tag="$(cat localdev/.image-tag)"
if ! curl -fsS -o /dev/null -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
    "http://${registry}/v2/hypothesisloop-workload/manifests/${tag}"; then
  echo "e2e: the dev registry holds no hypothesisloop-workload:${tag}; run make reload" >&2
  exit 1
fi
installed="$(kubectl --context "${context}" -n hypothesisloop get deployment/hypothesisloop-cluster-agent \
  -o 'jsonpath={.spec.template.spec.containers[0].image}')"
if [[ "${installed##*:}" != "${tag}" ]]; then
  echo "e2e: cluster ${context} runs ${installed}, and the images last pushed are tagged ${tag}; run make reload" >&2
  exit 1
fi

# The tests call kubectl with no context, so they get a kubeconfig that holds only this cluster.
kubeconfig="$(mktemp)"
nodes_were_up="$(podman ps --filter name=fake- --format '{{.Names}}' | head -1)"
if ! mkdir "${lock}" 2>/dev/null; then
  echo "e2e: the stack is held (${lock}): $(cat "${lock}/owner" 2>/dev/null || echo 'no owner named')" >&2
  exit 1
fi
echo "make e2e (pid $$) since $(date '+%Y-%m-%d %H:%M:%S')" > "${lock}/owner"
cleanup() {
  if [[ -z "${nodes_were_up}" ]]; then
    bash "${repo}/localdev/k3s-macos/dev-nodes-down.sh" || true
  fi
  rm -f "${kubeconfig}"
  rm -rf "${lock}"
}
trap cleanup EXIT
kubectl config view --minify --flatten --context "${context}" > "${kubeconfig}"
export KUBECONFIG="${kubeconfig}"

bash localdev/k3s-macos/dev-nodes-up.sh
# A node that just joined is in the control plane's catalog only once the cluster's agent has
# reported it. A test that started before that read a catalog short of the nodes kubectl lists.
kubectl -n hypothesisloop rollout status daemonset/hypothesisloop-node-agent --timeout=180s
installed="$(kubectl get nodes -o json | python3 -c 'import json, sys; print(sum(int(n["status"]["allocatable"].get("nvidia.com/gpu", 0)) for n in json.load(sys.stdin)["items"]))')"
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

# The tests that work on a real repository run when .env names the sandbox and the stack's git
# host is the GitHub App that reaches it. They skip themselves otherwise.
if [[ "${git_host}" == "github" && -n "$(lib_env_value TEST_GIT_HOST_GITHUB_REPOSITORY)" ]]; then
  TEST_GIT_HOST_GITHUB_INSTALLATION="$(lib_env_value TEST_GIT_HOST_GITHUB_INSTALLATION)"
  TEST_GIT_HOST_GITHUB_REPOSITORY="$(lib_env_value TEST_GIT_HOST_GITHUB_REPOSITORY)"
  export TEST_GIT_HOST_GITHUB_INSTALLATION TEST_GIT_HOST_GITHUB_REPOSITORY
fi

# On a stack with the local git host the suite makes its repository where that server serves from.
if [[ "${git_host}" == "local" ]]; then
  export HL_E2E_GIT_ROOT="${HL_GIT_ROOT}"
fi

if [[ $# -eq 0 ]]; then
  set -- e2e -m "not hardware and not real_providers"
fi
cd tests
uv run pytest "$@"
