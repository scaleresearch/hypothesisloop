#!/usr/bin/env bash
# Runs the control plane and the UI as host processes against the compose datastores, so a code
# change needs a restart of one process and no image build.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo}"
. localdev/lib/auth.sh

compose=localdev/controlplane/docker-compose.yml
rendered=controlplane/settings/.hypothesisloop.rendered.yaml
settings=controlplane/settings/.hypothesisloop.dev.yaml
bin=localdev/.bin

sed -e 's|http://greptimedb:4000|http://localhost:4010|' \
    -e 's|bolt://memgraph:7687|bolt://localhost:7687|' "${rendered}" > "${settings}"
for address in "http://localhost:4010" "bolt://localhost:7687"; do
  grep -q "${address}" "${settings}" || { echo "dev: the settings file changed shape, ${address} was not rendered" >&2; exit 1; }
done

podman compose -f "${compose}" stop control-service metrics-service >/dev/null 2>&1 || true
REGISTRY=unused TAG=unused podman compose -f "${compose}" up -d --no-recreate postgres memgraph greptimedb minio minio-init >/dev/null

mkdir -p "${bin}"
(cd controlplane && go build -o "${repo}/${bin}/" ./cmd/control-service ./cmd/metrics-service)

export HYPOTHESISLOOP_CONFIG="${repo}/${settings}"
export HYPOTHESISLOOP_SCHEMA_SQL="${repo}/controlplane/shared/db/schema.sql"
export DATABASE_URL="postgres://hypothesisloop:hypothesisloop@localhost:5433/hypothesisloop?sslmode=disable"

trap 'kill 0' EXIT INT TERM
"${bin}/control-service" &
api=$!
"${bin}/metrics-service" &
(cd controlplane/ui && exec npm run dev) &
until curl -fsS -o /dev/null http://localhost:8081/health 2>/dev/null; do
  kill -0 "${api}" 2>/dev/null || { echo "dev: control-service did not start" >&2; exit 1; }
  sleep 0.2
done
lib_register_local_clusters
echo "dev: API http://localhost:8081, UI http://localhost:3000"
wait
