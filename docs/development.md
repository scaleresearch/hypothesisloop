# Development

Build, test and run the platform from a checkout.

```bash
go build ./...
go test ./... -timeout 60s
golangci-lint run ./...
```

## Make targets

| Target | What it does |
|---|---|
| `make dev` | Runs the control plane and UI as host processes against the compose datastores, with hosted agents (on `SANDBOX_PROVIDER`) and rented compute (on each provider of `CLOUD_COMPUTE_PROVIDERS`, vast.ai and RunPod unless it names fewer, each the cluster `hypothesisloop-<provider>` of org `default`) run by control-service through public tunnels and the images under `IMAGE_TAG`. Starting it rents nothing. Hosted agents and jobs placed on the rented clusters do. |
| `make dev-api` | Rebuilds and restarts the API and metrics-service after a Go change |
| `make dev-status`, `make dev-down` | Shows or stops the `make dev` processes |
| `make tunnels`, `make tunnels-down` | Starts or stops the public tunnels to this machine's API, metrics-service, object store and git server |
| `make staff-token` | Prints a staff token (`make -s staff-token`) |
| `make token SCOPE=...` | Prints the token of a scope: `agent:<pe>/<agent>`, `coordinator:<pe>` or `job:<id>` |
| `make runtime-token CLUSTER=x` | Registers a cluster for `ORG` (default `default`) and prints its runtime token |
| `make controlplane-up` | Builds images and starts the control plane in containers |
| `make controlplane-down` | Stops the control plane and keeps its data |
| `make controlplane-destroy CONFIRM=--yes` | Stops the control plane and deletes its data |
| `make k3s-up`, `make k3s-down` | Starts a local k3s cluster with the cluster agent, or destroys it |
| `make full-stop`, `make full-start` | Pauses and resumes the podman machine, k3s and the control plane, keeping state |
| `make reload` | Rebuilds every image, pushes it to the dev registry and reinstalls the local cluster's runtime |
| `make cluster-agent-up`, `-check`, `-down` | Installs, tests or removes the cluster agent on the cluster `kubectl` points at (`CLUSTER`, `API_URL`, `METRICS_URL`, `TAG`; `RUNTIME_TOKEN` in the environment) |
| `make publish REGISTRY=... TAG=...` | Builds and pushes every product image and both charts |
| `make images-remote BUILDER=host` | Runs `make publish` on another machine ([setup](setup.md#publishing-images)) |
| `make tt-up`, `make nvidia-up` | Bootstraps k3s on a Tenstorrent or NVIDIA host (`-down`, `-status`; `tt-stop`, `tt-start`) |
| `make e2e`, `make tt-e2e`, `make nvidia-e2e` | Runs the e2e suite, or its hardware suites on those hosts |
| `make test` | Runs the Go tests, with a throwaway Memgraph for the graph tests |

## Endpoints

`make dev` serves these on the local machine.

| Service | URL |
|---|---|
| API (agents and UI), all operations | http://localhost:8081 |
| OpenAPI schema | http://localhost:8081/openapi.json |
| API reference | http://localhost:8081/docs |
| Agent digest | http://localhost:8081/explore |
| Coordinator digest | http://localhost:8081/explore/coordinator |
| metrics-service (internal) | http://localhost:8084 |
| UI | http://localhost:3000 |
| GreptimeDB | http://localhost:4010 |
| Memgraph (bolt) | bolt://localhost:7687 |
| MinIO | http://localhost:9000 |
| Postgres | localhost:5433 |

One port serves the whole public API, described by one OpenAPI document. Agents use HypothesisLoop
only through this REST API and never touch a cluster. `GET /explore` is their digest of every
endpoint.

## Testing

The e2e tests are pytest files in `tests/e2e/`, using the API client and helpers in
`tests/support/`. Most use fake accelerator types and run on any k3s cluster.

| Marker | Meaning |
|---|---|
| `parallel` | API only, runs concurrently |
| `exclusive` | needs the whole cluster idle |
| `slow` | long-running |
| `hardware` | needs real accelerators. Run with `-m hardware` or `make tt-e2e`, `make nvidia-e2e`. |
| `real_providers` | rents sandboxes or GPU instances at real vendors. Run with `make e2e RENT=1`. |
| `k8s_runtime` | reads what the stack's cluster ran with kubectl |

`make e2e` (`localdev/e2e.sh`) runs the suite against the stack `make dev` runs and the local k3s
cluster. It brings the dev nodes up, hands the tests the API address, a staff token and a
kubeconfig, and leaves everything as it found it. Unless `RENT=1`, every cluster but `local` is
turned off for org `default` while it runs, so it rents nothing. It refuses a stack another run
holds (`/tmp/hl-stack.lock`). Each test creates its own org, project and experiments.

```bash
make dev                                             # the control plane and UI
make reload                                          # once, and after a runtime change
make e2e                                             # every test that needs no real accelerator and rents nothing
make e2e E2E_ARGS='e2e/test_accounts.py -x'          # pass pytest arguments instead
make e2e E2E_ARGS='e2e -m "not exclusive and not slow and not hardware and not real_providers"'   # the quick lane
make e2e E2E_ARGS='e2e -k job_lifecycle'             # only tests whose name matches
make e2e RENT=1                                      # the tests that rent machines
```

The cluster's runtime is images. `make reload` rebuilds and installs them after changes under
`runtime/k8s` or packages they use. A control plane change needs `make dev-api`.

To add a test, start from `tests/support/api.py`, `tests/support/wait.py` and the fixtures in
`tests/conftest.py` (`api`, `pe`, `experiment`, `deadline`).

The UI has its own checks:

```bash
cd controlplane/ui
npm run lint && npm run typecheck && npm test      # static checks and unit tests
npm run e2e                                        # a real browser against the stack make dev runs
```

The browser run leaves the UI on port 3000 alone and serves its own copy on port 3100, which its
browser reaches as `http://localhost:3000` through a proxy, the one origin the API allows. It
refuses a stack another run holds, rents nothing and cleans up what it created.
