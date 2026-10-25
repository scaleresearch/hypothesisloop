#!/usr/bin/env bash
# dev.sh up|api|ui|status|down
#
# Runs the control plane and the UI as host processes against the compose datastores, so a code
# change needs a restart of one process and no image build. Every process is started detached,
# with its pid and its log in localdev/.run, so one can be restarted while the rest keep running.
#
#   up       render the settings and (re)start the API and metrics-service, building hl beside
#            them in localdev/.bin; start the tunnels that are down, the UI unless it runs, and the
#            git server when GIT_HOST_PROVIDER is plain. The settings rent compute: a sandbox
#            section on the provider SANDBOX_PROVIDER in .env names, for hosted agents, and a
#            cloud_compute entry on each provider CLOUD_COMPUTE_PROVIDERS lists (vast and runpod
#            unless it says otherwise), for jobs, all run by control-service itself. They name the
#            tunnels and the images under IMAGE_TAG in the registry CONTAINER_REGISTRY_PROVIDER
#            names. Its inputs are keys of .env, and every
#            missing one is named before anything is touched
#   api      rebuild and restart the API and metrics-service with the settings as they were last
#            rendered. Everything else keeps running
#   ui       (re)start the UI, which otherwise reloads by itself
#   status   which processes run
#   down     stop them all. The tunnels are not part of this (make tunnels-down)
set -euo pipefail

action="${1:?usage: dev.sh up|api|ui|status|down}"
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo}"
. localdev/lib/auth.sh
git_host="$(lib_git_host_provider)"

# With LOCAL_REACHABLE_ADDRESS in .env the registry is published on that address too
# (REGISTRY_REACHABLE_IP, from make), so other machines pull from it.
compose=(-f localdev/controlplane/docker-compose.yml)
if [[ -n "${REGISTRY_REACHABLE_IP:-}" ]]; then
  compose+=(-f localdev/controlplane/docker-compose.reachable.yml)
fi
rendered=controlplane/settings/.hypothesisloop.rendered.yaml
settings=controlplane/settings/.hypothesisloop.dev.yaml
bin=localdev/.bin
run=localdev/.run
processes=(control-service metrics-service ui git-server)
rented="${bin}/compute.yaml"
# The compute providers jobs are rented machines on, each a cluster every org sees.
cloud_providers=()
if [[ -f .env ]]; then
  mapfile -t cloud_providers < <(lib_cloud_providers)
fi

mkdir -p "${bin}" "${run}"
export HYPOTHESISLOOP_CONFIG="${repo}/${settings}"

running() {
  [[ -f "${run}/$1.pid" ]] && kill -0 "$(cat "${run}/$1.pid")" 2>/dev/null
}

stop() {
  local name="$1" pid
  if running "${name}"; then
    pid="$(cat "${run}/${name}.pid")"
    # next dev serves from a child of the process started here.
    pkill -TERM -P "${pid}" 2>/dev/null || true
    kill "${pid}" 2>/dev/null || true
    # control-service asks again for the destroys a vendor refused before it ends, for up to 20 s.
    for _ in $(seq 1 125); do
      kill -0 "${pid}" 2>/dev/null || break
      sleep 0.2
    done
    kill -9 "${pid}" 2>/dev/null || true
  fi
  rm -f "${run}/${name}.pid"
}

# start NAME COMMAND...: the command detached, in the environment of the caller. COMMAND may be
# lib_run (localdev/lib/env.sh).
start() {
  local name="$1"
  shift
  stop "${name}"
  local log="${run}/${name}.log"
  # Restarts append; a log past 50 MB moves to .log.1, replacing the previous one.
  if [[ -f "${log}" ]] && (( $(wc -c <"${log}") > 50 * 1024 * 1024 )); then
    mv -f "${log}" "${log}.1"
  fi
  echo "=== dev: ${name} starting $(date -u +%Y-%m-%dT%H:%M:%SZ) ===" >>"${log}"
  nohup bash -c '. "$0" && "$@"' "${repo}/localdev/lib/env.sh" "$@" >>"${log}" 2>&1 &
  echo $! >"${run}/${name}.pid"
}

