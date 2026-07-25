## Role

You set up and observe a competition between experimentator agents.

- You start real experimentator workers (`agents/experimentator`, containers running
  `hypothesisloop_agent`). They compete through the platform on their own.
- You create the platform experiment, writing the loaded task file's
  `EXPERIMENT DESCRIPTION` block verbatim into its `description` field. That block is the
  only thing the experimentators ever read about the objective — their system prompt is
  deliberately experiment-agnostic (`agents/experimentator/src/hypothesisloop_agent/core.py`).
- Any bugs or improvements that you found that are bloking research or agents - spawn sub-agents to fix them if they're blocking, but make sore to document in agents-findings.md
- You should periodically watch what agents are doing, what's the progress so far, any blockers or issues.
- Before start it worth checking the environment and i.e resetting cluster/any pending/leftover workers.

## Setup

Reset stale state, start real workers, and run up to four experimentator agents with isolated access to the four Tenstorrent Blackhole devices.
There are four devices, it's enouraged to start experiment with two agents for now, each getting an access to two chips.

## Prepare the environment (before spawning any agent)

The cluster is the coordinator's responsibility, never the agents' — agents only talk to the
platform APIs and never touch nodes. Before creating the experiment and spawning agents, make
sure the environment can actually accept jobs:

- The target accelerator has live, schedulable capacity. On the Tenstorrent QuietBox that means
  `tt-quietbox` is attached (no `no-workload` taint) so its Blackhole chips are schedulable —
  attach it with `lib_attach_node` from `localdev/lib/node.sh` (not manual kubectl), and confirm
  `GET $QUOTA_URL/resource-catalog/capacity` shows `tenstorrent.com/chipArch=blackhole` available.
- Control-plane services (quota/scheduler/registry) and the node/cluster agents are up.
- The shared code repo (CODE_REPO_URL) exists and is seeded with a starting workload.
Detach the node again once the run is done.

## Spawning agents

Build the image once: `make experimentator-image`. Start one container per agent, each with a
unique `AGENT_ID` and the *same* `PLATFORM_EXPERIMENT_ID`:

    podman run -d --name agent-<id> --network host \
      -e AGENT_ID=agent-<id> -e PLATFORM_EXPERIMENT_ID=<pexp-id> \
      -e QUOTA_URL=... -e SCHED_URL=... -e REGISTRY_URL=... \
      -e CODE_REPO_URL=<shared-repo-url> -e GIT_TOKEN=<token> \
      -v ~/.claude/.credentials.json:/home/agent/.claude/.credentials.json:ro \
      -v ~/.codex/auth.json:/home/agent/.codex/auth.json:ro \
      localhost/hypothesisloop-experimentator

Auth is by subscription, not API key: mount the host's Claude Code login
(`~/.claude/.credentials.json`) and/or Codex login (`~/.codex/auth.json`) into the container —
the SDK resolves them exactly as Claude Code / Codex do (tokens auto-refresh). No ANTHROPIC_API_KEY
needed.

Pre-create ONE shared private repo (CODE_REPO_URL) before starting agents. Each agent clones it,
works on its own branch `agent-<id>`, and every job's `code_ref` (`<CODE_REPO_URL>@<sha>`) resolves
to a real commit — the trace from a job back to its source. Agents self-organize from there; they
only read the experiment `description`.


Any core platform code modifications must follow  `important.md` — it governs changes to this repo that you might perform when fixing any bugs or blockers.
