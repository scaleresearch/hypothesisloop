#!/usr/bin/env bash
# dev.sh up|hosted|api|ui|status|down
#
# Runs the control plane and the UI as host processes against the compose datastores, so a code
# change needs a restart of one process and no image build. Every process is started detached,
# with its pid and its log in localdev/.run, so one can be restarted while the rest keep running.
#
#   up       render the settings for the local cluster and (re)start the API and metrics-service;
#            start the UI unless it runs
#   hosted   the same with what rents compute elsewhere: agent-launcher (Fly.io) and cloud-agent
#            (vast.ai) as two more processes, and settings that name the running tunnels (make
#            tunnels) and the images `make -C infra publish` pushed. Its inputs are keys of .env,
#            and every missing one is named before anything is touched
#   api      rebuild and restart the API and metrics-service with the settings as they were last
#            rendered. Everything else keeps running
#   ui       (re)start the UI, which otherwise reloads by itself
#   status   which processes run
#   down     stop them all. The tunnels are not part of this (make tunnels-down)
set -euo pipefail

action="${1:?usage: dev.sh up|hosted|api|ui|status|down}"
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo}"
. localdev/lib/auth.sh

compose=localdev/controlplane/docker-compose.yml
rendered=controlplane/settings/.hypothesisloop.rendered.yaml
settings=controlplane/settings/.hypothesisloop.dev.yaml
bin=localdev/.bin
run=localdev/.run
processes=(control-service metrics-service ui cloud-agent agent-launcher)
# The name every org sees the platform's rented compute under.
cloud_cluster=hypothesisloop-vast

mkdir -p "${bin}" "${run}"
export HYPOTHESISLOOP_CONFIG="${repo}/${settings}"
export HYPOTHESISLOOP_SCHEMA_SQL="${repo}/controlplane/shared/db/schema.sql"
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
    for _ in $(seq 1 25); do
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
  local hosted=() values=("http://localhost:4010" "bolt://localhost:7687") value host
  if [[ "${1:-}" == "hosted" ]]; then
    host="$(cat localdev/.host-address)"
    hosted=(-e "s|http://${host}:8081|${public_api}|"
            -e "s|http://${host}:8084|${public_metrics}|"
            -e "s|http://${host}:9000|${public_store}|"
            -e "s|^  image: registry.example.invalid/hypothesisloop-experimentator-base:latest$|  image: ${registry}/hypothesisloop-experimentator-base:${tag}|")
    values+=("  api_url: ${public_api}" "  metrics_url: ${public_metrics}" "  endpoint: ${public_store}" "  image: ${registry}/hypothesisloop-experimentator-base:${tag}")
  fi
  sed -e 's|http://greptimedb:4000|http://localhost:4010|' \
      -e 's|bolt://memgraph:7687|bolt://localhost:7687|' ${hosted[@]+"${hosted[@]}"} "${rendered}" > "${settings}"
  for value in "${values[@]}"; do
    grep -qF -- "${value}" "${settings}" || { echo "dev: the settings file changed shape, '${value}' was not rendered" >&2; exit 1; }
  done
}

# Each runtime process reads one config file: the example the core ships, with this deployment's
# addresses, images and accounts in it. A secret is named there and handed over in the
# environment of the one process that needs it.
render_runtime_configs() {
  local value
  sed -e "s|^cluster_name: .*$|cluster_name: ${cloud_cluster}|" \
      -e "s|^api_url: .*$|api_url: ${public_api}|" \
      -e "s|^job_image: .*$|job_image: ${registry}/hypothesisloop-job:${tag}|" \
      -e "s|^  account_id: .*$|  account_id: \"$(lib_env_value VAST_ACCOUNT_ID)\"|" \
      runtime/cloud/settings/cloud-agent.vast.yaml > "${bin}/cloud-agent.yaml"
  sed -e "s|^api_url: .*$|api_url: http://localhost:8081|" \
      -e "s|^  name: example$|  name: fly|" \
      -e "s|^  settings: {}$|  settings: {api_token_env: FLY_API_TOKEN, app: $(lib_env_value FLY_APP), region: ams, cpu_kind: shared}|" \
      runtime/cloud/settings/agent-launcher.yaml > "${bin}/agent-launcher.yaml"
  for value in "cluster_name: ${cloud_cluster}" "api_url: ${public_api}" "job_image: ${registry}/hypothesisloop-job:${tag}" "account_id: \"$(lib_env_value VAST_ACCOUNT_ID)\""; do
    grep -qF -- "${value}" "${bin}/cloud-agent.yaml" || { echo "dev: runtime/cloud/settings/cloud-agent.vast.yaml changed shape, '${value}' was not rendered" >&2; exit 1; }
  done
  for value in "api_url: http://localhost:8081" "  name: fly" "app: $(lib_env_value FLY_APP),"; do
    grep -qF -- "${value}" "${bin}/agent-launcher.yaml" || { echo "dev: runtime/cloud/settings/agent-launcher.yaml changed shape, '${value}' was not rendered" >&2; exit 1; }
  done
  launcher="$(sed -n 's/^launcher_name: *//p' "${bin}/agent-launcher.yaml")"
}

