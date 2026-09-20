# hypothesisloop (Helm chart)

Installs the control plane into one namespace with plain `helm install`/`helm upgrade`:
control-service, metrics-service, the UI, GreptimeDB, Memgraph, and (each optional) Postgres and
the object store.

## Install

```sh
make images REGISTRY=<registry>   # builds every image and pushes it under the TAG make prints
make helm-prepare                 # stages controlplane/settings/hypothesisloop.yaml into the chart

kubectl create namespace hypothesisloop
kubectl -n hypothesisloop create secret generic hypothesisloop \
  --from-literal=POSTGRES_PASSWORD=<password> \
  --from-literal=DATABASE_URL='postgres://hypothesisloop:<password>@hypothesisloop-postgres:5432/hypothesisloop?sslmode=disable' \
  --from-literal=OBJECT_STORE_ACCESS_KEY_ID=<key id> \
  --from-literal=OBJECT_STORE_SECRET_ACCESS_KEY=<key>

helm install hypothesisloop controlplane/infra/helm/hypothesisloop \
  --set namespace.create=false \
  --set images.registry=<registry> --set images.tag=<TAG> \
  --set objectStore.endpoint=http://object-store.hypothesisloop.svc:9000
```

The staged file is the only copy of platform policy; never edit it in the chart. `schema.sql` is
not staged: control-service carries it in its image and applies it on every start.

## The Secret

The chart reads one Secret (`secretName`, default `hypothesisloop`) and never creates it. It must
exist before the install.

| Key | Used by | When |
| --- | --- | --- |
| `DATABASE_URL` | control-service, metrics-service | always |
| `POSTGRES_PASSWORD` | Postgres | `postgres.enabled` |
| `OBJECT_STORE_ACCESS_KEY_ID`, `OBJECT_STORE_SECRET_ACCESS_KEY` | control-service, metrics-service, and the store as its root user | always |
| any key in `config.secretEnv` | control-service, metrics-service | when `config.extra` references it |
| any key in `cloudAgent.secretEnv`, `agentLauncher.secretEnv` | that process only | when it is enabled |

The rendered `hypothesisloop.yaml` holds `${NAME}` where a secret belongs, and the services
replace it from their environment at start (`config.Load`). Pods do not notice a changed Secret
by themselves: pass a new `secretChecksum` and they restart.

## Choices

- **Database.** `postgres.enabled=true` runs a single Postgres. For a database run elsewhere set
  `postgres.enabled=false` and put its connection string in `DATABASE_URL`. control-service uses
  `LISTEN` and advisory locks, so it needs a session connection (a transaction pooler breaks it).
- **Object store.** `objectStore.enabled=true` runs Silo and a Job that creates the bucket named
  by `data_store.bucket`. `objectStore.endpoint` is required either way and is the one address
  the control plane and every job use: the in-cluster Service when all jobs run in this cluster,
  an outside address (a tunnel to the NodePort, a load balancer) when they do not.
- **Reaching it from outside.** The API (8081), metrics (8084), the object store (9000) and the
  UI (3000) are NodePort Services on 30081, 30084, 30900 and 30300. Set `service.type` to
  `ClusterIP` on any of them to keep it inside the cluster.
- **Extra config sections.** `config.extra` is appended verbatim to `hypothesisloop.yaml`.
- **cloud-agent and agent-launcher.** Off by default. Each is one image, one config file passed
  verbatim through `config`, and the Secret keys listed in its `secretEnv`.

## What this chart does not do

- It does not install the cluster agent. A cluster that runs jobs gets the runtime's own chart
  through `make cluster-agent-up` (`runtime/k8s/infra/install.sh`). Both use the `hypothesisloop`
  namespace: install this chart first, or pass `--set namespace.create=false`.
- It keeps no backups. Postgres, GreptimeDB, Memgraph and the object store are single-replica
  StatefulSets. Memgraph is one shared instance reached only by control-service, which exits when
  it cannot reach Memgraph at start and is restarted until it can.

## Verifying an install

```sh
kubectl -n hypothesisloop get pods
kubectl -n hypothesisloop run curltest --rm -i --restart=Never --image=curlimages/curl:latest \
  -- curl -sf http://control-service:8081/health
```