# A process this script did not start holds no pid file, and stopping it is not this script's
# call: the port's holder is named and nothing is started over it.
require_port() {
  local port="$1" holder
  holder="$(lsof -nP -t -iTCP:"${port}" -sTCP:LISTEN 2>/dev/null | head -1 || true)"
  if [[ -n "${holder}" ]]; then
    echo "dev: port ${port} is held by process ${holder}, which make dev did not start; stop it first" >&2
    exit 1
  fi
}

wait_for() {
  local name="$1" url="$2"
  until curl -fsS -o /dev/null "${url}" 2>/dev/null; do
    if ! running "${name}"; then
      echo "dev: ${name} did not start:" >&2
      tail -20 "${run}/${name}.log" >&2
      exit 1
    fi
    sleep 0.2
  done
}

must_stay_up() {
  sleep 3
  if ! running "$1"; then
    echo "dev: $1 stopped right after it started:" >&2
    tail -20 "${run}/$1.log" >&2
    exit 1
  fi
}

# The settings name where a hosted agent and a rented instance reach this machine and which
# images they boot. make render-settings writes the host's address and a placeholder image, which
# become the tunnels' addresses and the published images.
render_settings() {
  local values=("  control_plane_endpoint: http://localhost:9000") value host public=()
  host="$(lib_advertised_address "$(cat localdev/.host-address)")"
  public=(-e "s|http://${host}:${HL_API_PORT}|${public_api}|"
          -e "s|http://${host}:${HL_METRICS_PORT}|${public_metrics}|"
          -e "s|http://${host}:9000|${public_store}|"
          -e "s|^  image: ${host}:5000/hypothesisloop-harness-base:.*$|  image: ${registry}/hypothesisloop-harness-base:${tag}|"
          -e "s|^  registry: ${host}:5000$|  registry: ${registry}|"
          -e "s|^  registry_plain_http: true$|  registry_plain_http: false|"
          -e "s|^  image_tag: .*$|  image_tag: ${tag}|")
  if [[ "${git_host}" == "plain" ]]; then
    public+=(-e "s|^    base_url: http://${host}:${HL_GIT_PORT}$|    base_url: ${public_git}|")
    values+=("    base_url: ${public_git}")
  fi
  values+=("  public_api_url: ${public_api}" "  metrics_url: ${public_metrics}" "  endpoint: ${public_store}" "  image: ${registry}/hypothesisloop-harness-base:${tag}"
           "  registry: ${registry}" "  image_tag: ${tag}" "  registry_plain_http: false")
  sed -e 's|http://minio:9000|http://localhost:9000|' "${public[@]}" "${rendered}" > "${settings}"
  for value in "${values[@]}"; do
    grep -qF -- "${value}" "${settings}" || { echo "dev: the settings file changed shape, '${value}' was not rendered" >&2; exit 1; }
  done
}

# The sandbox and cloud_compute sections are the examples the core ships, the sandbox on the
# provider .env names and one cloud_compute entry per provider of cloud_providers, each with its
# values from .env (lib_rented), appended to the settings. Each provider's key reaches
# control-service through its environment file.
render_compute() {
  local value sandbox cloud provider pairs=() expected=("  provider: ${sandbox_provider}")
  sandbox="$(lib_rented controlplane/settings/sandbox.yaml "${sandbox_provider}")"
  for provider in "${cloud_providers[@]}"; do
    pairs+=("${provider}" "$(lib_cloud_cluster "${provider}")")
    expected+=("  - cluster_name: $(lib_cloud_cluster "${provider}")" "    provider: ${provider}")
  done
  cloud="$(lib_rented controlplane/settings/cloud-compute.yaml "${pairs[@]}")"
  printf '%s\n\n%s\n' "${sandbox}" "${cloud}" > "${rented}"
  for value in "${expected[@]}"; do
    grep -qF -- "${value}" "${rented}" || { echo "dev: controlplane/settings/sandbox.yaml or cloud-compute.yaml changed shape, '${value}' was not rendered" >&2; exit 1; }
  done
}

