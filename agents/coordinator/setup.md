# Coordinator: setup & debugging

Get from nothing to "agents running real jobs." Then hand off to `supervise.md`. Come back here
when supervising surfaces an environment/platform problem.

## Variables

Set by the caller (e.g. `repeatable/oversee-experiment-record-all-issues.md`) or default as
shown. Resolve against the live environment, never guess.

    EXPERIMENT=<name>                          # required — agents/coordinator/experiments/<name>/
    NUM_AGENTS=2
    CHIPS_PER_AGENT=2                          # NUM_AGENTS * CHIPS_PER_AGENT <= available capacity
    CLAUDE_CODE_OAUTH_TOKEN=<auto-loaded from agents/coordinator/.env, see step 1>
    KUBE_CONTEXT=k3s-tt
    NODE_NAME=tt-quietbox
    CHIP_ARCH=tenstorrent.com/chipArch=blackhole
    API_URL=http://localhost:8081              # the whole API: platform experiments, jobs,
                                               # hypotheses, metrics, resource catalog
    METRICS_URL=http://localhost:8084
    CODE_REPO_URL=<set below>
    GIT_TOKEN=<set below>
    STAGES=<[]domain.Stage or unset>           # unset = domain.DefaultStages
    FINDINGS_FILE=fix-later.md
    PLATFORM_EXPERIMENT_ID=<set in step 2>

You never act on agents' behalf (no SSH into their jobs, no hand-editing their branch/metrics) —
capacity, environment, unblocking is your job; the research is theirs. Platform code changes
follow `important.md`. If a step below can't be done as written, stop and say why — don't
silently improvise.

## 1. Prepare the environment (all six, every time — don't assume from a prior session)

0. **Claude auth.** `set -a; source agents/coordinator/.env 2>/dev/null; set +a` — loads the
   persistent `CLAUDE_CODE_OAUTH_TOKEN` (gitignored) if present. Missing file → step 4 falls back
   to mounted dotfiles.
1. **Capacity.** `lib_attach_node $KUBE_CONTEXT $NODE_NAME` (`localdev/lib/node.sh`, not manual
   kubectl). Confirm `GET $API_URL/resource-catalog/capacity` shows `$CHIP_ARCH` with >=
   `NUM_AGENTS*CHIPS_PER_AGENT` devices. `lib_detach_node` again once done.
   **Note on board topology:** some Tenstorrent boards physically pair multiple ASICs on one card
   (e.g. P300 = 2 ASICs per physical board). Exposing a single `/dev/tenstorrent/N` node or
   setting a single-chip env var (e.g. `TT_METAL_VISIBLE_DEVICES`) does not give true isolation —
   the runtime still detects the sibling ASIC and may reject the topology (e.g. "Board has 1
   chips, but expected 2 chips for board type p300") or fall back to a custom cluster type
   requiring an explicit mesh-graph descriptor. When planning per-chip capacity/allocation, treat
   paired-ASIC boards as a single allocatable unit unless a validated mesh-graph descriptor for
   the split configuration exists.
2. **Control plane + agents up**: `controlplane/infra/podman.sh`, cluster-agent
   DaemonSet/Deployment on `$KUBE_CONTEXT`.
   **After any host reboot, check these too — none of them auto-restart:** the Tenstorrent DRA
   driver pod (kubelet sometimes fails to re-register it; `kubectl delete pod -n
   tt-operator-system -l app.kubernetes.io/name=tt-dra-driver` forces a clean re-register — job
   pods fail with `CDI device injection failed` until this is done), `git-daemon` (`make
   git-daemon-start`; job pods fail cloning with `connection refused` until it's back), and any
   bare-metal node's `bare-agent` process (no systemd unit — restart its `start.sh` by hand; if it
   used an ephemeral cloudflare tunnel URL, prefer a plain LAN URL instead since that survives
   reboots without needing a new tunnel).
3. **Shared code repo** `$CODE_REPO_URL` seeded with `$EXPERIMENT`'s own `seed/` (never
   `tests/workloads/`). A missing `seed/` isn't automatically a bug — some experiments point at a
   sibling's `seed/` in prose; check `experiment.md` first.
   **`$CODE_REPO_URL` is a separate repo, not a live mirror of this one** — after editing
   `seed/`, push it to `$CODE_REPO_URL`'s `main` too, and diff-check before spawning agents.
   **`CODE_REPO_URL` must use the host's LAN IP (e.g. `git://192.168.1.76/...`), never
   `127.0.0.1`.** The coordinator/agent containers run `--network host` so `127.0.0.1` resolves
   there, but a job pod (k3s) is in a separate network namespace and can't reach the host's
   loopback — `code_ref` built from a `127.0.0.1` `CODE_REPO_URL` fails to clone inside every job,
   with no useful error beyond "connection refused". Find the LAN IP with `ip -4 addr show`
   (the address on the main interface, not `lo`/`docker0`/`br-*`/`tailscale0`/`flannel.1`/`cni0`).
   Confirm reachability from *inside a real job pod*, not just from the coordinator container.
   **`data_store.endpoint` in `controlplane/settings/hypothesisloop.yaml` has the same rule and
   the same reason.** Job pods reach the object store directly to write checkpoints (the platform
   is not in the data path), so it must be the host LAN address too — the shipped value is the
   placeholder `http://REPLACE-WITH-HOST-LAN-IP:9000`, and loading a config whose endpoint is
   loopback is refused outright rather than failing later inside a job. `podman.sh up` publishes
   MinIO on `:9000` and creates the bucket; the LAN address is reachable from the control-plane
   containers as well, so one value serves both.
