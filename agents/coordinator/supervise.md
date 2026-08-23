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

The `BASELINE` block in the live description (`setup.md` step 2, `experiment-checklist.md` item 1)
is the thing to watch, not a number you hold in your head.

- Still `not yet established` once the first jobs have completed is a blocker, not a note: queue
  the baseline run now, concurrently, not once things are quiet. Check for an in-flight or
  completed one first so you don't duplicate it.
- When a baseline job completes, fill the block in the same turn — `metric:` with the value it
  reported, `measured:` with that experiment's id, `code_ref:` with the commit it actually ran —
  then `PUT` the refreshed description and diff it back, exactly as below. A baseline sitting
  measured-but-unpublished is worth nothing to the agents being ranked against it.
- If a later run makes the block wrong (the pin moved, the config changed), that is a description
  edit like any other and follows the same same-turn sync rule.

## Keeping the live description in sync

Every time you edit `experiment.md` or `FINAL_RESULT.md` (a resolved question, a redirect, a new
recommended direction), immediately `PUT` the refreshed `EXPERIMENT DESCRIPTION` block to the live
platform experiment's `description` field, then `GET` it back and diff against the file — see
`setup.md` step 2. Don't rely on a hypothesis comment alone to redirect an agent's in-flight retry
loop; it has failed to stop one before. If an agent keeps retrying past a comment, verify the
*description* actually changed before assuming the agent is ignoring you.

That `PUT` now emits a `platform_experiment.description` event on `/watch`, and agents are told to
re-read the description when it fires. That does not weaken anything above: the event carries no
text, so the description remains the only place the new question exists, and an agent that is
mid-job, disconnected or restarting learns nothing until it reads it. The event only shortens the
delay between your edit and the agent noticing — which is why the edit still has to happen, and
still has to be read back and diffed. An unsynced description is silent in exactly the same way it
was before; it now also emits nothing, so nobody is even nudged to look.

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
