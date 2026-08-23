# hypothesisloop (Helm chart)

Installs control-service, metrics-service, Postgres, and GreptimeDB into a Kubernetes
cluster with plain `helm install`/`helm upgrade` — Helm 4, no server-side component to
install into the cluster first.

## Install

```sh
make helm-images                 # build control-service/metrics-service/cluster-agent/
                                  # node-agent images and import them into this host's k3s
make helm-prepare                # stage controlplane/settings/hypothesisloop.yaml into the
                                  # chart (single source of truth — never edit the staged
                                  # copy directly). schema.sql is not staged: it's baked into
                                  # control-service's own image and self-applied on every boot
                                  # (db.ApplySchema) — Postgres starts with no init script at
                                  # all, and metrics-service never touches the schema.
helm install hypothesisloop controlplane/infra/helm/hypothesisloop \
  --set objectStore.endpoint=http://<host-lan-ip>:9000
```

`objectStore.endpoint` must be the durable-data object store's real LAN address, same
constraint as `controlplane/settings/hypothesisloop.yaml` itself — a loopback address is
unreachable from inside the cluster.

On a single-node cluster, or once every node has its own copy of the locally-built
images, clear the node pinning:

```sh
helm install hypothesisloop controlplane/infra/helm/hypothesisloop \
  --set objectStore.endpoint=http://<host-lan-ip>:9000 \
  --set builtImageNodeSelector={} --set builtImageTolerations={}
```

## What this chart does not do

- **Does not install the cluster-agent/node-agent bundle by default** (`clusterAgentBundle.enabled: false`).
  A cluster may already have that bundle from `runtime/k8s/infra/install.sh` — installing
  both would double-register the same cluster with the control plane. Enable it only on a
  cluster with neither.
- **Does not manage the durable-data object store.** Point `objectStore.endpoint` at an
  existing S3-compatible bucket; the platform is deliberately not in the data path (see
  the `data_store` comment in `controlplane/settings/hypothesisloop.yaml`).
- **Runs Postgres/GreptimeDB as single-replica StatefulSets with no backup policy.**
  Fine for getting the platform running; durability/HA for both is a later problem, not
  one this chart tries to solve now.

## Verifying an install

```sh
kubectl -n hypothesisloop get pods
kubectl -n hypothesisloop run curltest --rm -i --restart=Never --image=curlimages/curl:latest \
  -- curl -sf http://control-service:8081/health
```

## Upgrading

`make helm-prepare && helm upgrade hypothesisloop controlplane/infra/helm/hypothesisloop ...` —
same values you installed with. `helm upgrade` on unchanged values is a no-op (verified:
no pod restarts).