4. **Job image** built before agents start (e.g. `make sparse-sdpa-workload-image`), smoke-tested
   on real hardware (`podman run --device /dev/tenstorrent/0 -v
   /dev/hugepages-1G:/dev/hugepages-1G ...`) — a missing hugepages mount fails with
   `RuntimeError: Querying size for a host channel that does not exist.`, which looks like a
   broken image but isn't.
   **On a k8s cluster ($KUBE_CONTEXT), a podman-level smoke test alone doesn't prove a real job
   pod can run it.** `podman build` writes to podman's own local image store, entirely separate
   from k3s's containerd store; job pods use `ImagePullPolicy: PullIfNotPresent`
   (`runtime/k8s/internal/k8sexec/job_build.go`), so an image that's only in podman's store is
   NOT present as far as containerd is concerned — it tries a real network pull of
   `localhost/<image>:latest`, which fails immediately (`dial tcp 127.0.0.1:443: connection
   refused`) since nothing is listening there. This LOOKS like (and gets misdiagnosed as) "the
   image isn't buildable/pullable, fall back to a from-source build" — it's actually just a
   missing import step. Fix: `podman save localhost/<image>:latest | sudo k3s ctr images import
   -` (see `localdev/k3s-tenstorrent-qb2/reload.sh` for the pattern used by the platform's own
   fixed images) for every experiment-specific job image, not just the platform's generic ones.
   Verify with `sudo k3s ctr images ls | grep <image>` before spawning any agents.
5. `make git-daemon-start` (safe to re-run).

## 2. Create the platform experiment

`POST $API_URL/platform-experiments`, `description` = `$EXPERIMENT`'s `experiment.md`
`EXPERIMENT DESCRIPTION` block **verbatim** (the only thing agents read about the objective —
system prompt is experiment-agnostic).

Required fields (`GET $API_URL/openapi.json` is the source of truth): `name`, `description`,
`budget_accelerator_hours`, `max_agents`, `metrics`, `report_interval_seconds`, `starts_at`,
`ends_at` (RFC3339).

- `budget_accelerator_hours = NUM_AGENTS * CHIPS_PER_AGENT * planned_run_hours` — not a round
  number, or quota throttles the fleet mid-run. Count every agent you will launch here, including
  the non-competitors step 3 decides on: they draw real quota from the same budget.