append_compute() {
  printf '\n' >> "${settings}"
  cat "${rented}" >> "${settings}"
}

# Each provider of .env is one its role knows, and each key that provider reads is set: the
# rented ones are checked as their sections are rendered (localdev/rented).
inputs() {
  local missing=() key tunnels provider_keys=""
  sandbox_provider="$(lib_env_value SANDBOX_PROVIDER)"
  # The settings' default models are anthropic's, and MODEL_OPENROUTER_API_KEY and
  # MODEL_OPENAI_AUTH_JSON (the content of ~/.codex/auth.json) are optional.
  provider_keys+=" MODEL_ANTHROPIC_OAUTH_TOKEN"
  if [[ -z "${HL_REGISTRY}" ]]; then
    echo "dev: .env names no CONTAINER_REGISTRY_PROVIDER, the registry the images under IMAGE_TAG are in" >&2
    exit 1
  fi
  for key in IMAGE_TAG ${provider_keys}; do
    [[ -n "$(lib_env_value "${key}")" ]] || missing+=("${key}")
  done
  if [[ ${#missing[@]} -gt 0 ]]; then
    echo "dev: .env does not set ${missing[*]} (.env.example says what each is)" >&2
    exit 1
  fi
  registry="${HL_REGISTRY}"
  tag="$(lib_env_value IMAGE_TAG)"
  render_compute
  bash localdev/local-tunnels/tunnels.sh up
  tunnels="$(bash localdev/local-tunnels/tunnels.sh addresses)"
  public_api="$(awk '$1 == "api" { print $2 }' <<<"${tunnels}")"
  public_metrics="$(awk '$1 == "metric-controller" { print $2 }' <<<"${tunnels}")"
  public_store="$(awk '$1 == "object-store" { print $2 }' <<<"${tunnels}")"
  public_git="$(awk '$1 == "git" { print $2 }' <<<"${tunnels}")"
}

require_settings() {
  if [[ ! -f "${settings}" ]]; then
    echo "dev: no settings rendered yet; run make dev first" >&2
    exit 1
  fi
}

start_control_plane() {
  require_settings
  # Built before anything is stopped: a tree that does not compile leaves what runs running.
  (cd controlplane && go build -o "${repo}/${bin}/" ./cmd/control-service ./cmd/metrics-service)
  # hl beside them, built from the same tree, so localdev/.bin/hl never lags the API it calls.
  # go build relinks only what changed.
  go build -o "${repo}/${bin}/hl" ./cli/cmd/hl
  # Each process is started with the keys these settings call for.
  lib_env_files "${settings}"
  # Checked before anything is stopped too: settings rendered before this tree made a key
  # required do not load, and the API would be stopped with nothing to start in its place.
  if ! lib_hl_admin token staff >/dev/null; then
    echo "dev: ${settings} does not load with this tree; render it again with make dev" >&2
    exit 1
  fi
  stop control-service
  stop metrics-service
  require_port "${HL_API_PORT}"
  require_port "${HL_METRICS_PORT}"
  start control-service lib_run control-service "${bin}/control-service"
  start metrics-service lib_run metrics-service "${bin}/metrics-service"
  wait_for control-service "http://localhost:${HL_API_PORT}/health"
  wait_for metrics-service "http://localhost:${HL_METRICS_PORT}/health"
  lib_register_clusters
}

# The git host of a stack whose GIT_HOST_PROVIDER is plain. Any other runs none.
start_git_server() {
  if [[ "${git_host}" != "plain" ]]; then
    stop git-server
    return
  fi
  go build -o "${repo}/${bin}/git-server" ./localdev/local-git
  mkdir -p "${HL_GIT_ROOT}"
  if [[ ! -d "${HL_GIT_ROOT}/${HL_GIT_EXPERIMENT_TEMPLATE}" ]]; then
    git clone --quiet --bare "${HL_EXPERIMENT_TEMPLATE_URL}" "${HL_GIT_ROOT}/${HL_GIT_EXPERIMENT_TEMPLATE}"
  else
    # A bare clone has no fetch refspec, so the branches are named. Offline, the copy stays as it is.
    git -C "${HL_GIT_ROOT}/${HL_GIT_EXPERIMENT_TEMPLATE}" fetch --quiet --prune "${HL_EXPERIMENT_TEMPLATE_URL}" \
      '+refs/heads/*:refs/heads/*' 2>/dev/null || true
  fi
  stop git-server
  require_port "${HL_GIT_PORT}"
  GIT_SERVER_ADDRESS=":${HL_GIT_PORT}" GIT_SERVER_ROOT="${HL_GIT_ROOT}" GIT_SERVER_USERNAME="${HL_GIT_USERNAME}" \
    GIT_HOST_PLAIN_PASSWORD="$(<"${HL_GIT_PASSWORD_FILE}")" start git-server "${bin}/git-server"
  wait_for git-server "http://localhost:${HL_GIT_PORT}/health"
}

start_datastores() {
  podman compose "${compose[@]}" stop control-service metrics-service >/dev/null 2>&1 || true
  # Every checkout's compose project has the same name, so a container may have been made from
  # another checkout's paths. compose replaces one whose definition differs from this tree's; the
  # data is in named volumes and stays.
  local out
  if ! out="$(REGISTRY=unused TAG=unused podman compose "${compose[@]}" up -d registry postgres memgraph greptimedb minio 2>&1)"; then
    printf '%s\n' "${out}" >&2
    exit 1
  fi
}

start_ui() {
  if running ui; then
    return
  fi
  if [[ -n "$(lsof -nP -t -iTCP:3000 -sTCP:LISTEN 2>/dev/null | head -1 || true)" ]]; then
    echo "dev: something already serves port 3000 and is left as it is"
    return
  fi
  # Node 22.18+ can load tailwind.config.ts natively as ESM, a module Tailwind cannot evict to
  # reload it, and then config edits stop reaching the served CSS until a restart. Without type
  # stripping Tailwind always loads it through jiti, which it does reload.
  start ui lib_run ui env NODE_OPTIONS=--no-experimental-strip-types npm --prefix controlplane/ui run dev
}

status() {
  local name
  for name in "${processes[@]}"; do
    if running "${name}"; then
      echo "  ${name}: running (pid $(cat "${run}/${name}.pid"), log ${run}/${name}.log)"
    else
      echo "  ${name}: not running"
    fi
  done
}

case "${action}" in
  up)
    inputs
    render_settings
    append_compute
    start_datastores
    start_git_server
    start_control_plane
    # Every cluster takes the jobs of org default, as placement chooses between them: the k3s
    # cluster on this machine and each cloud cluster. All belong to the org every schema starts
    # with, the platform's, so every org's jobs are placed on them. control-service registers each
    # cloud cluster itself as it starts. Each is turned on again for org default, which an e2e run
    # stopped half way may have left off.
    lib_register_local_cluster
    lib_default_org_cluster local enable
    for provider in "${cloud_providers[@]}"; do
      lib_default_org_cluster "$(lib_cloud_cluster "${provider}")" enable
    done
    must_stay_up control-service
    start_ui
    echo "dev: API http://localhost:${HL_API_PORT} (from outside ${public_api}), UI http://localhost:3000"
    echo "dev: control-service rents jobs for $(for p in "${cloud_providers[@]}"; do printf '%s ' "$(lib_cloud_cluster "${p}")"; done)and runs hosted agents on ${sandbox_provider}"
    status
    ;;
  api)
    start_control_plane
    status
    ;;
  ui)
    require_settings
    # Written afresh so the restarted UI serves with what .env says now.
    lib_env_files "${settings}"
    stop ui
    start_ui
    status
    ;;
  status)
    status
    ;;
  down)
    for name in "${processes[@]}"; do
      stop "${name}"
    done
    ;;
  *)
    echo "usage: dev.sh up|api|ui|status|down" >&2
    exit 1
    ;;
esac
