#!/usr/bin/env bash
# dev.sh up|hosted|api|ui|status|require-local|down
#
# Runs the control plane and the UI as host processes against the compose datastores, so a code
# change needs a restart of one process and no image build. Every process is started detached,
# with its pid and its log in localdev/.run, so one can be restarted while the rest keep running.
#
#   up       render the settings for the local cluster and (re)start the API and metrics-service,
#            building hl beside them in localdev/.bin;
#            start the UI unless it runs, and the git server when GIT_HOST_PROVIDER is local
#   hosted   the same with what rents compute elsewhere: agent-launcher, on the sandbox provider
#            SANDBOX_PROVIDER in .env names, and cloud-agent, on the compute provider
#            CLOUD_COMPUTE_PROVIDER names, as two more processes, and settings that name the
#            running tunnels (make tunnels) and the images under IMAGE_TAG in the registry
#            CONTAINER_REGISTRY_PROVIDER names. Its inputs are keys of .env, and every missing one is
#            named before anything is touched
#   api      rebuild and restart the API and metrics-service with the settings as they were last
#            rendered. Everything else keeps running
#   ui       (re)start the UI, which otherwise reloads by itself
#   status   which processes run
#   require-local  fail, naming them, while the processes that rent compute run. What must
#            never reach a provider (the e2e suite) asks this first
#   down     stop them all. The tunnels are not part of this (make tunnels-down)
set -euo pipefail

action="${1:?usage: dev.sh up|hosted|api|ui|status|require-local|down}"
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
processes=(control-service metrics-service ui git-server cloud-agent agent-launcher)
# The compute provider cloud-agent rents from, and the name every org sees that compute under.
# Empty when .env names none.
cloud_provider="$( [[ ! -f .env ]] || lib_env_value CLOUD_COMPUTE_PROVIDER)"
cloud_cluster="${cloud_provider:+hypothesisloop-${cloud_provider}}"

mkdir -p "${bin}" "${run}"
export HYPOTHESISLOOP_CONFIG="${repo}/${settings}"
export DATABASE_URL="postgres://hypothesisloop:hypothesisloop@localhost:5433/hypothesisloop?sslmode=disable"

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
    # cloud-agent asks again for the destroys a vendor refused before it ends, for up to 20 s.
    for _ in $(seq 1 125); do
      kill -0 "${pid}" 2>/dev/null || break
      sleep 0.2
    done
    kill -9 "${pid}" 2>/dev/null || true
  fi
  rm -f "${run}/${name}.pid"
}

