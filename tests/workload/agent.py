#!/usr/bin/env python3
"""
OpenResearch autonomous research agent.

Uses Claude with tool use to autonomously:
- Register itself on the platform
- Browse and sign up for platform experiments
- Check experiment-scoped T4h quota
- Submit training jobs tied to platform experiments
- Track progress and cancel underperformers
- React to results and decide next steps

Usage:
  ANTHROPIC_API_KEY=... OPENRESEARCH_AGENT_ID=agent-1 python agent/agent.py

Optional env vars:
  OPENRESEARCH_AGENT_NAME      — display name (default: derived from OPENRESEARCH_AGENT_ID)
  OPENRESEARCH_QUOTA_URL       — base URL of quota service   (default: http://localhost:8081)
  OPENRESEARCH_SCHEDULER_URL   — base URL of scheduler       (default: http://localhost:8082)
  OPENRESEARCH_REGISTRY_URL    — base URL of registry        (default: http://localhost:8083)
  OPENRESEARCH_PROJECT_ID      — project identifier          (default: openresearch)
  OPENRESEARCH_MAX_TURNS       — max agent turns before stopping (default: 20)
"""

import os
import sys
import json
import uuid
import time
import urllib.request
import urllib.error
from pathlib import Path
from typing import Any

import anthropic
import yaml


# Config

AGENT_ID      = os.environ.get("OPENRESEARCH_AGENT_ID", f"agent-{uuid.uuid4().hex[:6]}")
AGENT_NAME    = os.environ.get("OPENRESEARCH_AGENT_NAME", AGENT_ID.replace("-", " ").title())
PROJECT_ID    = os.environ.get("OPENRESEARCH_PROJECT_ID", "openresearch")
MAX_TURNS     = int(os.environ.get("OPENRESEARCH_MAX_TURNS", "20"))

QUOTA_URL     = os.environ.get("OPENRESEARCH_QUOTA_URL",     "http://localhost:8081").rstrip("/")
SCHEDULER_URL = os.environ.get("OPENRESEARCH_SCHEDULER_URL", "http://localhost:8082").rstrip("/")
REGISTRY_URL  = os.environ.get("OPENRESEARCH_REGISTRY_URL",  "http://localhost:8083").rstrip("/")

# Standalone job definition (domain.JobSpec — the platform's own DSL, never a raw k8s
# manifest) sitting next to this script. Loaded once as the base for every submission;
# tool_submit_experiment only overrides the fields the agent actually decides per run
# (gpu_type/gpu_count), same file tests/scenarios/phase2-and-settlement.sh submits verbatim.
JOB_FILE = Path(os.environ.get("OPENRESEARCH_JOB_FILE", Path(__file__).parent / "job.yaml"))

client = anthropic.Anthropic()


# HTTP helpers


def _request(base: str, method: str, path: str, body: dict | None = None, timeout: int = 10) -> tuple[int, Any]:
    url = base + path
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json", "Accept": "application/json"}
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read())
        except Exception:
            return e.code, {"error": str(e)}
    except Exception as e:
        return 0, {"error": str(e)}


def _quota_get(path: str)                         -> tuple[int, Any]: return _request(QUOTA_URL,     "GET",   path)
def _quota_post(path: str, body: dict | None = None) -> tuple[int, Any]: return _request(QUOTA_URL, "POST",  path, body)
def _sched_get(path: str)                         -> tuple[int, Any]: return _request(SCHEDULER_URL, "GET",   path)
def _sched_post(path: str, body: dict | None = None) -> tuple[int, Any]: return _request(SCHEDULER_URL, "POST", path, body)
def _reg_get(path: str)                           -> tuple[int, Any]: return _request(REGISTRY_URL,  "GET",   path)
def _reg_post(path: str, body: dict | None = None) -> tuple[int, Any]: return _request(REGISTRY_URL, "POST",  path, body)


# Tool implementations


def tool_register_agent(agent_id: str, name: str) -> dict:
    status, body = _quota_post("/agents", {"id": agent_id, "name": name})
    return {"status": status, "body": body}


def tool_list_agents() -> dict:
    status, body = _quota_get("/agents")
    return {"status": status, "body": body}


# ---- Platform Experiments --------------------------------------------------

def tool_list_platform_experiments(status: str = "") -> dict:
    path = "/platform-experiments"
    if status:
        path += f"?status={status}"
    s, body = _quota_get(path)
    return {"status": s, "body": body}


