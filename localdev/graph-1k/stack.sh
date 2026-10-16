#!/usr/bin/env bash
# stack.sh up|ui|down|env
#
# A control plane of its own for timing the hypothesis graph page against a big graph: its own
# Postgres, Memgraph, GreptimeDB and object store containers (prefix hl-graph1k-, no named
# volumes), the API, the metrics service, the git server and next dev as host processes, all on
# ports the stack `make dev` runs leaves alone, with a signing secret and a sign-in key made for it
# under STATE. Nothing of the dev stack is read or touched. A person signs in through the stand-in
# identity provider the UI's browser run uses (controlplane/ui/e2e/identity-provider.mjs).
#
#   up    start everything, building the services from this tree
#   ui    restart the UI alone (UI_MODE=prod for a production build)
#   down  stop the processes and remove the containers with their data
#   env   print the addresses and the state directory as shell variables
set -euo pipefail

action="${1:?usage: stack.sh up|ui|down|env}"
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE="${STATE:-/tmp/graph-1k/stack}"
PREFIX=hl-graph1k
PG_PORT=5533
MEMGRAPH_PORT=7787
GREPTIME_PORT=4110
STORE_PORT=9100
API_PORT=8181
METRICS_PORT=8184
GIT_PORT=8185
UI_PORT=3300
IDP_PORT=54499
HOST=host.containers.internal
DATABASE_URL="postgres://hypothesisloop:hypothesisloop@localhost:${PG_PORT}/hypothesisloop?sslmode=disable"
processes=(control-service metrics-service git-server ui identity-provider cluster)

print_env() {
  cat <<EOF
STATE=${STATE}
API_URL=http://localhost:${API_PORT}
UI_URL=http://localhost:${UI_PORT}
IDP_URL=http://127.0.0.1:${IDP_PORT}
DATABASE_URL=${DATABASE_URL}
HYPOTHESISLOOP_CONFIG=${STATE}/hypothesisloop.yaml
EOF
}

running() {
  [[ -f "${STATE}/$1.pid" ]] && kill -0 "$(cat "${STATE}/$1.pid")" 2>/dev/null
}

stop() {
  if running "$1"; then
    pkill -TERM -P "$(cat "${STATE}/$1.pid")" 2>/dev/null || true
    kill "$(cat "${STATE}/$1.pid")" 2>/dev/null || true
  fi
  rm -f "${STATE}/$1.pid"
}

start() {
  local name="$1"
  shift
  stop "${name}"
  nohup "$@" >"${STATE}/${name}.log" 2>&1 &
  echo $! >"${STATE}/${name}.pid"
}

wait_for() {
  local name="$1" url="$2"
  for _ in $(seq 1 600); do
    curl -fsS -o /dev/null "${url}" 2>/dev/null && return
    if ! running "${name}"; then
      echo "stack: ${name} did not start:" >&2
      tail -20 "${STATE}/${name}.log" >&2
      exit 1
    fi
    sleep 0.2
  done
  echo "stack: ${name} did not answer at ${url} in two minutes" >&2
  exit 1
}

require_free() {
  local port
  for port in "$@"; do
    if lsof -nP -t -iTCP:"${port}" -sTCP:LISTEN >/dev/null 2>&1; then
      echo "stack: port ${port} is already served by process $(lsof -nP -t -iTCP:"${port}" -sTCP:LISTEN | head -1)" >&2
      exit 1
    fi
  done
}

containers_up() {
  podman run -d --name "${PREFIX}-postgres" -p "${PG_PORT}:5432" \
    -e POSTGRES_DB=hypothesisloop -e POSTGRES_USER=hypothesisloop -e POSTGRES_PASSWORD=hypothesisloop \
    docker.io/library/postgres:16-alpine >/dev/null
  podman run -d --name "${PREFIX}-memgraph" -p "${MEMGRAPH_PORT}:7687" \
    docker.io/memgraph/memgraph:3.13.1 --telemetry-enabled=false --storage-wal-enabled=true >/dev/null
  podman run -d --name "${PREFIX}-greptimedb" -p "${GREPTIME_PORT}:4000" \
    docker.io/greptime/greptimedb:latest standalone start --http-addr=0.0.0.0:4000 --rpc-bind-addr=0.0.0.0:4001 >/dev/null
  podman run -d --name "${PREFIX}-store" -p "${STORE_PORT}:9000" \
    -e MINIO_ROOT_USER=hypothesisloop -e MINIO_ROOT_PASSWORD=hypothesisloop \
    docker.io/pgsty/silo:RELEASE.2026-09-16T00-00-00Z server /data --address 0.0.0.0:9000 >/dev/null
  until podman exec "${PREFIX}-postgres" pg_isready -U hypothesisloop -d hypothesisloop >/dev/null 2>&1; do sleep 0.5; done
  until podman exec "${PREFIX}-memgraph" sh -c "echo 'RETURN 1;' | mgconsole" >/dev/null 2>&1; do sleep 0.5; done
  until curl -fsS -o /dev/null "http://localhost:${STORE_PORT}/minio/health/live" 2>/dev/null; do sleep 0.5; done
  until curl -fsS -o /dev/null "http://localhost:${GREPTIME_PORT}/health" 2>/dev/null; do sleep 0.5; done
  podman exec "${PREFIX}-store" sh -c 'mcli alias set hl http://localhost:9000 hypothesisloop hypothesisloop >/dev/null && mcli mb --ignore-existing hl/hypothesisloop-data >/dev/null'
}