# start NAME COMMAND...: the command detached, in the environment of the caller.
start() {
  local name="$1"
  shift
  stop "${name}"
  nohup "$@" >"${run}/${name}.log" 2>&1 &
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
# images they boot. Rendered for the local cluster these are the host's address and a
# placeholder image, so hosted mode writes the tunnels' addresses and the published images.
render_settings() {
  local hosted=() values=("http://localhost:4010" "bolt://localhost:7687" "  control_plane_endpoint: http://localhost:9000") value host
  if [[ "${1:-}" == "hosted" ]]; then
    host="$(lib_advertised_address "$(cat localdev/.host-address)")"
    hosted=(-e "s|http://${host}:8081|${public_api}|"
            -e "s|http://${host}:8084|${public_metrics}|"
            -e "s|http://${host}:9000|${public_store}|"
            -e "s|^  image: registry.example.invalid/hypothesisloop-experimentator-base:latest$|  image: ${registry}/hypothesisloop-experimentator-base:${tag}|"
            -e "s|^  registry: ${host}:5000$|  registry: ${registry}|"
            -e "s|^  registry_plain_http: true$|  registry_plain_http: false|"
            -e "s|^  image_tag: .*$|  image_tag: ${tag}|")
    if [[ "${git_host}" == "local" ]]; then
      hosted+=(-e "s|^    base_url: http://${host}:${HL_GIT_PORT}$|    base_url: ${public_git}|")
      values+=("    base_url: ${public_git}")
    fi
    values+=("  public_api_url: ${public_api}" "  proxy_url: ${public_api}/git" "  metrics_url: ${public_metrics}" "  endpoint: ${public_store}" "  image: ${registry}/hypothesisloop-experimentator-base:${tag}"
             "  registry: ${registry}" "  image_tag: ${tag}" "  registry_plain_http: false")
  fi
  sed -e 's|http://greptimedb:4000|http://localhost:4010|' \
      -e 's|bolt://memgraph:7687|bolt://localhost:7687|' \
      -e 's|http://minio:9000|http://localhost:9000|' ${hosted[@]+"${hosted[@]}"} "${rendered}" > "${settings}"
  for value in "${values[@]}"; do
    grep -qF -- "${value}" "${settings}" || { echo "dev: the settings file changed shape, '${value}' was not rendered" >&2; exit 1; }
  done
}

# Each runtime process reads one config file: the example the core ships, with this deployment's
# addresses, images and accounts in it. A secret is named there and handed over in the
# environment of the one process that needs it.
render_runtime_configs() {
  local value
  # Which vendor rents is the provider section of each config and nothing else: the control
  # plane, the harness and the processes' own loops are the same for every one.
  sed -e "s|^cluster_name: .*$|cluster_name: ${cloud_cluster}|" \
      -e "s|^api_url: .*$|api_url: ${public_api}|" \
      -e "s|^job_image: .*$|job_image: ${registry}/hypothesisloop-job:${tag}|" \
      -e "s|^environments_repository: .*$|environments_repository: ${HL_ENVIRONMENTS_REPOSITORY}|" \
      -e "s|^  account_id: .*$|  account_id: \"$(lib_env_value CLOUD_COMPUTE_VAST_ACCOUNT_ID)\"|" \
      "runtime/cloud/settings/cloud-agent.${cloud_provider}.yaml" > "${bin}/cloud-agent.yaml"
  local sandbox_settings
  case "${sandbox_provider}" in
    fly) sandbox_settings="{api_token_env: FLY_API_TOKEN, app: $(lib_env_value SANDBOX_FLY_APP), region: ams, cpu_kind: performance}" ;;
    e2b) sandbox_settings="{api_key_env: E2B_API_KEY, lifetime_seconds: 3600}" ;;
  esac
  sed -e "s|^api_url: .*$|api_url: http://localhost:8081|" \
      -e "s|^  name: example$|  name: ${sandbox_provider}|" \
      -e "s|^  settings: {}$|  settings: ${sandbox_settings}|" \
      runtime/cloud/settings/agent-launcher.yaml > "${bin}/agent-launcher.yaml"
  for value in "cluster_name: ${cloud_cluster}" "api_url: ${public_api}" "job_image: ${registry}/hypothesisloop-job:${tag}" "environments_repository: ${HL_ENVIRONMENTS_REPOSITORY}" "account_id: \"$(lib_env_value CLOUD_COMPUTE_VAST_ACCOUNT_ID)\""; do
    grep -qF -- "${value}" "${bin}/cloud-agent.yaml" || { echo "dev: runtime/cloud/settings/cloud-agent.${cloud_provider}.yaml changed shape, '${value}' was not rendered" >&2; exit 1; }
  done
  for value in "api_url: http://localhost:8081" "  name: ${sandbox_provider}" "  settings: ${sandbox_settings}"; do
    grep -qF -- "${value}" "${bin}/agent-launcher.yaml" || { echo "dev: runtime/cloud/settings/agent-launcher.yaml changed shape, '${value}' was not rendered" >&2; exit 1; }
  done
  launcher="$(sed -n 's/^launcher_name: *//p' "${bin}/agent-launcher.yaml")"
}

# Each provider of .env is one its role knows, and each key that provider reads is set.
hosted_inputs() {
  local missing=() key tunnels provider_keys
  sandbox_provider="$(lib_env_value SANDBOX_PROVIDER)"
  case "${sandbox_provider}" in
    fly) provider_keys="SANDBOX_FLY_APP SANDBOX_FLY_DEPLOY_TOKEN" ;;
    e2b) provider_keys="SANDBOX_E2B_API_KEY" ;;
    *)
      echo "dev: .env sets SANDBOX_PROVIDER to '${sandbox_provider}', and hosted agents run on fly or e2b" >&2
      exit 1
      ;;
  esac
  case "${cloud_provider}" in
    vast) provider_keys+=" CLOUD_COMPUTE_VAST_API_KEY CLOUD_COMPUTE_VAST_ACCOUNT_ID" ;;
    *)
      echo "dev: .env sets CLOUD_COMPUTE_PROVIDER to '${cloud_provider}', and cloud-agent rents on vast" >&2
      exit 1
      ;;
  esac
  case "$(lib_env_value MODEL_PROVIDER)" in
    anthropic) provider_keys+=" MODEL_ANTHROPIC_OAUTH_TOKEN" ;;
    *)
      echo "dev: .env sets MODEL_PROVIDER to '$(lib_env_value MODEL_PROVIDER)', and hosted agents run on anthropic" >&2
      exit 1
      ;;
  esac
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
  tunnels="$(bash localdev/local-tunnels/tunnels.sh addresses)"
  public_api="$(awk '$1 == "api" { print $2 }' <<<"${tunnels}")"
  public_metrics="$(awk '$1 == "metric-controller" { print $2 }' <<<"${tunnels}")"
  public_store="$(awk '$1 == "object-store" { print $2 }' <<<"${tunnels}")"
  public_git="$(awk '$1 == "git" { print $2 }' <<<"${tunnels}")"
  registry="${HL_REGISTRY}"
  tag="$(lib_env_value IMAGE_TAG)"
}