- `max_agents` is the size of the *ranked* field — it counts competitors only, so a baseline or a
  reviewer never displaces one.
- `metrics`: `{key, direction, role?}[]` — `"role":"ranking"` on the metric `experiment.md` calls
  RANKING METRIC, no role on the rest.
- Every reported metric value has an implicit `metric_basis`, default `"raw"`. If a job's value
  is on a different scale/definition than every other run's (e.g. denormalized against a
  different reference), the job must set a non-`"raw"` `metric_basis` on that value — never
  leave it as `"raw"` and rely on someone noticing the number looks off. A wrongly-"raw" value is
  how a rescaled metric quietly wins a ranking it actually lost.
- `report_interval_seconds` = the cadence the harness actually reports at.
- `stages` (optional; omit = `domain.DefaultStages`, 40%/75%-cut then 60%/0%): `length_pct` sums
  to 100, last stage's `evict_pct` = 0, `max_job_hours` caps a stage's job length (0/absent =
  unlimited). Working short-jobs-first example: `[{20,50,0.25},{30,50,1},{50,0}]` as
  `length_pct,evict_pct,max_job_hours`. If `experiment.md` describes a phased method (e.g.
  screen-then-confirm), it should also carry a coordinator note with the matching `stages` values
  to actually enforce that phasing — check for one before defaulting or inventing your own split.
- Record the returned `id` as `PLATFORM_EXPERIMENT_ID`.
- **Description sync is not optional, ever.** Agents read the live platform experiment's
  `description` field — not `experiment.md`, not `FINAL_RESULT.md`. Any edit to either of those
  files (a resolved question, a new redirect, a stage/direction change) is incomplete until it is
  mirrored: `PUT $API_URL/platform-experiments/{id}` with `description` = the refreshed
  `EXPERIMENT DESCRIPTION` block verbatim, immediately, same turn as the file edit — never batched
  for later. Then `GET $API_URL/platform-experiments/{id}` and diff its `description` against the
  file's block before moving on; treat any mismatch as a blocker, not a note-for-later. This has
  caused wasted-retry-loop incidents twice already (agents burning device time re-deriving a
  question the coordinator had already resolved locally, because the live description hadn't
  caught up) — a posted comment redirecting the agent is not a substitute for this and does not
  reliably stop an in-flight retry loop by itself.
- **The `BASELINE` block must be present and answered before any agent is spawned.** It lives
  inside the `EXPERIMENT DESCRIPTION` block you just posted, so it reaches agents verbatim through
  the sync rule above — see `experiment-checklist.md` item 1 for the four required lines. Check
  the posted `description`, not just the file. Either `metric:` is a real number with an
  experiment id on `measured:`, or `measured:` says `not yet established` **and** the description
  names establishing it as the first task. A block that is absent, or present with all four lines
  blank, is a blocker — fix `experiment.md`, re-`PUT`, and only then spawn. An agent that reads no
  baseline invents one, and every result it ranks is measured against a different control than its
  neighbour's.
- `starts_at` must be several minutes in the future, never "now" — signup only succeeds while
  status is `Open`, and it auto-flips to `Running` the instant `starts_at` passes (`SweepAutoStart`
  in `controlplane/services/quota/platform_experiments_lifecycle.go`). An agent container takes
  ~30-55s to boot (deps install + Claude Code CLI init) before it can call signup; `starts_at=now`
  leaves a race where a slower agent gets `signup_closed: experiment is running` and never runs a
  single job. A 3-5 min buffer after creation covers it comfortably.

## 3. Decide the roles

Every agent signs up in exactly one role, fixed at signup, chosen here and never by the agent
itself. Decide the mix before spawning anything and write the reason into the run's notes, because
the mix is what the results mean:

- `competitor` (default) — ranked, cut at stage boundaries, takes a quota share, eligible for the
  top-3 bonus. An experiment that only wants a competition launches these and nothing else, which
  is the common case.
