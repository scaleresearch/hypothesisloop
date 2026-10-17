# hypothesisloop cluster-agent API — live API reference

Generated live from the running service's Huma-registered operations. For the full machine-readable schema fetch /openapi.json on this same port.

## cluster-agent

- `GET /` — List clusters and their connectivity
  Every cluster that has ever polled, and whether its agent is connected right now.
- `DELETE /{name}` — Detach this cluster
  The cluster's own runtime ends the cluster, as its owner does with DELETE /orgs/{id}/clusters/{name}: every job placed on it goes back to the queue, its settings and the identity bound to it are deleted, and its runtime token is refused from then on with reason cluster_not_registered. requeued names the jobs.
- `GET /{name}/access` — Check this cluster's runtime token
  Answers 204 to the runtime token of a registered cluster named name, and refuses any other token as every route here does. It records and changes nothing, so an installer's dry run asks it before anything is installed.
- `POST /{name}/reconcile` — Exchange actual capacity for desired workloads
  Atomically records one complete actual-capacity snapshot in metrics storage, keeping only priced accelerator types, then returns the complete PostgreSQL desired workload set for this cluster, plus the ids of workloads whose termination earns them their declared checkpoint window. Each workload names the image it runs in by digest, with image_pull, a read-only login that pulls it, when the image needs one, and the launch script every node runs. The version the agent sends in X-Hypothesisloop-Agent-Version is kept with the snapshot, and a refusal of the request is kept as the cluster's last error until one is accepted.
- `POST /{name}/status` — Push job-phase observations from a cluster-agent
  Body: {"reports": [...]}. The complete snapshot is validated and written atomically; any invalid report rejects the whole request.