def tool_signup_platform_experiment(platform_experiment_id: str) -> dict:
    s, body = _quota_post(f"/platform-experiments/{platform_experiment_id}/signup", {"agent_id": AGENT_ID})
    return {"status": s, "body": body}


def tool_get_experiment_quota(platform_experiment_id: str) -> dict:
    s, body = _quota_get(f"/quota/{AGENT_ID}/experiment/{platform_experiment_id}")
    return {"status": s, "body": body}


# ---- Jobs ------------------------------------------------------------------

def tool_list_experiments(agent_id: str = "", status: str = "") -> dict:
    params = []
    if agent_id:
        params.append(f"agent={agent_id}")
    if status:
        params.append(f"status={status}")
    path = "/experiments"
    if params:
        path += "?" + "&".join(params)
    s, body = _sched_get(path)
    return {"status": s, "body": body}


def tool_get_experiment(experiment_id: str) -> dict:
    status, body = _sched_get(f"/experiments/{experiment_id}")
    return {"status": status, "body": body}


def tool_register_hypothesis(platform_experiment_id: str, text: str) -> dict:
    """Register (or retrieve, if an equivalent one already exists in the same platform
    experiment) a hypothesis. Returns a hypothesis_id that must be passed to
    submit_experiment. Hypotheses are scoped to one platform experiment — this is the real
    uniqueness check: registering text equivalent to an already-registered hypothesis
    (modulo case/whitespace) *in the same platform experiment* returns the existing
    hypothesis_id instead of creating a duplicate — use it to check whether your idea has
    already been claimed before running an experiment."""
    status, body = _reg_post(
        "/hypotheses",
        {"agent_id": AGENT_ID, "platform_experiment_id": platform_experiment_id, "text": text},
    )
    return {"status": status, "body": body}


def tool_list_hypotheses(platform_experiment_id: str) -> dict:
    """List every hypothesis registered so far in a platform experiment, across all agents —
    check this before registering a new one to see what's already been claimed or tested."""
    status, body = _reg_get(f"/hypotheses?platform_experiment_id={platform_experiment_id}")
    return {"status": status, "body": body}


def tool_get_hypothesis(hypothesis_id: str) -> dict:
    """Get a hypothesis, every job (experiment) submitted against it, and every finding
    (post-run write-up) filed against it so far — read the findings before deciding whether
    to test this hypothesis again yourself."""
    status, body = _reg_get(f"/hypotheses/{hypothesis_id}")
    return {"status": status, "body": body}


def tool_submit_experiment(
    platform_experiment_id: str,
    hypothesis_id: str,
    theory: str,
    objective: str,
    gpu_type: str,
    gpu_count: int,
    estimated_duration_hours: float,
    capacity_tier: str = "guaranteed",
) -> dict:
    """Submit a training job. GPU type/count go in `job` (the platform's own DSL — never a
    raw k8s manifest); hypothesis_id/theory/objective/etc go in `metadata`. hypothesis_id
    must come from register_hypothesis (or list_hypotheses/get_hypothesis) — call that first.
    estimated_cost_t4h is computed by the scheduler from job.gpu_type/gpu_count, not sent by
    the caller."""
    exp_id = str(uuid.uuid4())

    # Start from the standalone job definition (image, etc.) and only override what this
    # particular run actually decided (gpu_type/gpu_count) — everything else about how the
    # job executes stays exactly as declared in job.yaml.
    with open(JOB_FILE) as f:
        job = yaml.safe_load(f)
    job["gpu_type"] = gpu_type
    job["gpu_count"] = gpu_count

    payload = {
        "id": exp_id,
        "metadata": {
            "agent_id": AGENT_ID,
            "platform_experiment_id": platform_experiment_id,
            "capacity_tier": capacity_tier,
            "project_id": PROJECT_ID,
            "code_ref": "git://github.com/antonibertel/openresearch@main",
            "config_hash": f"sha256:{uuid.uuid4().hex}",
            "data_ref": "s3://openresearch-data/imagenet-mini@latest",
            "hypothesis_id": hypothesis_id,
            "theory": theory,
            "objective": objective,
            "estimated_duration_hours": estimated_duration_hours,
        },
        "job": job,
    }
    status, body = _sched_post("/experiments", payload)
    return {"status": status, "body": body, "experiment_id": exp_id}


def tool_cancel_experiment(experiment_id: str) -> dict:
    status, body = _sched_post(f"/experiments/{experiment_id}/cancel")
    return {"status": status, "body": body}


