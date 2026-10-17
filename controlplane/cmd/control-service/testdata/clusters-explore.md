# hypothesisloop cluster-agent API — live API reference

Generated live from the running service's Huma-registered operations. For the full machine-readable schema fetch /openapi.json on this same port.

## cluster-agent

- `GET /` — List clusters and their connectivity
  Every cluster that has ever polled, and whether its agent is connected right now.
- `DELETE /{name}` — Detach this cluster
  The cluster's own runtime ends the cluster, as its owner does with DELETE /orgs/{id}/clusters/{name}: every job placed on it goes back to the queue, its settings and the identity bound to it are deleted, and its runtime token is refused from then on with reason cluster_not_registered. requeued names the jobs.
- `GET /{name}/access` — Check this cluster's runtime token
  Answers 204 to the runtime token of a registered cluster named name, and refuses any other token as every route here does. It records and changes nothing, so an installer's dry run asks it before anything is installed.
- `POST /{name}/builds/{key}` — Report the outcome of an assigned environment build
  outcome pushed makes the environment ready in the image the repository holds under digest; file fails it with log_tail as the file's fault; platform frees the build to be assigned again. Recorded only while the build of key is assigned to this cluster at attempt and still building; otherwise 409. file is the exception: the file is the key's, so it fails a build still building at attempt or a later one, wherever it was assigned again.
- `POST /{name}/builds/{key}/credential` — The push login of an assigned environment build
  A login that writes to the environment repository for at least 25 minutes, handed out only while the build of key is assigned to this cluster at attempt and still building; otherwise 409.
- `POST /{name}/builds/{key}/git-credential` — A read of the repository an assigned environment build is of
  The git proxy address the repository whose environment file this is is read at, the repository's own address, and a platform token that reads that repository alone through the proxy, read-only, while the build of key is assigned to this cluster at attempt and still building. The build mounts them as the git config secret git-credentials of a RUN that asks for it, never into a layer. Handed out only while the build is assigned and building; otherwise 409. 404 for an environment of no repository: the build then mounts no secret.
- `POST /{name}/builds/{key}/loaded` — Report an assigned environment build held on one node
  The image of the build, the repository's under digest, is in the image store of node while its push goes on: a job that waits for the environment and fits on that node is placed there, pinned to it, and runs the image from there before it is pushed. The outcome is still reported once the push ends. Recorded only while the build of key is assigned to this cluster at attempt and still building; otherwise 409.
- `POST /{name}/builds/{key}/progress` — Report the step an assigned environment build is at
  step, one line of at most 512 bytes, replaces the last one reported, and the jobs waiting for the environment show it. Recorded only while the build of key is assigned to this cluster at attempt and still building; otherwise 409.
- `POST /{name}/reconcile` — Exchange actual capacity for desired workloads
  Atomically records one complete actual-capacity snapshot in metrics storage, keeping only priced accelerator types, then returns the complete PostgreSQL desired workload set for this cluster, plus the ids of workloads whose termination earns them their declared checkpoint window, the environment builds assigned to this cluster (builds, recorded as building only for a runtime that reports builds true), and registry_pull, a login that reads environment images, when a listed workload runs in one. The version the agent sends in X-Hypothesisloop-Agent-Version is kept with the snapshot, and a refusal of the request is kept as the cluster's last error until one is accepted.
- `POST /{name}/status` — Push job-phase observations from a cluster-agent
  Body: {"reports": [...]}. The complete snapshot is validated and written atomically; any invalid report rejects the whole request.