hosted_inputs() {
  local missing=() key tunnels
  for key in IMAGE_TAG GCP_PROJECT REGISTRY_LOCATION REGISTRY_REPOSITORY FLY_APP FLY_DEPLOY_TOKEN VAST_API_KEY VAST_ACCOUNT_ID CLAUDE_CODE_OAUTH_TOKEN; do
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
  # The registry's address as infra/pulumi composes it from the same three keys.
  registry="$(lib_env_value REGISTRY_LOCATION)-docker.pkg.dev/$(lib_env_value GCP_PROJECT)/$(lib_env_value REGISTRY_REPOSITORY)"
  tag="$(lib_env_value IMAGE_TAG)"
}

start_control_plane() {
  if [[ ! -f "${settings}" ]]; then
    echo "dev: no settings rendered yet; run make dev or make dev-hosted first" >&2
    exit 1
  fi
  # Built before anything is stopped: a tree that does not compile leaves what runs running.
  (cd controlplane && go build -o "${repo}/${bin}/" ./cmd/control-service ./cmd/metrics-service)
  stop control-service
  stop metrics-service
  require_port 8081
  # Checked before anything is stopped too: settings rendered before this tree made a key
  # required do not load, and the API would be stopped with nothing to start in its place.
  if ! lib_hl_admin token staff >/dev/null; then
    echo "dev: ${settings} does not load with this tree; render it again with make dev or make dev-hosted" >&2
    exit 1
  fi
  require_port 8084
  start control-service "${bin}/control-service"
  start metrics-service "${bin}/metrics-service"
  wait_for control-service http://localhost:8081/health
  wait_for metrics-service http://localhost:8084/health
  lib_register_local_clusters
}

start_datastores() {
  podman compose -f "${compose}" stop control-service metrics-service >/dev/null 2>&1 || true
  REGISTRY=unused TAG=unused podman compose -f "${compose}" up -d --no-recreate postgres memgraph greptimedb minio minio-init >/dev/null
}

start_ui() {
  if running ui; then
    return
  fi
  if [[ -n "$(lsof -nP -t -iTCP:3000 -sTCP:LISTEN 2>/dev/null | head -1 || true)" ]]; then
    echo "dev: something already serves port 3000 and is left as it is"
    return
  fi
  start ui npm --prefix controlplane/ui run dev
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
    render_settings
    start_datastores
    # Local settings give hosted agents no address they could reach, so nothing rents for them.
    stop cloud-agent
    stop agent-launcher
    start_control_plane
    start_ui
    echo "dev: API http://localhost:8081, UI http://localhost:3000"
    status
    ;;
  hosted)
    hosted_inputs
    render_settings hosted
    render_runtime_configs
    go build -o "${repo}/${bin}/" ./runtime/cloud/cmd/cloud-agent ./runtime/cloud/cmd/agent-launcher
    start_datastores
    start_control_plane
    # The cloud cluster belongs to the org every schema starts with and is shared, so every org's
    # jobs are placed on it. Registering again changes nothing and prints the same token.
    runtime_token="$(lib_hl_admin cluster add "${cloud_cluster}" default)"
    lib_hl_admin cluster share "${cloud_cluster}" on >/dev/null
    launcher_token="$(lib_hl_admin launcher add "${launcher}")"
    CLOUD_AGENT_CONFIG="${repo}/${bin}/cloud-agent.yaml" RUNTIME_TOKEN="${runtime_token}" \
      VAST_API_KEY="$(lib_env_value VAST_API_KEY)" start cloud-agent "${bin}/cloud-agent"
    AGENT_LAUNCHER_CONFIG="${repo}/${bin}/agent-launcher.yaml" LAUNCHER_TOKEN="${launcher_token}" \
      FLY_API_TOKEN="$(lib_env_value FLY_DEPLOY_TOKEN)" CLAUDE_CODE_OAUTH_TOKEN="$(lib_env_value CLAUDE_CODE_OAUTH_TOKEN)" \
      start agent-launcher "${bin}/agent-launcher"
    must_stay_up cloud-agent
    must_stay_up agent-launcher
    start_ui
    echo "dev: API http://localhost:8081 (from outside ${public_api}), UI http://localhost:3000"
    echo "dev: cloud-agent rents for cluster ${cloud_cluster}; agent-launcher ${launcher} rents in Fly.io app $(lib_env_value FLY_APP)"
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
  down)
    for name in "${processes[@]}"; do
      stop "${name}"
    done
    ;;
  *)
    echo "usage: dev.sh up|hosted|api|ui|status|down" >&2
    exit 1
    ;;
esac
