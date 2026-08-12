#!/usr/bin/env bash
# Throwaway second control-plane stack for verifying frontend changes against the new
# pagination/search backend without touching the primary running stack. Alt ports, alt pod
# name, alt volumes — up/down only, no destroy needed since data here is disposable.
set -euo pipefail

ACTION="${1:?usage: podman-dev2.sh up|down}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
POD="hypothesisloop-controlplane-dev2"
POSTGRES_VOLUME="hypothesisloop-postgres-data-dev2"

wait_for_postgres() {
  local deadline=$((SECONDS + 60))
  until podman exec hypothesisloop-postgres-dev2 pg_isready -U hypothesisloop -d hypothesisloop >/dev/null; do
    (( SECONDS < deadline )) || { echo "postgres did not become ready" >&2; exit 1; }
    sleep 1
  done
}

wait_for_http() {
  local url="$1" deadline=$((SECONDS + 60))
  until curl -fsS -o /dev/null "$url"; do
    (( SECONDS < deadline )) || { echo "service did not become ready: $url" >&2; exit 1; }
    sleep 1
  done
}

case "$ACTION" in
  up)
    podman volume create --ignore "$POSTGRES_VOLUME" >/dev/null
    podman pod create --name "$POD" \
      -p 5443:5432 -p 8091:8081 -p 8092:8082 -p 8093:8083 -p 8094:8084 >/dev/null

    podman run -d --pod "$POD" --name hypothesisloop-postgres-dev2 \
      -e POSTGRES_DB=hypothesisloop -e POSTGRES_USER=hypothesisloop -e POSTGRES_PASSWORD=hypothesisloop \
      -v "$POSTGRES_VOLUME:/var/lib/postgresql/data" \
      -v "$ROOT/controlplane/infra/postgres/init.sql:/docker-entrypoint-initdb.d/init.sql:ro" \
      -v "$ROOT/controlplane/shared/db/schema.sql:/schema/schema.sql:ro" \
      docker.io/library/postgres:16-alpine >/dev/null

    wait_for_postgres

    podman run -d --pod "$POD" --name hypothesisloop-control-service-dev2 \
      -e DATABASE_URL='postgres://hypothesisloop:hypothesisloop@localhost:5432/hypothesisloop?sslmode=disable' \
      -v "$ROOT/controlplane/settings/hypothesisloop.yaml:/settings/hypothesisloop.yaml:ro" \
      localhost/hypothesisloop-control-service:dev2 >/dev/null

    podman run -d --pod "$POD" --name hypothesisloop-metrics-service-dev2 \
      -e DATABASE_URL='postgres://hypothesisloop:hypothesisloop@localhost:5432/hypothesisloop?sslmode=disable' \
      -v "$ROOT/controlplane/settings/hypothesisloop.yaml:/settings/hypothesisloop.yaml:ro" \
      localhost/hypothesisloop-metrics-service:dev2 >/dev/null

    wait_for_http http://localhost:8092/experiments
    wait_for_http http://localhost:8093/health
    echo "dev2 stack up: quota=8091 scheduler=8092 registry=8093 metric-controller=8094 postgres=5443"
    ;;
  down)
    podman pod rm -f "$POD" || true
    ;;
  *)
    echo "usage: podman-dev2.sh up|down" >&2
    exit 1
    ;;
esac
