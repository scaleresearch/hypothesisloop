"""Agent configuration — every knob is an env var with a default, no config library, mirroring
how cluster-agent/node-agent are configured (see runtime/k8s/cmd/*/main.go)."""
from __future__ import annotations

import json
import os
from dataclasses import dataclass, field



def _env(name: str, default: str = "") -> str:
    return os.environ.get(name, default)


def _env_float(name: str, default: float) -> float:
    v = os.environ.get(name)
    return float(v) if v else default


def _env_json_object(name: str, default: str = "{}") -> dict:
    """Parses an env var as a small JSON object of hyperparameters. Malformed or non-object JSON
    fails loudly at startup rather than silently handing the agent an empty dict — a sweep that
    got its hyperparameters wrong should not run believing it got the defaults."""
    raw = os.environ.get(name, default) or default
    try:
        value = json.loads(raw)
    except json.JSONDecodeError as e:
        raise SystemExit(f"agent: {name} must be a JSON object, got {raw!r}: {e}")
    if not isinstance(value, dict):
        raise SystemExit(f"agent: {name} must be a JSON object, got {raw!r}")
    return value


@dataclass
class Config:
    # Same URLs used by tests/lib/common.sh and runtime/k8s/cmd/cluster-agent.
    api_url: str = field(default_factory=lambda: _env("API_URL", "http://localhost:8081"))

    agent_id: str = field(default_factory=lambda: _env("AGENT_ID", ""))

    # Which platform experiment to compete in. Required: an agent competes in exactly the one it
    # was launched for. Everything about that experiment — objective, metric, job spec, rules —
    # the agent discovers itself from the experiment's `description` (see core.py's prompt).
    platform_experiment_id: str = field(default_factory=lambda: _env("PLATFORM_EXPERIMENT_ID", ""))

    # THE shared code repo, pre-created by the operator — every agent clones the same one and works
    # on its own branch (agent-{agent_id}). Must be a real remote pods can clone: code_ref =
    # "{code_repo_url}@<sha>" is the commit a job pod checks out to run. Auth via GIT_TOKEN (see the
    # Dockerfile's git credential helper); a local git:// remote needs no token.
    code_repo_url: str = field(default_factory=lambda: _env(
        "CODE_REPO_URL", "https://github.com/hypothesisloop-agents/experiments"))
    git_token: str = field(default_factory=lambda: _env("GIT_TOKEN"))

    # Which specialization this agent runs: the coordinator decides it and passes it in, the agent
    # never picks its own. Every flavor competes identically — nothing here is a ranking axis — it
    # only selects which brief the agent reads at $FLAVOR_BRIEFS/{flavor}.md
    # (prompts/flavors/<flavor>.md), which holds the specialized approach: what kind of trial to
    # run, what to vary, what "winning" looks like for that specialization.
    flavor: str = field(default_factory=lambda: _env("AGENT_FLAVOR", "generalist"))

    # Hyperparameters: a small JSON object of sweep knobs (batch_size, risk_tolerance, ...) the
    # coordinator hands this one agent at launch, on top of its flavor — a sweep is a coordinator
    # loop that starts N agents of the same flavor, each with its own AGENT_HYPERPARAMETERS,
    # exactly like XManager's `experiment.add` loop. Pure env var: the control plane never sees or
    # stores it, so there is no second record of what an agent was told to try that could drift
    # from what it actually ran. The agent reads it back via {hyperparameters} in the system
    # prompt and decides for itself what to do with it.
    hyperparameters: dict = field(default_factory=lambda: _env_json_object("AGENT_HYPERPARAMETERS"))

    # Stop condition. 0 = unlimited: runs until the agent decides it's done or the platform
    # experiment closes and submissions start getting rejected (see core.py's stop_reason).
    max_wall_hours: float = field(default_factory=lambda: _env_float("MAX_WALL_HOURS", 0))

    def validate(self) -> None:
        for name, value in (("AGENT_ID", self.agent_id),
                            ("PLATFORM_EXPERIMENT_ID", self.platform_experiment_id)):
            if not value:
                raise SystemExit(f"agent: missing required env var: {name}")
