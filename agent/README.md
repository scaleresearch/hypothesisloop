# agent/ — OpenResearch LLM research agent

A standalone process — literally Claude Code with a goal — that signs up for a platform
experiment and runs autonomously: it decides for itself what hypotheses to test, submits real
jobs, reads results, and keeps going until it judges the goal is met or nothing more is worth
trying. Nothing about *which* trials to run is scripted. It runs on the
[Claude Agent SDK](https://code.claude.com/docs/en/agent-sdk) (`claude_agent_sdk`), unrestricted
inside its own container — the standard Bash/Read/Write/Edit/Glob/Grep/WebFetch/WebSearch tool
set, `permission_mode="bypassPermissions"`, no bespoke tool layer of our own.

Fully generic: this process is not wired to any one workload. Given (at most) a
`platform_experiment_id`, it discovers everything else — the job spec to submit, the metric to
optimize, competition rules — by reading the platform experiment's own `description` field over
the API. That's what lets multiple copies of this same process (different `AGENT_ID`, same
`PLATFORM_EXPERIMENT_ID`) genuinely compete: each signs up, checks live accelerator capacity
before picking hardware, forms a hypothesis, submits a real job, polls its own job status via
plain `curl` calls it writes itself, reads back metrics, files a summary, and keeps iterating —
all agents read the same description and are ranked on the same declared metric, with no shared
code beyond the platform APIs themselves. It stops cleanly on its own once the platform experiment
closes or it judges nothing more is worth trying.

## Layout

```
agent/
  pyproject.toml           # uv-managed; `uv run python3 -m openresearch_agent`
  src/openresearch_agent/
    config.py               # settings from env vars
    dotenv.py               # minimal .env loader
    api_client.py            # tiny client for the outer loop's own "is my PE closed?" check
    llm_agent.py             # system prompt + Claude Agent SDK invocation + main()
    __main__.py               # `python3 -m openresearch_agent` entrypoint
  Dockerfile                # non-root (required — see below), container is the isolation boundary
  .env / .env.example       # ANTHROPIC_API_KEY, ANTHROPIC_MODEL (gitignored)
```

No local state file, no custom tool-calling loop, no hand-rolled context management. Everything
about the agent's jobs, hypotheses, and metrics is queryable live from the platform APIs
(`GET /experiments?agent={id}`, `GET /registry/hypotheses?platform_experiment_id=...`, `GET
/registry/experiments/{id}/metrics`) — a restart just means asking the model to re-orient itself
via those same calls.

The API contract itself is also fetched live, not checked in: on startup the agent GETs
`{QUOTA_URL}/explore`, `{SCHED_URL}/explore` and `{REGISTRY_URL}/explore` (see
`api_client.PlatformClient.fetch_api_guide`) and splices the result into the system prompt.
Those compact digests are generated from the running
services' Huma-registered operations, so they never drift; the full machine-readable OpenAPI 3.1
spec for each service is at its `/openapi.json`. There is no hand-maintained API doc to keep in
sync.

## Why the Claude Agent SDK, not a hand-rolled loop

An earlier version of this agent was a plain Python ReAct loop against OpenRouter, with a custom
`run_bash`/`call_api` tool pair and a hand-written history-trimming hack to keep context bounded.
All three of those are things the Claude Agent SDK already solves properly: it *is* Claude Code
packaged as a library — same agent loop, same real tool set (no need to define our own bash
wrapper), and **real server-side context compaction** instead of blind truncation, so it can
actually run for hours without degrading. It still runs entirely on our own infrastructure (same
container, calling our own platform APIs) — nothing is hosted by Anthropic; that's Managed
Agents, a different product. The tradeoff versus OpenRouter is model choice: the SDK talks to the
real Anthropic API, not an OpenAI-compatible proxy, so it's Claude-only (moot here since we'd
already standardized on Sonnet 5).

## How it works

1. `llm_agent.main()` loads config, fetches the live API guide from each service's `/explore`,
   builds one system prompt (assignment + full API reference + platform rules), and calls
   `claude_agent_sdk.query()` with `prompt="Begin."`.
2. Nothing is done on the model's behalf — it registers itself, discovers or is assigned a
   platform experiment, reads its `description` for the job spec/metric/rules, signs up, checks
   `/resource-catalog/capacity` before picking hardware, forms hypotheses, submits jobs (via
   plain `curl` through the Bash tool — no custom API wrapper), watches them, reads metrics, and
   files summaries, entirely through its own tool calls.