# The committed settings with this stack's addresses, its own sign-in key and the git server.
render_settings() {
  local coordinate x y
  [[ -s "${STATE}/signing-secret" ]] || (umask 077 && openssl rand -hex 32 >"${STATE}/signing-secret")
  [[ -s "${STATE}/jwt-key.pem" ]] || (umask 077 && openssl ecparam -name prime256v1 -genkey -noout | openssl pkcs8 -topk8 -nocrypt -out "${STATE}/jwt-key.pem")
  [[ -s "${STATE}/git-password" ]] || (umask 077 && openssl rand -hex 24 >"${STATE}/git-password")
  coordinate() {
    openssl ec -in "${STATE}/jwt-key.pem" -pubout -outform DER 2>/dev/null | tail -c 64 | "$@" | base64 | tr '+/' '-_' | tr -d '=\n'
  }
  x="$(coordinate head -c 32)"
  y="$(coordinate tail -c 32)"
  sed -e "s|^  api_port: 8081$|  api_port: ${API_PORT}|" \
      -e "s|^  metric_controller_port: 8084$|  metric_controller_port: ${METRICS_PORT}|" \
      -e "s|http://REPLACE-WITH-HOST-ADDRESS:8081|http://${HOST}:${API_PORT}|g" \
      -e "s|http://REPLACE-WITH-HOST-ADDRESS:8084|http://${HOST}:${METRICS_PORT}|g" \
      -e "s|http://REPLACE-WITH-HOST-ADDRESS:9000|http://${HOST}:${STORE_PORT}|g" \
      -e "s|REPLACE-WITH-HOST-ADDRESS|${HOST}|g" \
      -e "s|REPLACE-WITH-IMAGE-TAG|graph-1k|g" \
      -e "s|REPLACE-WITH-UI-ORIGIN|http://localhost:${UI_PORT}|g" \
      -e "s|http://greptimedb:4000|http://localhost:${GREPTIME_PORT}|" \
      -e "s|bolt://memgraph:7687|bolt://localhost:${MEMGRAPH_PORT}|" \
      -e "s|http://minio:9000|http://localhost:${STORE_PORT}|" \
      -e "s|^  identity_providers: \[\]$|  identity_providers: [{issuer: \"http://localhost/hypothesisloop-dev\", audience: hypothesisloop-dev, jwt_keys: [{kid: local-dev, kty: EC, alg: ES256, crv: P-256, x: \"${x}\", y: \"${y}\"}]}]|" \
      "${repo}/controlplane/settings/hypothesisloop.yaml" >"${STATE}/hypothesisloop.yaml"
  printf '\ngit_host:\n  proxy_url: http://%s:%s/git\n  provider: plain\n  plain:\n    base_url: http://%s:%s\n    allow_http: true\n    username: hypothesisloop\n    password: ${HYPOTHESISLOOP_GIT_HOST_PLAIN_PASSWORD}\n' \
    "${HOST}" "${API_PORT}" "${HOST}" "${GIT_PORT}" >>"${STATE}/hypothesisloop.yaml"
  if grep -q 'REPLACE-WITH\|:8081\|:8084\|greptimedb:\|memgraph:\|minio:' "${STATE}/hypothesisloop.yaml"; then
    echo "stack: the settings file changed shape; these lines still name the dev stack or a placeholder:" >&2
    grep -n 'REPLACE-WITH\|:8081\|:8084\|greptimedb:\|memgraph:\|minio:' "${STATE}/hypothesisloop.yaml" >&2
    exit 1
  fi
}

