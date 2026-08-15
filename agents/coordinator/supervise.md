# Coordinator: supervise a run

For after `setup.md` has agents running. Variables are the same as `setup.md`. You watch; you
never act on an agent's behalf.

## Poll periodically

Every 5-10 min at first, widen once behavior looks steady.

- `podman logs --tail 100 agent-<id>` — progress or stuck loop?
- `GET $API_URL/platform-experiments/{id}/stages` — current stage, progress, cut agents. A job
  stuck past `max_job_hours` is a bug, not just slow.
- `GET $API_URL/...` — is the ranking metric moving, reporting at `experiment.md`'s cadence?
  Long-running with no new point = likely dead.
- `podman ps` / `$API_URL` job status — stuck/crashed containers, pending jobs (capacity
  starvation, image pull failure).

## When something actually blocks research

Not a style nit, not a hypothetical. Go to `setup.md`'s "Fixing a blocker" section and fix it
there. If the platform experiment itself is unrecoverable (misconfigured, capacity lost), close
and restart clean — but log why in `$FINDINGS_FILE` first. Keep in mind these instructions should be generic and work for every experiment.

## Baseline

No established baseline (number + config) is a blocker: queue one now, concurrently, not once
things are quiet. Check for an in-flight/completed one first to avoid duplicates.

## Record findings

Append to `$FINDINGS_FILE` as you go, not just at the end. Each entry: what happened (observed,
not paraphrased), what you changed and why, resolved or still open. Bias toward what speeds up the
*next* run, not what's already obvious from the experiment's own metrics.

## End the run

- `POST .../platform-experiments/{id}/close` with a JSON body — `{}` is enough (a request with no
  body at all 400s: "request body is required").
- `lib_detach_node $KUBE_CONTEXT $NODE_NAME`.
- `podman stop agent-<id>`; leave branches/commits in `$CODE_REPO_URL` intact.
- Final pass over `$FINDINGS_FILE` — anything still open should say so explicitly.