def tool_get_metrics(experiment_id: str) -> dict:
    status, body = _reg_get(f"/experiments/{experiment_id}/metrics")
    return {"status": status, "body": body}


def tool_get_lineage(experiment_id: str) -> dict:
    status, body = _reg_get(f"/experiments/{experiment_id}/lineage")
    return {"status": status, "body": body}


def tool_wait(seconds: int) -> dict:
    seconds = min(seconds, 60)
    time.sleep(seconds)
    return {"waited_seconds": seconds}


# Tool dispatch table


TOOLS = [
    {
        "name": "register_agent",
        "description": "Register this agent on the OpenResearch platform. Call once at startup.",
        "input_schema": {
            "type": "object",
            "properties": {
                "agent_id": {"type": "string"},
                "name": {"type": "string"},
            },
            "required": ["agent_id", "name"],
        },
    },
    {
        "name": "list_agents",
        "description": "List all registered agents.",
        "input_schema": {"type": "object", "properties": {}},
    },
    {
        "name": "list_platform_experiments",
        "description": (
            "List platform experiments (the compute envelopes agents compete within). "
            "status: 'open' (accepting signups), 'running' (active, jobs allowed), "
            "'draft', 'closed', or '' (all)."
        ),
        "input_schema": {
            "type": "object",
            "properties": {
                "status": {"type": "string", "description": "Filter by status (open/running/draft/closed) or empty for all"},
            },
        },
    },
    {
        "name": "signup_platform_experiment",
        "description": (
            "Sign up this agent for an open platform experiment. "
            "Must be done before the experiment starts (while status=open). "
            "Returns {status: signed_up} on success."
        ),
        "input_schema": {
            "type": "object",
            "properties": {
                "platform_experiment_id": {"type": "string"},
            },
            "required": ["platform_experiment_id"],
        },
    },
    {
        "name": "get_experiment_quota",
        "description": (
            "Get this agent's T4h quota for a specific platform experiment. "
            "Returns guaranteed and burst buckets with used/remaining amounts. "
            "Check this before submitting to ensure you have enough quota."
        ),
        "input_schema": {
            "type": "object",
            "properties": {
                "platform_experiment_id": {"type": "string"},
            },
            "required": ["platform_experiment_id"],
        },
    },
    {
        "name": "list_experiments",
        "description": "List training jobs. agent_id filters to one agent; status filters by state (QUEUED/RUNNING/COMPLETED/EVICTED/FAILED/CANCELLED).",
        "input_schema": {
            "type": "object",
            "properties": {
                "agent_id": {"type": "string", "description": "Filter to this agent's jobs (empty = all agents)"},
                "status": {"type": "string", "description": "Filter by status"},
            },
        },
    },
    {
        "name": "get_experiment",
        "description": "Get full details and current status of a specific training job.",
        "input_schema": {
            "type": "object",
            "properties": {
                "experiment_id": {"type": "string"},
            },
            "required": ["experiment_id"],
        },
    },
    {
        "name": "register_hypothesis",
        "description": (
            "Register (or retrieve, if an equivalent one already exists in the same platform "
            "experiment) a scientific hypothesis, scoped to one platform experiment's shared "
            "idea pool. Returns a hypothesis_id required by submit_experiment. Registering text "
            "equivalent (modulo case/whitespace) to an already-registered hypothesis in the "
            "same platform experiment returns the existing hypothesis_id instead of creating a "
            "duplicate — call this to validate/retrieve a hypothesis before running an "
            "experiment against it."
        ),
        "input_schema": {
            "type": "object",
            "properties": {
                "platform_experiment_id": {"type": "string"},
                "text": {"type": "string", "description": "The hypothesis statement"},
            },
            "required": ["platform_experiment_id", "text"],
        },
    },
    {
        "name": "list_hypotheses",
        "description": "List every hypothesis registered so far in a platform experiment, across all agents.",
        "input_schema": {
            "type": "object",
            "properties": {
                "platform_experiment_id": {"type": "string"},
            },
            "required": ["platform_experiment_id"],
        },
    },
    {
        "name": "get_hypothesis",
        "description": "Get a hypothesis, every job submitted against it, and every finding (post-run write-up) filed against it so far.",
        "input_schema": {
            "type": "object",
            "properties": {
                "hypothesis_id": {"type": "string"},
            },
            "required": ["hypothesis_id"],
        },
    },
    {
        "name": "submit_experiment",
        "description": (
            "Submit a training job inside a running platform experiment. "
            "The agent must be signed up for the platform experiment. "
            "hypothesis_id must come from register_hypothesis (or list_hypotheses/get_hypothesis) — call that first. "
            "The job will emit metrics defined in the platform experiment; use get_metrics to observe them. "
            "estimated_cost_t4h is computed by the scheduler as: duration × gpu_count × gpu_type_rate. "
            "capacity_tier='guaranteed' is high-priority non-preemptable; 'burst' is preemptable but uses separate quota."
        ),
        "input_schema": {
            "type": "object",
            "properties": {
                "platform_experiment_id": {"type": "string", "description": "ID of the running platform experiment"},
                "hypothesis_id": {"type": "string", "description": "ID from register_hypothesis"},
                "theory": {"type": "string", "description": "Specific prediction/bet this run tests"},
                "objective": {"type": "string", "description": "What we're optimizing"},
                "gpu_type": {"type": "string", "enum": ["T4", "L40", "A100"]},
                "gpu_count": {"type": "integer", "minimum": 1},
                "estimated_duration_hours": {"type": "number"},
                "capacity_tier": {"type": "string", "enum": ["guaranteed", "burst"], "description": "guaranteed=high priority; burst=preemptable"},
            },
            "required": ["platform_experiment_id", "hypothesis_id", "theory", "objective", "gpu_type", "gpu_count", "estimated_duration_hours"],
        },
    },
    {
        "name": "cancel_experiment",
        "description": "Cancel a QUEUED or RUNNING job. QUEUED → full T4h refund to quota; RUNNING → 50% refund.",
        "input_schema": {
            "type": "object",
            "properties": {
                "experiment_id": {"type": "string"},
            },
            "required": ["experiment_id"],
        },
    },
    {
        "name": "get_metrics",
        "description": "Get live metric timeseries for a running or completed experiment.",
        "input_schema": {
            "type": "object",
            "properties": {
                "experiment_id": {"type": "string"},
            },
            "required": ["experiment_id"],
        },
    },
    {
        "name": "get_lineage",
        "description": "Get the lineage chain (parent → child experiments) for an experiment.",
        "input_schema": {
            "type": "object",
            "properties": {
                "experiment_id": {"type": "string"},
            },
            "required": ["experiment_id"],
        },
    },
    {
        "name": "wait",
        "description": "Wait N seconds for training to progress. Max 60s per call.",
        "input_schema": {
            "type": "object",
            "properties": {
                "seconds": {"type": "integer", "minimum": 1, "maximum": 60},
            },
            "required": ["seconds"],
        },
    },
]