start_control_plane() {
  if [[ ! -f "${settings}" ]]; then
    echo "dev: no settings rendered yet; run make dev or make dev-hosted first" >&2
    exit 1
  fi
  # Built before anything is stopped: a tree that does not compile leaves what runs running.
  (cd controlplane && go build -o "${repo}/${bin}/" ./cmd/control-service ./cmd/metrics-service)
  # hl beside them, built from the same tree, so localdev/.bin/hl never lags the API it calls.
  # go build relinks only what changed.
  go build -o "${repo}/${bin}/hl" ./cli
  # Checked before anything is stopped too: settings rendered before this tree made a key
  # required do not load, and the API would be stopped with nothing to start in its place.
  if ! lib_hl_admin token staff >/dev/null; then
    echo "dev: ${settings} does not load with this tree; render it again with make dev or make dev-hosted" >&2
    exit 1
  fi
  stop control-service
  stop metrics-service
  require_port 8081
  require_port 8084
  start control-service "${bin}/control-service"
  start metrics-service "${bin}/metrics-service"
  wait_for control-service http://localhost:8081/health
  wait_for metrics-service http://localhost:8084/health
  lib_register_clusters
}

# unregister_launcher: a stack that runs no agent-launcher has none registered, so the API reports
# hosting unavailable and refuses hosted work up front. A launcher that still has machines or
# hosted agents to take down is refused removal: while it runs it keeps running, and the dev
# switch fails; once it is gone it stays registered and silent, which the API reports too.
unregister_launcher() {
  local name out
  name="$(sed -n 's/^launcher_name: *//p' runtime/cloud/settings/agent-launcher.yaml)"
  if out="$(lib_hl_admin launcher remove "${name}" 2>&1)"; then
    echo "dev: launcher ${name} unregistered: this stack runs no agent-launcher, so hosted agents are unavailable"
    return
  fi
  if [[ "${out}" == *"no launcher ${name} is registered"* ]]; then
    return
  fi
  if running agent-launcher; then
    echo "dev: agent-launcher ${name} still has hosted work, so it keeps running (close its experiments, or stay on make dev-hosted): ${out}" >&2
    exit 1
  fi
  echo "dev: launcher ${name} stays registered and runs nowhere, so the API reports it silent: ${out}" >&2
}

# local_cluster enable|disable: whether org default places jobs on the cluster on this machine
# (lib_register_local_cluster). A database an earlier make dev used has it registered, so a hosted
# stack turns it off; one that never registered it answers 404 and has nothing to turn off.
local_cluster() {
  local code
  code="$(printf 'Authorization: Bearer %s\n' "$(lib_hl_admin token staff)" |
    curl -sS -o /dev/null -w '%{http_code}' -X POST -H @- "http://localhost:8081/orgs/default/clusters/local/$1")"
  if [[ "${code}" == "200" || ( "$1" == "disable" && "${code}" == "404" ) ]]; then
    return
  fi
  echo "dev: could not $1 cluster local for org default: HTTP ${code}" >&2
  exit 1
}

# The git host of a stack whose GIT_HOST_PROVIDER is local. Any other runs none.
start_git_server() {
  if [[ "${git_host}" != "local" ]]; then
    stop git-server
    return
  fi
  go build -o "${repo}/${bin}/git-server" ./localdev/local-git
  mkdir -p "${HL_GIT_ROOT}"
  stop git-server
  require_port "${HL_GIT_PORT}"
  GIT_SERVER_ADDRESS=":${HL_GIT_PORT}" GIT_SERVER_ROOT="${HL_GIT_ROOT}" GIT_SERVER_USERNAME="${HL_GIT_USERNAME}" \
    GIT_SERVER_PASSWORD="${HYPOTHESISLOOP_GIT_HOST_PLAIN_PASSWORD}" start git-server "${bin}/git-server"
  wait_for git-server "http://localhost:${HL_GIT_PORT}/health"
}

