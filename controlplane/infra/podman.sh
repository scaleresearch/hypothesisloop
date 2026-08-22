#!/usr/bin/env bash
set -euo pipefail

ACTION="${1:?usage: podman.sh up|down|destroy|start|stop|reload}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
POD="hypothesisloop-controlplane"
POSTGRES_VOLUME="hypothesisloop-postgres-data"
GREPTIME_VOLUME="hypothesisloop-greptimedb-data"
MINIO_VOLUME="hypothesisloop-minio-data"

wait_for_postgres() {
  local deadline=$((SECONDS + 60))
  until podman exec hypothesisloop-postgres pg_isready -U hypothesisloop -d hypothesisloop >/dev/null; do
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

run_services() {
  podman run -d --pod "$POD" --name hypothesisloop-control-service \
    --restart=unless-stopped \
    -e DATABASE_URL='postgres://hypothesisloop:hypothesisloop@localhost:5432/hypothesisloop?sslmode=disable' \
    -v "$ROOT/controlplane/settings/hypothesisloop.yaml:/settings/hypothesisloop.yaml:ro" \
    localhost/hypothesisloop-control-service:latest >/dev/null

  podman run -d --pod "$POD" --name hypothesisloop-metrics-service \
    --restart=unless-stopped \
    -e DATABASE_URL='postgres://hypothesisloop:hypothesisloop@localhost:5432/hypothesisloop?sslmode=disable' \
    -v "$ROOT/controlplane/settings/hypothesisloop.yaml:/settings/hypothesisloop.yaml:ro" \
    localhost/hypothesisloop-metrics-service:latest >/dev/null

  wait_for_http http://localhost:8081/health
  wait_for_http http://localhost:8084/health
}

case "$ACTION" in
  up)
    # If the pod already exists (e.g. after a reboot, since `--restart=unless-stopped` doesn't
    # survive the host going down), it and its containers are still there — start them instead of
    # failing on `pod create`. A pod that exists but won't start is a genuinely broken one and
    # `podman pod start` still errors out, so this stays fail-fast.
    if podman pod exists "$POD"; then
      for c in hypothesisloop-postgres hypothesisloop-greptimedb hypothesisloop-minio hypothesisloop-control-service hypothesisloop-metrics-service; do
        podman container exists "$c" || {
          echo "pod \"$POD\" exists but container $c doesn't — a half-created pod. Run 'podman.sh down && podman.sh up' to rebuild it." >&2
          exit 1
        }
      done
      podman pod start "$POD" >/dev/null
      pod_existed=1
    else
      pod_existed=0
      # --ignore: `up` must be re-runnable against surviving volumes (down keeps them unless asked
      # to purge), otherwise the second `up` on any machine that already has data aborts here.
      podman volume create --ignore "$POSTGRES_VOLUME" >/dev/null
      podman volume create --ignore "$GREPTIME_VOLUME" >/dev/null
      podman volume create --ignore "$MINIO_VOLUME" >/dev/null
      podman pod create --name "$POD" \
        --add-host greptimedb:127.0.0.1 --add-host minio:127.0.0.1 \
        -p 5433:5432 -p 8081:8081 -p 8084:8084 \
        -p 4010:4000 -p 4001:4001 -p 9000:9000 >/dev/null

      podman run -d --pod "$POD" --name hypothesisloop-postgres \
        --restart=unless-stopped \
        -e POSTGRES_DB=hypothesisloop -e POSTGRES_USER=hypothesisloop -e POSTGRES_PASSWORD=hypothesisloop \
        -v "$POSTGRES_VOLUME:/var/lib/postgresql/data" \
        -v "$ROOT/controlplane/infra/postgres/init.sql:/docker-entrypoint-initdb.d/init.sql:ro" \
        -v "$ROOT/controlplane/shared/db/schema.sql:/schema/schema.sql:ro" \
        docker.io/library/postgres:16-alpine >/dev/null

      podman run -d --pod "$POD" --name hypothesisloop-greptimedb \
        --restart=unless-stopped \
        -v "$GREPTIME_VOLUME:/tmp/greptimedb-data" \
        docker.io/greptime/greptimedb:latest standalone start \
        --http-addr=0.0.0.0:4000 --rpc-bind-addr=0.0.0.0:4001 >/dev/null

      # Durable data between jobs (hypothesisloop.yaml data_store). Object expiry is a lifecycle
      # rule on the bucket, never a sweeper in the control plane.
      podman run -d --pod "$POD" --name hypothesisloop-minio \
        --restart=unless-stopped \
        -e MINIO_ROOT_USER=hypothesisloop -e MINIO_ROOT_PASSWORD=hypothesisloop \
        -v "$MINIO_VOLUME:/data" \
        docker.io/minio/minio:latest server /data --address 0.0.0.0:9000 >/dev/null
      wait_for_http http://localhost:9000/minio/health/live
      podman run --rm --pod "$POD" --entrypoint sh docker.io/minio/mc:latest -c \
        "mc alias set hl http://localhost:9000 hypothesisloop hypothesisloop >/dev/null && mc mb --ignore-existing hl/hypothesisloop-data >/dev/null" >/dev/null
    fi

    wait_for_postgres
    if (( pod_existed )); then
      wait_for_http http://localhost:8081/health
      wait_for_http http://localhost:8084/health
    else
      run_services
    fi
    ;;
  down)
    # Removes the pod and every container, and KEEPS the volumes: a finished run's experiments,
    # metrics and hypotheses survive, so `down` followed by `up` is always safe. Deleting the data
    # is `destroy`, which you have to ask for by name — this used to be `down`'s job, and one
    # routine `down` to pick up a rebuilt image silently erased a completed 7-hour run.
    if podman pod exists "$POD"; then
      podman pod rm -f "$POD"
    else
      rc=$?
      (( rc == 1 )) || exit "$rc"
    fi
    ;;
  destroy)
    # down + delete the postgres and GreptimeDB volumes. Irreversible: every platform experiment,
    # experiment record, metric sample and hypothesis in this control plane goes with them.
    if [[ "${2:-}" != "--yes" ]]; then
      cat >&2 <<EOF
podman.sh destroy: this DELETES ALL control-plane data (volumes $POSTGRES_VOLUME,
$GREPTIME_VOLUME, $MINIO_VOLUME) — every platform experiment, experiment record, metric,
hypothesis and stored checkpoint.
There is no backup and no undo.

If you only want to restart the services (including onto a rebuilt image), use:
    podman.sh reload      # rebuild-aware restart, keeps data
    podman.sh down && podman.sh up

Re-run with --yes to proceed: podman.sh destroy --yes
EOF
      exit 1
    fi
    if podman pod exists "$POD"; then
      podman pod rm -f "$POD"
    else
      rc=$?
      (( rc == 1 )) || exit "$rc"
    fi
    for volume in "$POSTGRES_VOLUME" "$GREPTIME_VOLUME" "$MINIO_VOLUME"; do
      if podman volume exists "$volume"; then
        podman volume rm "$volume"
      else
        rc=$?
        (( rc == 1 )) || exit "$rc"
      fi
    done
    ;;
  start)
    podman pod start "$POD"
    wait_for_http http://localhost:8081/health
    wait_for_http http://localhost:8084/health
    ;;
  stop)
    podman pod stop "$POD"
    ;;
  reload)
    podman rm -f hypothesisloop-control-service hypothesisloop-metrics-service
    run_services
    ;;
  *)
    echo "unknown action: $ACTION" >&2
    exit 2
    ;;
esac