TOOL_FN = {
    "register_agent":             lambda inp: tool_register_agent(inp["agent_id"], inp["name"]),
    "list_agents":                lambda inp: tool_list_agents(),
    "list_platform_experiments":  lambda inp: tool_list_platform_experiments(inp.get("status", "")),
    "signup_platform_experiment": lambda inp: tool_signup_platform_experiment(inp["platform_experiment_id"]),
    "get_experiment_quota":       lambda inp: tool_get_experiment_quota(inp["platform_experiment_id"]),
    "list_experiments":           lambda inp: tool_list_experiments(inp.get("agent_id", ""), inp.get("status", "")),
    "get_experiment":             lambda inp: tool_get_experiment(inp["experiment_id"]),
    "register_hypothesis":        lambda inp: tool_register_hypothesis(inp["platform_experiment_id"], inp["text"]),
    "list_hypotheses":            lambda inp: tool_list_hypotheses(inp["platform_experiment_id"]),
    "get_hypothesis":             lambda inp: tool_get_hypothesis(inp["hypothesis_id"]),
    "submit_experiment":          lambda inp: tool_submit_experiment(
        inp["platform_experiment_id"],
        inp["hypothesis_id"],
        inp["theory"],
        inp["objective"],
        inp["gpu_type"],
        inp["gpu_count"],
        inp["estimated_duration_hours"],
        inp.get("capacity_tier", "guaranteed"),
    ),
    "cancel_experiment":          lambda inp: tool_cancel_experiment(inp["experiment_id"]),
    "get_metrics":                lambda inp: tool_get_metrics(inp["experiment_id"]),
    "get_lineage":                lambda inp: tool_get_lineage(inp["experiment_id"]),
    "wait":                       lambda inp: tool_wait(inp["seconds"]),
}


# Agent loop


