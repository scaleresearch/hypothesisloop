# Setup

How to run the control plane, attach compute, sign people in and connect a git host.

- [Run it locally](#run-it-locally)
- [Attach compute](#attach-compute)
- [Clusters belong to an org](#clusters-belong-to-an-org)
- [Configuration](#configuration)
- [Sign-in](#sign-in)
- [GitHub](#github)
- [Database](#database)
- [Publishing images](#publishing-images)

## Run it locally

**macOS or Linux, with a local k3s cluster**

```bash
brew install podman kubectl helm go
podman machine init --rootful --cpus 4 --memory 4096 --disk-size 40
podman machine start
cp .env.example .env   # fill in the keys make dev names when it stops
make dev               # the control plane and UI as host processes
make k3s-up            # a local k3s cluster with the cluster agent (about 3 minutes the first time)
```

`make controlplane-up` runs the control plane as containers instead of `make dev`. On macOS:

- The podman machine must be rootful. The dev nodes reach the k3s server over the VM's own address,
  which rootless node containers cannot reach.
- Nodes, job pods and containers reach the host (registry, object store, git server) as
  `host.containers.internal`, so the stack keeps working when the Mac changes networks. A Linux
  host uses its LAN address.
- `make full-start` rebuilds the dev node containers, since a stopped k3s agent cannot resume once
  its address changes.

**Windows** is untested. The control plane is Docker Compose plus Go, and the make targets need
bash, make and podman, so WSL 2 is the likely route.

## Attach compute

A cluster is a Kubernetes cluster or one bare-metal node. Both call out to the control plane, and
nothing connects in. [install-cluster.md](install-cluster.md) is the full guide.

1. Register it: **Compute Resources > Add cluster** in the UI, or `hl cluster add my-cluster`
   (`--bare-metal` for one node).
2. Run the command it shows. It asks for the runtime token shown beside it. Add
   `--dry-run=server` (Kubernetes) or `--dry-run` (bare metal) at the end to preview.
3. The cluster shows Connected within a minute (`hl cluster list`, `hl capacity`).

A Kubernetes cluster gets the Helm release `hypothesisloop-cluster-agent` in the namespace
`hypothesisloop`, runs jobs in `hypothesisloop-jobs`, and needs Kubernetes 1.34 and helm 3.17 or
newer. A bare-metal node gets `/usr/local/bin/bare-agent`, `/etc/hypothesisloop/bare-agent.env` and
the unit `hypothesisloop-agent.service`, and needs docker or podman.

The command carries the control plane's settings: `services.public_api_url` and
`runtimes.metrics_url` for the agents, and `runtimes.registry` and `runtimes.image_tag` for their
images. Each of them, and `object_store.endpoint`, must be reachable from inside the cluster, by IP
for a control plane on a tailnet. A stack started with `make dev` names this host as local
containers see it. For a cluster on another machine, set `LOCAL_REACHABLE_ADDRESS` in `.env` and
run `make dev` again ([how](install-cluster.md#a-cluster-on-another-machine-attached-to-a-local-stack)).

From a checkout:

```bash
RUNTIME_TOKEN="$(hl cluster token my-cluster)" make cluster-agent-up CLUSTER=my-cluster \
  API_URL=... METRICS_URL=... TAG=...     # HELM_FLAGS=--dry-run=server previews
make cluster-agent-check                   # the chart's test, from a pod of the cluster
make cluster-agent-down
```

A bare-metal agent builds with `go build -o bare-agent ./runtime/bare-metal/cmd/bare-agent` (Go and
gcc) and runs in the foreground with `CLUSTER_NAME`, `API_URL` and `RUNTIME_TOKEN` set.

## Clusters belong to an org

`hl cluster add` (`POST /orgs/{id}/clusters`) registers a cluster by name and returns its runtime
token. The token derives from the name, so registering a name again returns the same token and
`hl cluster token` prints it. A token for an unregistered name is refused. An operator can also
run `make -s runtime-token CLUSTER=my-cluster ORG=<org id>`.

| Cluster | Runs |
|---|---|
| registered by an org | that org's jobs |
| of the platform's own org (`default` locally: the local cluster and the rented ones) | every org's jobs, ranked below an org's own |

`hl-admin org limits ORG RATE PROJECT_RATE` caps the accelerator hours per hour of the platform's
clusters that an org's placed jobs, and each of its projects', may reserve. A new org starts with
`quota.default_org_shared_acch_per_hour` (512) and `quota.default_project_shared_acch_per_hour`
(256), in H100s at once. A GPU counts its `acch_rate` and a CPU core `quota.shared_acch_per_cpu_core`
(0.02, about its price as a share of an H100), so a project may run 256 H100s or 12800 cores. The
budget of a job on cores still counts a core-hour as 1.

| Command | Effect |
|---|---|
| `hl cluster order first second` | sets the order an org's jobs try its clusters. A job goes to the first that can take it now. |
| `hl cluster disable` / `enable` | stops or resumes new jobs on a cluster. Running jobs finish. |
| `hl cluster settings` | overrides the scale-up timeout, speculative cap, stuck-pending timeout and status silence ceiling for one cluster |
| `hl cluster forget-identity` | lets a cluster with rebuilt machines bind to the identity it now reports |
| `hl cluster remove` | requeues the cluster's jobs and refuses its token from then on |

A cluster that failed a job is skipped for that job for `scheduler.tried_cluster_ttl_seconds`. A
FAILED job's `eviction_reason` holds how its process ended and its last lines of output.

Staff can share an experiment with every account as a read-only showcase
(`hl-admin platform-experiment share PE`). Share only what anyone may read.

The signing secret `AUTH_SIGNING_SECRET` belongs to the deployment. `control-service` and
`metrics-service` do not start without it, and rotating it ends every token. Local development
creates one at the first render (`localdev/.run/signing-secret`).

## Configuration

The control plane reads one settings file, `controlplane/settings/hypothesisloop.yaml`, which
explains every key. Rented compute and hosted agents add `cloud-compute.yaml` and `sandbox.yaml`
beside it. Each runtime reads one config of its own: the cluster-agent chart's values, or
`/etc/hypothesisloop/bare-agent.env`. Local development renders the settings from `.env`
(`.env.example` names every key) with `make render-settings`. A deployment renders them from its
chart values (`deploy/charts/hypothesisloop`). Every image reference, public URL and registry comes
from configuration.

Each external service fills a role, and every role names its provider the same way:

- `.env` has `<ROLE>_PROVIDER`, such as `CONTAINER_REGISTRY_PROVIDER='gcp'`, and keys
  `<ROLE>_<PROVIDER>_<FIELD>` that only that provider reads.
- A settings section has `provider: <name>` and a subsection of that name with its keys
  (`sandbox.provider: fly` with `sandbox.fly`).
- A provider's secret is the environment key `<SECTION>_<PROVIDER>_<FIELD>`, the same in `.env`,
  the program and the chart's Secret. Rented compute takes `CLOUD_COMPUTE_<CLUSTER_NAME>_API_KEY`.
- In Go each role is a small interface with a sub-package per provider. An unknown name stops the
  process at start.

| Role | `.env` | Settings | Providers |
|---|---|---|---|
| container registry | `CONTAINER_REGISTRY_PROVIDER` | `container_registry.provider` | `gcp` |
| object store | (local MinIO) | `object_store.provider` | `minio` (MinIO, Silo, AIStor) |
| sign-in | `IDENTITY_PROVIDER` | `auth.identity_providers` | `supabase`, any OIDC issuer |
| git host | `GIT_HOST_PROVIDER` | `git_host.provider` | `github`, `plain` |
| agent sandboxes | `SANDBOX_PROVIDER` | `sandbox.provider` | `fly`, `e2b` |
| agent models | `MODEL_ANTHROPIC_OAUTH_TOKEN`, `MODEL_OPENROUTER_API_KEY`, `MODEL_OPENAI_AUTH_JSON` | `models.<provider>` | `anthropic`, `openrouter`, `openai` |
| rented compute | `CLOUD_COMPUTE_PROVIDERS` (a list, both by default) | `cloud_compute[].provider` | `vast`, `runpod` |
| the platform's own clusters | `OWNED_COMPUTE_CLUSTERS` | registered clusters | `k8s`, `bare-metal` |

Providers in `.env` are the platform's own, and adding one is an operator's decision. A customer
brings compute by registering a cluster or a machine (`hl cluster add`).

**Rented compute** (`cloud-compute.yaml`). Each entry is one provider account, scheduled as one
cluster. It rents one machine per node of a job and destroys it when the job ends. `vast` runs
single-node jobs. `runpod` runs single-node jobs and multi-node jobs on GPU pods in one data
center over Global Networking. Each entry lists the shapes (machine sizes) the account may rent,
CPU-only shapes included, each naming its GPU by catalog string alone.

Every GPU model is defined once, in `accelerator_types` of `hypothesisloop.yaml`: its catalog
string, a `label` the capacity answer and the UI show, its `acch_rate`, and under `providers` its
name on each provider (`gpu_name` and `min_gpu_ram_mb` on vast.ai, `gpu_id` on RunPod) with an
optional `max_price_per_gpu_hour` that replaces the account's `max_price_per_hour` for it. A shape
of a type with no entry for its provider is refused at load. The example rents the RTX 4090, RTX
5090, RTX A6000, L40S, A100 PCIe and SXM 80GB, H100 SXM, PCIe and NVL, H200 and B200 on both, and the
AMD Instinct MI300X on RunPod alone. A job on the MI300X runs ROCm, so its image needs a ROCm build
of its framework, such as PyTorch from the ROCm wheel index.

The example ships two entries, one on vast.ai and one on RunPod, and the control plane runs every
entry it is given at once, each as its own cluster with its own account key. To use both, list
both entries in the settings (the chart's `cloudCompute`), each with its `account_id`, and put
`CLOUD_COMPUTE_VAST_API_KEY` and `CLOUD_COMPUTE_RUNPOD_API_KEY` (after each `cluster_name`) in the
Secret. Listing an entry is the consent to rent on it: control-service registers each one for the
platform's org as it starts. Registering again changes nothing, so a cluster keeps its settings and
every org's choice to turn it off. A name another org already registered stops control-service at
start.

A cluster of the platform's org serves every org and is on for every org until that org turns it
off, so a new org starts with both. Both clusters share one place in an org's order, after the
org's own, and a job goes to the one with the most room for it. The Compute page names them
Vast.ai and RunPod and shows each one's on-demand capacity. A deployment on one provider lists
that entry alone.

## Sign-in

People sign in through a service you name with `IDENTITY_PROVIDER`. The UI knows `supabase`, a
[Supabase](https://supabase.com) project, and sends the session's access token with every API
request. The UI server reads these values from its environment at start and refuses to start
without one (`controlplane/ui/.env.example`). All are public. Local development writes them from
the root `.env`.

| Variable | Value |
|---|---|
| `UI_API_URL` | the API as the browser reaches it, e.g. `http://localhost:8081` |
| `IDENTITY_PROVIDER` | `supabase` |
| `IDENTITY_SUPABASE_URL` | `https://<project-ref>.supabase.co` |
| `IDENTITY_SUPABASE_PUBLISHABLE_KEY` | `sb_publishable_...` |
| `IDENTITY_SIGN_IN_PROVIDERS` | providers the login page offers, in order: `github`, `google`, `apple` |

To run the UI alone, export these and run `npm install && npm run dev` in `controlplane/ui`
(http://localhost:3000).

In the Supabase project (Authentication):

- **Providers**: enable each listed provider. GitHub takes the client id and secret of a GitHub
  OAuth App with the callback URL `https://<project-ref>.supabase.co/auth/v1/callback`.
- **URL configuration**: the site URL is the UI's origin, and the redirect allow-list holds
  `<ui origin>/auth/callback`.
- **JWT signing keys**: an asymmetric key (ES256 or RS256). The control plane checks public keys
  and needs no shared secret.

The control plane accepts that project's tokens through the `auth` settings:

```yaml
auth:
  identity_providers:
    - issuer: https://<project-ref>.supabase.co/auth/v1
      audience: authenticated
      jwt_keys: [...]    # the keys of https://<project-ref>.supabase.co/auth/v1/.well-known/jwks.json
  ui_origin: http://localhost:3000
```

A token is checked against the entry whose issuer it names. `make render-settings`
(`localdev/lib/auth.sh`) always adds a stand-in provider with a key made on this machine, which
the e2e suite and the UI's browser tests sign in with. With `IDENTITY_PROVIDER=supabase` and
`IDENTITY_SUPABASE_PROJECT_REF` in `.env` it also lists your Supabase project.

## GitHub

Every experiment keeps its code in git. `git_host.provider` names the host, and local development
reads `GIT_HOST_PROVIDER` from `.env`.

- **`github`**: the platform's GitHub App. An org owner presses Connect GitHub (Settings, GitHub,
  or when planning an experiment) and authorizes the App on GitHub. Every installation of the App
  they can reach that the org has not connected is connected; when there is none, GitHub's install
  page opens and they install it on their account or an organization. Disconnecting an account
  removes the App from it on GitHub, unless another org still uses it. An experiment then names a
  repository the installation covers, or the platform creates a private one from the experiment
  template (`git_host.experiment_template_repository`).
  `hl-admin github connect ORG INSTALLATION_ID` connects one for an operator.
- **`plain`**: any git server that serves repositories under one root. The platform creates a
  repository by pushing the template's files. `make dev` with `GIT_HOST_PROVIDER=plain` runs one.

Agents, coordinators and jobs never hold a git host credential. They clone, fetch and push through
the control plane's git proxy, `/git/<platform experiment id>.git` on the API (the experiment's
`git_url`), with their platform token as a Bearer header. The proxy reads every ref update before
the host sees any, and accepts a push only to the pusher's own branch:

| Pusher | May push |
|---|---|
| executor, reviewer | `hl/<id>/<agent name>/...` |
| baseline agent | `hl/<id>/baseline` and `hl/<id>/baseline-<name>` |
| researcher, planner, coordinator, jobs | nothing |

No one pushes a tag or moves or deletes the default branch or another agent's branch. The proxy
reaches the host with a credential it mints for that request.

The App's permissions are stated in `controlplane/clients/githost/github/app-manifest.json`:

| Permission | Level | Used for |
|---|---|---|
| Administration | write | creating a repository |
| Contents | write | clone, fetch and push of agents' branches, reading `.hypothesisloop/`, the first commit |
| Metadata | read | listing repositories, reading the default branch |

Without Workflows (write), GitHub refuses a push that changes `.github/workflows`, so agents cannot
change what CI runs. An installation missing a permission is refused with
`git_host_permission_missing`, naming it.

Connecting is bound to the person who pressed the button. `POST /orgs/{id}/github-install-states`
issues a single-use `state` for the org, the person and the page they left from, valid 15 minutes,
and sends the person to the App's authorization page. GitHub returns the state to the callback URL
with a `code`, and `POST /github/installations` connects every installation GitHub lists for that
GitHub user (`GET /user/installations`) that the org has not connected. With none, it issues a new
state and the tab goes on to GitHub's install page, which returns with a `code` and the
`installation_id`. Either way an installation is connected only when:

- the state is unused, unexpired and the caller's own,
- the `code` shows a GitHub user who reaches the installation,
- the installation exists, is not suspended and granted every permission.

Starting at the authorization page matters when the App is already installed on an account, for
example by another org or before a disconnect: GitHub's install page then shows that installation
and never returns to the platform.

Disconnecting an installation that no other org connected also removes the App from the GitHub
account (`DELETE /app/installations/{id}`, as the App). One GitHub removed already counts as
removed. When GitHub fails the removal the installation stays connected and the owner is told to
try again, so the platform never forgets an installation that is still on GitHub.

An org's first installation becomes its default for new repositories. Disconnecting the default makes
the oldest remaining installation the default, so an org that has any installation has a default.

To make the App, fill the `REPLACE-WITH-` values of the manifest and post it as
[GitHub's manifest flow](https://docs.github.com/apps/sharing-github-apps/registering-a-github-app-from-a-manifest)
describes. GitHub's `POST /app-manifests/{code}/conversions` returns the App's id, slug, client id,
client secret and private key. An App made by hand takes the same permissions and these settings:

- Setup URL and callback URL `<ui origin>/settings/repository`, with "Redirect on update" checked.
- "Request user authorization (OAuth) during installation" on, and webhooks off.
- Installable on any account.

Put the id, slug and client id in `git_host.github`, and the client secret and private key in
`GIT_HOST_GITHUB_CLIENT_SECRET` and `GIT_HOST_GITHUB_APP_PRIVATE_KEY`.

## Database

The Postgres schema is one file, `controlplane/storage/db/schema.sql`, embedded in
`control-service`. At start it builds the file on a database with no table, and refuses any other
database whose shape differs from the file's in any column, constraint, index, sequence, view,
type, trigger or function (`db.EnsureSchema`). A schema change is an edit of `schema.sql`.

Built-in agent types and the platform's rules are seeded at every start. A prompt, settings or
rules that differ from the current version become the next version, which every live agent is told
of. This happens only while the binary's `agenttypes.Release` is at least the one that wrote the
current version, so an older replica of a rolling deploy writes nothing.

## Publishing images

`make dev` runs the agent and job images under `IMAGE_TAG` from the registry `.env` names.
`make publish REGISTRY=... TAG=...` builds and pushes every product image and both charts.

To build on a Linux x86 machine with good upload, commit and run:

```bash
make images-remote BUILDER=<ssh host>
```

It sends HEAD's source to `~/hl-build/<you>` on that host, runs `make publish` there for
linux/amd64, and pushes under HEAD's short SHA. The registry token is minted here with `gcloud` as
`GCP_ACCOUNT`, reaches the host on ssh's stdin and is logged out after. `IMPORT=true` also imports
the images into that host's k3s, and `SSH_OPTIONS` passes options to ssh. Set the `IMAGE_TAG` it
prints in `.env`, then run `make dev`.