- `baseline` — runs the `BASELINE` block's configuration and publishes the control's number.
  Never ranked, never cut. Launch one when the experiment's conclusion is a *comparison* ("X% over
  the reference") rather than an ordering, or when the `BASELINE` block says the control is not
  yet established: the alternative is every competitor measuring its own control, which makes the
  results incomparable. Its quota comes out of the same budget as everyone else's, so size the
  budget for competitors + baselines, not competitors alone.
- `reviewer` — re-checks the metrics and `code_ref` behind other agents' claims and records
  agreement or dispute as comments. Never ranked, never cut, small or zero accelerator quota.
  Launch one when a wrong claim is expensive — a long run where agents build on each other's
  findings, or one whose output is a recommendation someone will act on.

`max_agents` counts competitors only, so adding a baseline or a reviewer never shrinks the field.
Nothing else in the platform branches on role: every role's jobs are admitted, billed, evicted and
settled by identical code, every role's metrics are recorded and readable in full, and every role
must file its findings before submitting again.

## 4. Spawn agents

`NUM_AGENTS = floor(available_devices / CHIPS_PER_AGENT)` competitors, not a fixed count, plus
whatever non-competitors step 3 decided on. Build once: `make experimentator-image
EXPERIMENT=$EXPERIMENT`. One container per agent, unique `AGENT_ID`, shared
`PLATFORM_EXPERIMENT_ID`, and `AGENT_ROLE` = that agent's role (omit it and the agent is a
competitor; an unrecognized value fails the container at startup rather than defaulting):

    podman run -d --name agent-<id> --network host --userns=keep-id \
      -e AGENT_ID=agent-<id> -e PLATFORM_EXPERIMENT_ID=$PLATFORM_EXPERIMENT_ID \
      -e AGENT_ROLE=competitor \
      -e API_URL=$API_URL \
      -e CODE_REPO_URL=$CODE_REPO_URL -e GIT_TOKEN=$GIT_TOKEN \
      ${CLAUDE_CODE_OAUTH_TOKEN:+-e CLAUDE_CODE_OAUTH_TOKEN=$CLAUDE_CODE_OAUTH_TOKEN} \
      -v ~/.claude/.credentials.json:/home/agent/.claude/.credentials.json:ro \
      -v ~/.claude.json:/home/agent/.claude.json:ro \
      -v ~/.codex/auth.json:/home/agent/.codex/auth.json:ro \
      localhost/hypothesisloop-experimentator-$EXPERIMENT

Default: the token from `agents/coordinator/.env` (loaded in step 0). Otherwise: mount the host's
`~/.claude` / `~/.codex` credentials as shown above. Both can be set at once — the SDK always
prefers the token. Never use `--bare`; it ignores the token.

One shared private `$CODE_REPO_URL`; each agent works on branch
`agent-<id>-$PLATFORM_EXPERIMENT_ID` (platform experiment id included so a fresh experiment always
starts from a fresh branch off current `main` — see the agent's own system prompt for why) so
`code_ref` (`$CODE_REPO_URL@<sha>`) always resolves to a real commit.

The role reaches the platform through the agent's own signup, so confirm it landed as intended
before handing off: `GET $API_URL/platform-experiments/{id}/signups/{agent_id}` per agent. A role
is fixed at signup and there is no path to change it — a wrong one means stopping that container,
which has not yet signed up under the right role, and relaunching it with a fresh `AGENT_ID`.

Hand off to `supervise.md` once agents are running.

## Fixing a blocker (called from supervise.md)

Diagnose from the actual failure (logs/job status/API response), not the symptom's shape.

- Platform bug → fix in code, per `important.md`
- Experiment-definition gap → fix `seed/`/`job.yaml`/`experiment.md` directly, check against
  `experiment-checklist.md`.
- Environment/capacity → redo the relevant step-1 check, don't work around it per-agent.
