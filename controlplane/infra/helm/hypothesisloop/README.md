# hypothesisloop (Helm chart)

Installs the control plane into one namespace with plain `helm install`/`helm upgrade`:
control-service, metrics-service, the UI, GreptimeDB, Memgraph, and (each optional) Postgres and
the object store.

## Install

```sh
make images REGISTRY=<registry>   # builds every image and pushes it under the TAG make prints
make ui-image REGISTRY=<registry> NEXT_PUBLIC_API_URL=<the API as a browser reaches it> \
  NEXT_PUBLIC_SUPABASE_URL=<project address> NEXT_PUBLIC_SUPABASE_PUBLISHABLE_KEY=<its publishable key> \
  NEXT_PUBLIC_SIGN_IN_PROVIDERS=<the sign-in providers offered, in order, such as github>
make helm-prepare                 # stages controlplane/settings/hypothesisloop.yaml into the chart

kubectl create namespace hypothesisloop
kubectl -n hypothesisloop create secret generic hypothesisloop \
  --from-env-file=<a file of KEY=value lines, see The Secret>

helm install hypothesisloop controlplane/infra/helm/hypothesisloop --values <your values file>
```

A values file for every store in the cluster, with made-up addresses, is
`ci/in-cluster-values.yaml`; `ci/external-values.yaml` is one for a database run elsewhere, an
identity provider, a cloud-agent and an agent-launcher. Values are checked against
`values.schema.json`, so a misspelled or mistyped one stops the install.

The staged file is the only copy of platform policy; never edit it in the chart. `schema.sql` is
not staged: control-service carries it in its image and applies it on every start.

## The Secret

The chart reads one Secret, named by `secretName`, and never creates it. It must exist before the
install. No value of the chart is a secret, and nothing secret is rendered into a ConfigMap.

| Key | Used by | When |
| --- | --- | --- |
| `DATABASE_URL` | control-service, metrics-service | always |
| `HYPOTHESISLOOP_SIGNING_SECRET` | control-service, metrics-service | always. 32 characters or more, for example `openssl rand -hex 32` |
| `OBJECT_STORE_ACCESS_KEY_ID`, `OBJECT_STORE_SECRET_ACCESS_KEY` | control-service, metrics-service, and the store as its root user | always |
| `POSTGRES_PASSWORD` | Postgres | `postgres.enabled` |
| `HYPOTHESISLOOP_GITHUB_APP_CLIENT_SECRET`, `HYPOTHESISLOOP_GITHUB_APP_PRIVATE_KEY` | control-service, metrics-service | `github.appId` |
| `HYPOTHESISLOOP_GIT_PASSWORD` | control-service, metrics-service | `git.username` |
| `GOOGLE_CREDENTIALS` | control-service, as its Google application default credentials file | `environments.repository`. Its identity may mint the tokens of `environments.pusher` and `environments.puller` |
| any key in `cloudAgent.secretEnv`, `agentLauncher.secretEnv` | that process only, under the same name | when it is enabled |

The rendered `hypothesisloop.yaml` holds `${NAME}` where a secret belongs, and the services
replace it from their environment at start (`config.Load`). Pods do not notice a changed Secret
by themselves: pass a new `secretChecksum` and they restart.

The tokens cloud-agent and agent-launcher present are derived from the signing secret and are in
no Secret. Each of the two pods first runs `hl-admin` from the control-service image, with the
config and the Secret keys control-service is started with: the cloud-agent's registers
`cloudAgent.clusterName` for `cloudAgent.org`, shares it or not as `cloudAgent.shared` says, and
leaves the cluster's runtime token for the process; the launcher's leaves the token of
`agents.launcher`. Registering again changes nothing, so every start of the pod does it. Until
control-service has made the tables the init container fails and the pod waits.

## Choices

- **Database.** `postgres.enabled=true` runs a single Postgres. For a database run elsewhere set
  `postgres.enabled=false` and put its connection string in `DATABASE_URL`. control-service uses
  `LISTEN` and advisory locks, so it needs a session connection (a transaction pooler breaks it).
- **Sign-in.** `auth.issuer`, `auth.audience` and `auth.jwtKeys` name the identity provider whose
  tokens the API accepts; the keys are listed and never fetched. With none, people cannot sign in
  and the API takes API keys and derived tokens only. `auth.uiOrigin` is the one browser origin
  allowed to call the API.
- **Object store.** `objectStore.enabled=true` runs Silo and a Job that creates the bucket named
  by `data_store.bucket`. `objectStore.endpoint` is required either way and is the address every
  job and agent is handed: the in-cluster Service when all jobs run in this cluster, an outside
  address (a tunnel to the NodePort, a load balancer) when they do not. The control plane sends
  its own requests to the store's Service, so they keep working while that outside address is
  down. With `objectStore.enabled=false` name the store's address on the control plane's network
  in `objectStore.controlPlaneEndpoint`, which is then required.
- **Reaching it from outside.** The API (8081), metrics (8084), the object store (9000) and the
  UI (3000) are NodePort Services on 30081, 30084, 30900 and 30300. Set `service.type` to
  `ClusterIP` on any of them to keep it inside the cluster.
- **Who reaches the stores.** Memgraph and GreptimeDB take no credentials. Each store the chart
  runs has a NetworkPolicy that accepts its clients only: Memgraph control-service, GreptimeDB
  control-service and metrics-service, Postgres those two and the init containers of the
  cloud-agent and the agent-launcher. A policy does something only where the cluster enforces
  NetworkPolicy. k3s does out of the box; a cluster whose network plugin does not leaves the
  stores open to every pod. A pod on the host network is the node to a policy and is let through.
- **The UI.** Next.js compiles the API address, the two Supabase values and the sign-in
  providers into the image when it is built (the four `NEXT_PUBLIC_*` build arguments of
  `controlplane/build/Dockerfile.ui`, all required), so an image serves one deployment. `ui.supabaseURL` and `ui.supabasePublishableKey` repeat the two
  Supabase values as the pod's environment.
- **The API's address.** `publicAPIURL` is the one address hosted agents and every cluster's
  runtime reach the API at. It is required and must be reachable from wherever a launcher rents
  and from inside every cluster that runs jobs.
- **Hosted agents.** `agents.launcher` and `agents.image` are the `agents` section of the config
  and are required, since the control plane does not start without them.
- **Runtimes.** `runtimes.metricsURL` is where a cluster's runtime reaches metrics-service.
  Registering a cluster hands it out beside `publicAPIURL`.
- **cloud-agent and agent-launcher.** Off by default. Each is one image and one config file,
  passed whole through `config` and mounted from a ConfigMap. The file names environment
  variables and never holds a secret; list those variables in `secretEnv` and keep their values
  in the Secret.

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
