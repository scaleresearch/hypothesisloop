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
2. **Control plane + agents up**: `make controlplane-up` (podman compose, see
   `localdev/controlplane/docker-compose.yml`), cluster-agent DaemonSet/Deployment on
   `$KUBE_CONTEXT`.
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
   loopback is refused outright rather than failing later inside a job. `make controlplane-up`
   publishes MinIO on `:9000` and creates the bucket; the LAN address is reachable from the
   control-plane containers as well, so one value serves both.
4. **Job image** built before agents start (e.g. `make sparse-sdpa-workload-image`), smoke-tested
   on real hardware (`podman run --device /dev/tenstorrent/0 -v
   /dev/hugepages-1G:/dev/hugepages-1G ...`) — a missing hugepages mount fails with
   `RuntimeError: Querying size for a host channel that does not exist.`, which looks like a
   broken image but isn't.
   **On a k8s cluster ($KUBE_CONTEXT), a podman-level smoke test alone doesn't prove a real job
   pod can run it.** Job images are pushed to the local registry
   (`localdev/controlplane/docker-compose.yml`'s `registry` service) and pulled normally
   (`imagePullPolicy: IfNotPresent`) — a build that never made it past `podman build` (no
   `podman push`) is invisible to every cluster node the same way a typo'd tag is. Build with
   the per-experiment `seed/build_and_push.sh` where one exists (it pushes and renders the
   job.yaml's placeholder image ref for you), or `make <name>-workload-image` for the platform's
   own images. Verify with `podman pull --tls-verify=false localhost:5000/<image>:<tag>` from
   any cluster node before spawning any agents.
5. `make git-daemon-start` (safe to re-run).

## 2. Create the platform experiment

`agents/coordinator/experiments/$EXPERIMENT/experiment.yaml` is the checked-in template for this
(copied from `template/experiment.yaml` when the experiment was defined — see
`experiment-checklist.md` item 1). Fill in its `REPLACE-WITH-...` placeholders
(`budget_accelerator_hours`, `max_agents`, `starts_at` — these depend on `NUM_AGENTS`/
`CHIPS_PER_AGENT`/`planned_run_hours`, decided now, not ahead of time) and confirm its
`description` is still `experiment.md`'s current `EXPERIMENT DESCRIPTION` block verbatim (the only
thing agents read about the objective — system prompt is experiment-agnostic), then:
`hl platform-experiments create agents/coordinator/experiments/$EXPERIMENT/experiment.yaml` (see
`controlplane/settings/examples/experiment.yaml` for the full DSL reference).

Required fields (`GET $API_URL/openapi.json` is the source of truth): `name`, `description`,
`budget_accelerator_hours`, `max_agents`, `metrics`, `report_interval_seconds`, `starts_at`,
`ends_at` (RFC3339).

- `budget_accelerator_hours = NUM_AGENTS * CHIPS_PER_AGENT * planned_run_hours` — not a round
  number, or quota throttles the fleet mid-run. Count every agent you will launch here.
- `max_agents` is the size of the ranked field — every agent that signs up competes and counts
  against it, there is no separate role that doesn't.
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
  mirrored: update `agents/coordinator/experiments/$EXPERIMENT/experiment.yaml`'s `description` to
  the refreshed `EXPERIMENT DESCRIPTION` block verbatim, then `hl platform-experiments update --id
  $PLATFORM_EXPERIMENT_ID agents/coordinator/experiments/$EXPERIMENT/experiment.yaml`, immediately,
  same turn as the file edit — never batched for later. (While the platform experiment is
  `running`, only `name`/`description` are actually amended by this call — see `hl
  platform-experiments update --help` — which is exactly what a description sync needs.) Then `hl
  platform-experiments list` or `GET $API_URL/platform-experiments/{id}` and diff its
  `description` against the file's block before moving on; treat any mismatch as a blocker, not a
  note-for-later. This has
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
  baseline invents one, and every result it ranks is measured against a different reference than
  its neighbour's.
- `starts_at` must be several minutes in the future, never "now" — signup only succeeds while
  status is `Open`, and it auto-flips to `Running` the instant `starts_at` passes (`SweepAutoStart`
  in `controlplane/services/quota/platform_experiments_lifecycle.go`). An agent container takes
  ~30-55s to boot (deps install + Claude Code CLI init) before it can call signup; `starts_at=now`
  leaves a race where a slower agent gets `signup_closed: experiment is running` and never runs a
  single job. A 3-5 min buffer after creation covers it comfortably.

## 3. Decide the flavor mix and the sweep

Every agent that signs up competes — ranked, cut at stage boundaries, eligible for the top-3
bonus. There is no other role, and the platform never branches on one: every agent's jobs are
admitted, billed, evicted and settled by identical code.

What differs between agents is two independent things, both decided here and set at launch:

- `AGENT_FLAVOR` picks which specialization the agent reads from
  `agents/experimentator/src/hypothesisloop_agent/prompts/flavors/<flavor>.md` — what kind of
  trial it runs and what it varies (`generalist`, `hyperparameter-search`, `architecture-search`;
  add a new file for a new specialization). Decide the flavor mix before spawning — e.g. 2
  hyperparameter-search + 1 architecture-search + 1 generalist — and write the reason into the
  run's notes, since it's what the results mean.
- `AGENT_HYPERPARAMETERS` is a small JSON object handed to an agent on top of its flavor (batch
  size, a specific point in a search space, ...). A sweep is several agents of the same flavor
  launched with different hyperparameter values, same as XManager's `experiment.add` loop.

If you want a single agent to just replicate the `BASELINE` block's configuration verbatim, launch
it as `generalist` with that config as its hyperparameters (or none at all, and let it read
`BASELINE` from the description) — it competes and is ranked like everyone else.

## 4. Spawn agents

`NUM_AGENTS = floor(available_devices / CHIPS_PER_AGENT)`, one container per agent, unique
`AGENT_ID`, shared `PLATFORM_EXPERIMENT_ID`, each agent's own `AGENT_FLAVOR` (omit it and the
agent gets `generalist`) and `AGENT_HYPERPARAMETERS` (a JSON object; omit it and the agent gets
`{}`). Build once: `make experimentator-image EXPERIMENT=$EXPERIMENT`.

    podman run -d --name agent-<id> --network host --userns=keep-id \
      -e AGENT_ID=agent-<id> -e PLATFORM_EXPERIMENT_ID=$PLATFORM_EXPERIMENT_ID \
      -e AGENT_FLAVOR=hyperparameter-search -e AGENT_HYPERPARAMETERS='{"batch_size":64}' \
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

Confirm each agent actually signed up before handing off: `GET
$API_URL/platform-experiments/{id}/signups/{agent_id}` per agent.

Hand off to `supervise.md` once agents are running.

## Fixing a blocker (called from supervise.md)

Diagnose from the actual failure (logs/job status/API response), not the symptom's shape.

- Platform bug → fix in code, per `important.md`
- Experiment-definition gap → fix `seed/`/`job.yaml`/`experiment.md` directly, check against
  `experiment-checklist.md`.
- Environment/capacity → redo the relevant step-1 check, don't work around it per-agent.