SYSTEM_PROMPT = f"""You are an autonomous ML research agent on the OpenResearch platform.

Your identity:
  agent_id: {AGENT_ID}
  name: {AGENT_NAME}
  project: {PROJECT_ID}

## Workflow

The OpenResearch platform organises compute into **Platform Experiments** — operator-created
compute envelopes with a fixed T4-GPU-hour (T4h) budget. You must follow this flow:

1. **Register** yourself (ignore 409/500 errors if already registered).
2. **List open platform experiments** and sign up for one that interests you.
3. **Wait for the experiment to start** (status becomes 'running') if it isn't already.
4. **Check your quota** — after signup and start, you'll have two buckets:
   - `guaranteed` (high priority, non-preemptable)
   - `burst` (lower priority, preemptable — refunded if evicted)
5. **Submit training jobs** targeting that platform experiment. Always pass:
   - `platform_experiment_id` — the PE you're competing in
   - `capacity_tier` — 'guaranteed' for important runs, 'burst' for speculative ones
6. **Track jobs** via get_experiment and get_metrics. Cancel underperformers early.

## Cost model

```
estimated_cost_t4h = duration_hours × gpu_count × gpu_cost × 1.2
gpu_cost: T4=1.0, L40=2.0, A100=3.0
```

Your quota is in T4h units. Check get_experiment_quota before submitting.

## Eviction

The platform auto-evicts jobs that:
- Fall below metric-contract checkpoints (below_contract)
- Plateau without improvement (plateau)
- Stop reporting metrics (silent)
- Exceed 1.5× estimated duration (overrun)

Evicted burst jobs get a proportional T4h refund. Guaranteed jobs are not preempted.

## Scientific strategy

- All runs optimize `val_accuracy` on the same synthetic dataset.
- Start with T4 exploration runs (0.03–0.1h) to probe hypothesis space.
- Use burst tier for speculative ideas to preserve guaranteed quota.
- If a run beats its baseline convincingly, chain a follow-up validation run with parent_id.
- The agent seeded by `{AGENT_ID}` explores a distinct hyperparameter region.

Be decisive. Make tool calls, reason about results, take the next action. Don't ask for confirmation.
"""


def run_agent() -> None:
    print(f"[openresearch-agent] Starting: {AGENT_ID} ({AGENT_NAME})")
    print(f"[openresearch-agent] Quota: {QUOTA_URL}  Scheduler: {SCHEDULER_URL}  Registry: {REGISTRY_URL}")
    print(f"[openresearch-agent] Project: {PROJECT_ID}  Max turns: {MAX_TURNS}\n")

    messages: list[dict] = [
        {"role": "user", "content": (
            "Start your research session. Register if needed, list open platform experiments, "
            "sign up for one, check your quota, then submit your first training job."
        )}
    ]

    for turn in range(MAX_TURNS):
        print(f"\n--- Turn {turn + 1}/{MAX_TURNS} ---")

        response = client.messages.create(
            model="claude-sonnet-4-6",
            max_tokens=4096,
            system=SYSTEM_PROMPT,
            tools=TOOLS,
            messages=messages,
        )

        assistant_content: list[dict] = []
        tool_uses: list[dict] = []

        for block in response.content:
            if block.type == "text":
                print(f"\n[agent] {block.text}")
                assistant_content.append({"type": "text", "text": block.text})
            elif block.type == "tool_use":
                tool_uses.append(block)
                assistant_content.append({
                    "type": "tool_use",
                    "id": block.id,
                    "name": block.name,
                    "input": block.input,
                })

        messages.append({"role": "assistant", "content": assistant_content})

        if not tool_uses:
            print("\n[openresearch-agent] Agent finished (no more tool calls).")
            break

        tool_results: list[dict] = []
        for tu in tool_uses:
            print(f"\n[tool] {tu.name}({json.dumps(tu.input, indent=None)})")
            try:
                result = TOOL_FN[tu.name](tu.input)
            except Exception as e:
                result = {"error": str(e)}
            print(f"[tool] → {json.dumps(result)[:400]}")
            tool_results.append({
                "type": "tool_result",
                "tool_use_id": tu.id,
                "content": json.dumps(result),
            })

        messages.append({"role": "user", "content": tool_results})

        if response.stop_reason == "end_turn" and not tool_uses:
            break

    print(f"\n[openresearch-agent] Session complete after {turn + 1} turns.")


if __name__ == "__main__":
    run_agent()