# next dev, or with UI_MODE=prod a production build served by next start. Run from the UI's own
# directory: Tailwind finds the classes it generates relative to it.
start_ui() {
  local ui="${repo}/controlplane/ui" next="${repo}/controlplane/ui/node_modules/.bin/next"
  export NEXT_DIST_DIR=".next-graph-1k" NODE_OPTIONS=--no-experimental-strip-types \
    NEXT_PUBLIC_API_URL="http://localhost:${API_PORT}" NEXT_PUBLIC_IDENTITY_PROVIDER=supabase \
    NEXT_PUBLIC_IDENTITY_SUPABASE_URL="http://127.0.0.1:${IDP_PORT}" NEXT_PUBLIC_IDENTITY_SUPABASE_PUBLISHABLE_KEY=sb_publishable_graph-1k \
    NEXT_PUBLIC_SIGN_IN_PROVIDERS=github
  stop ui
  require_free "${UI_PORT}"
  if [[ "${UI_MODE:-dev}" == "prod" ]]; then
    (cd "${ui}" && "${next}" build >"${STATE}/ui-build.log" 2>&1) || { tail -30 "${STATE}/ui-build.log" >&2; exit 1; }
    (cd "${ui}" && start ui "${next}" start -p "${UI_PORT}")
  else
    (cd "${ui}" && start ui "${next}" dev -p "${UI_PORT}")
  fi
  wait_for ui "http://localhost:${UI_PORT}/login"
}

case "${action}" in
  up)
    mkdir -p "${STATE}/git" "${STATE}/bin"
    require_free "${PG_PORT}" "${MEMGRAPH_PORT}" "${GREPTIME_PORT}" "${STORE_PORT}" "${API_PORT}" "${METRICS_PORT}" "${GIT_PORT}" "${IDP_PORT}"
    render_settings
    (cd "${repo}/controlplane" && go build -o "${STATE}/bin/" ./cmd/control-service ./cmd/metrics-service ./cmd/hl-admin)
    (cd "${repo}" && go build -o "${STATE}/bin/git-server" ./localdev/local-git)
    containers_up
    export DATABASE_URL HYPOTHESISLOOP_CONFIG="${STATE}/hypothesisloop.yaml"
    HYPOTHESISLOOP_SIGNING_SECRET="$(<"${STATE}/signing-secret")"
    HYPOTHESISLOOP_GIT_HOST_PLAIN_PASSWORD="$(<"${STATE}/git-password")"
    # The model proxy's credential is never used here: no agent is hosted.
    MODEL_ANTHROPIC_OAUTH_TOKEN=unused-by-graph-1k
    export HYPOTHESISLOOP_SIGNING_SECRET HYPOTHESISLOOP_GIT_HOST_PLAIN_PASSWORD MODEL_ANTHROPIC_OAUTH_TOKEN
    GIT_SERVER_ADDRESS=":${GIT_PORT}" GIT_SERVER_ROOT="${STATE}/git" GIT_SERVER_USERNAME=hypothesisloop \
      GIT_SERVER_PASSWORD="${HYPOTHESISLOOP_GIT_HOST_PLAIN_PASSWORD}" start git-server "${STATE}/bin/git-server"
    wait_for git-server "http://localhost:${GIT_PORT}/health"
    start control-service "${STATE}/bin/control-service"
    wait_for control-service "http://localhost:${API_PORT}/health"
    start metrics-service "${STATE}/bin/metrics-service"
    wait_for metrics-service "http://localhost:${METRICS_PORT}/health"
    start identity-provider node "${repo}/localdev/graph-1k/identity-provider.mjs" "${IDP_PORT}" "${STATE}/jwt-key.pem" "http://localhost:${UI_PORT}"
    wait_for identity-provider "http://127.0.0.1:${IDP_PORT}/test/state"
    API_URL="http://localhost:${API_PORT}" IDP_URL="http://127.0.0.1:${IDP_PORT}" start cluster node "${repo}/localdev/graph-1k/cluster.mjs"
    start_ui
    print_env
    ;;
  down)
    for name in "${processes[@]}"; do stop "${name}"; done
    for c in postgres memgraph greptimedb store; do podman rm -f -v "${PREFIX}-${c}" >/dev/null 2>&1 || true; done
    ;;
  ui)
    mkdir -p "${STATE}"
    start_ui
    ;;
  env)
    print_env
    ;;
  *)
    echo "usage: stack.sh up|ui|down|env" >&2
    exit 1
    ;;
esac