start_datastores() {
  podman compose "${compose[@]}" stop control-service metrics-service >/dev/null 2>&1 || true
  # Every checkout's compose project has the same name, so a container may have been made from
  # another checkout's paths. compose replaces one whose definition differs from this tree's; the
  # data is in named volumes and stays.
  REGISTRY=unused TAG=unused podman compose "${compose[@]}" up -d registry postgres memgraph greptimedb minio minio-init >/dev/null
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
  start ui env NODE_OPTIONS=--no-experimental-strip-types npm --prefix controlplane/ui run dev
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
    # Asked first while the launcher runs, so a refusal leaves the hosted stack as it was.
    if running agent-launcher; then
      unregister_launcher
    fi
    render_settings
    start_datastores
    start_git_server
    start_control_plane
    # Local settings give hosted agents no address they could reach, so nothing rents for them.
    unregister_launcher
    stop cloud-agent
    stop agent-launcher
    lib_register_local_cluster
    local_cluster enable
    # Nor is the cloud cluster offered to every org while nothing can rent for it: hosted mode
    # shares it again. A job placed on it goes back to the queue.
    if [[ -n "${cloud_cluster}" ]]; then
      lib_hl_admin cluster add "${cloud_cluster}" default >/dev/null
      lib_hl_admin cluster share "${cloud_cluster}" off >/dev/null
    fi
    start_ui
    echo "dev: API http://localhost:8081, UI http://localhost:3000"
    reachable="$(lib_reachable_address)"
    if [[ -n "${reachable}" ]]; then
      echo "dev: clusters on other machines are handed ${reachable} (LOCAL_REACHABLE_ADDRESS): API :8081, metrics :8084, registry :5000 (plain HTTP), object store :9000"
    fi
    status
    ;;
  hosted)
    hosted_inputs
    render_settings hosted
    render_runtime_configs
    go build -o "${repo}/${bin}/" ./runtime/cloud/cmd/cloud-agent ./runtime/cloud/cmd/agent-launcher
    start_datastores
    start_git_server
    start_control_plane
    local_cluster disable
    # The cloud cluster belongs to the org every schema starts with and is shared, so every org's
    # jobs are placed on it. Registering again changes nothing and prints the same token.
    runtime_token="$(lib_hl_admin cluster add "${cloud_cluster}" default)"
    lib_hl_admin cluster share "${cloud_cluster}" on >/dev/null
    launcher_token="$(lib_hl_admin launcher add "${launcher}")"
    CLOUD_AGENT_CONFIG="${repo}/${bin}/cloud-agent.yaml" RUNTIME_TOKEN="${runtime_token}" \
      VAST_API_KEY="$(lib_env_value CLOUD_COMPUTE_VAST_API_KEY)" start cloud-agent "${bin}/cloud-agent"
    AGENT_LAUNCHER_CONFIG="${repo}/${bin}/agent-launcher.yaml" LAUNCHER_TOKEN="${launcher_token}" \
      FLY_API_TOKEN="$(lib_env_value SANDBOX_FLY_DEPLOY_TOKEN)" E2B_API_KEY="$(lib_env_value SANDBOX_E2B_API_KEY)" \
      start agent-launcher "${bin}/agent-launcher"
    must_stay_up cloud-agent
    must_stay_up agent-launcher
    start_ui
    echo "dev: API http://localhost:8081 (from outside ${public_api}), UI http://localhost:3000"
    echo "dev: cloud-agent rents for cluster ${cloud_cluster}; agent-launcher ${launcher} runs hosted agents on ${sandbox_provider}"
    status
    ;;
  api)
    start_control_plane
    status
    ;;
  ui)
    stop ui
    start_ui
    status
    ;;
  status)
    status
    ;;
  require-local)
    for name in cloud-agent agent-launcher; do
      if running "${name}"; then
        echo "dev: ${name} runs (pid $(cat "${run}/${name}.pid")), so this stack rents compute (make dev-hosted). Run make dev for a stack that cannot" >&2
        exit 1
      fi
    done
    ;;
  down)
    for name in "${processes[@]}"; do
      stop "${name}"
    done
    ;;
  *)
    echo "usage: dev.sh up|hosted|api|ui|status|require-local|down" >&2
    exit 1
    ;;
esac