3. The outer Python loop just watches the SDK's message stream for stop conditions (below) — it
   does not drive the conversation.

## Auth

- **Local dev machine already `claude login`'d:** works with zero config — the bundled CLI reads
  the same `~/.claude` credential store any Claude Code session does.
- **Docker / production:** set `ANTHROPIC_API_KEY` in `.env` (or the container's environment).
  There's no ambient login inside a fresh container, and a missing key fails fast and cleanly
  ("Not logged in · Please run /login", clean exit — not a crash).

## Long-running behavior

The Claude Agent SDK handles context growth itself — real server-side compaction as the
conversation approaches its context limit, not something this codebase manages. There is nothing
here equivalent to the old `_trim_history` hack; that's exactly the class of problem the SDK
migration was for.

## Stop conditions

- The model finishes its turn with nothing left to do (natural `end_turn`).
- `MAX_LLM_TURNS` reached (0 = unlimited) — surfaces as a `ResultMessage`, handled as a clean
  stop, not an error.
- `MAX_WALL_HOURS` elapsed (0 = unlimited — pairs with the platform-side `ends_at` auto-close;
  see `controlplane/services/quota/platform_experiments_lifecycle.go`'s `StartExpirySweep`).
- The platform experiment itself transitions to `closed` — checked between each message in the
  stream.
- `SIGTERM`/`SIGINT` — stops after the in-flight message, then exits.

Note: every `ResultMessage` (whatever its subtype) marks the SDK session as over — advancing the
message stream again after one raises. The loop treats *any* `ResultMessage` as an unconditional
stop signal for this reason, confirmed live (including the ordinary `subtype="success"` case).

## Running it

```bash
cd agent
uv sync
cp .env.example .env   # fill in ANTHROPIC_API_KEY (or rely on local `claude login`)

export AGENT_ID=agent-1
export PLATFORM_EXPERIMENT_ID=pe-...          # optional — omit to let the agent discover one itself
export QUOTA_URL=http://<host>:8081 SCHED_URL=http://<host>:8082 REGISTRY_URL=http://<host>:8083
export AGENT_GOAL="Optimize val_accuracy by trying different configurations."

uv run python3 -m openresearch_agent
```

Or in Docker (container is the sandbox boundary — no restrictions inside it):

```bash
docker build -t openresearch-agent .
docker run --rm --network host --env-file .env \
  -e AGENT_ID=agent-1 -e PLATFORM_EXPERIMENT_ID=pe-... \
  -e QUOTA_URL=http://localhost:8081 -e SCHED_URL=http://localhost:8082 -e REGISTRY_URL=http://localhost:8083 \
  openresearch-agent
```

The container **must** run as a non-root user (already set up in the Dockerfile) —
`permission_mode="bypassPermissions"` maps to `--dangerously-skip-permissions`, which the CLI
refuses to run as root/sudo as a safety guard, failing fast with a clear error rather than
silently degrading.

Run several instances (different `AGENT_ID`, same `PLATFORM_EXPERIMENT_ID`) for multiple
concurrent agents — capacity/admission across them is enforced platform-side.

## Known gaps (honest, not fixed here)

- **No process supervision.** Nothing restarts the process if killed unexpectedly — wrap it in
  systemd/a container restart policy for real days-long unattended operation. With no local state
  to corrupt, a restart is just "run it again and let the model re-orient via the API."
- **No orchestration.** Spinning up N agents against a platform experiment, or scheduling agents
  to run on a cadence, is entirely manual today — an operator runs processes/containers by hand.
- **Orphaned child process on ungraceful termination.** If the outer Python process is killed
  hard (`kill -9`) or exits abnormally before `gen.aclose()` runs, the bundled Claude Code CLI
  child (`claude_agent_sdk`'s `_bundled/claude`) can survive as an orphan rather than being
  reaped with its parent — `kill -9 <pid>` on the tracked PID alone is not sufficient. A process
  supervisor wrapping this agent for real deployment should kill the whole process group on
  restart, not just the tracked PID. Not fixed here; graceful shutdown (`SIGTERM`, already
  handled — see `_install_signal_handler`) does not have this problem, only hard kills do.
