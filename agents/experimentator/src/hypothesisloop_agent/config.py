"""Agent configuration — every knob is an env var with a default, no config library, mirroring
how cluster-agent/node-agent are configured (see runtime/k8s/cmd/*/main.go)."""
from __future__ import annotations

import os
from dataclasses import dataclass, field


def _env(name: str, default: str = "") -> str:
    return os.environ.get(name, default)


def _env_float(name: str, default: float) -> float:
    v = os.environ.get(name)
    return float(v) if v else default


@dataclass
class Config:
    # Same URLs used by tests/lib/common.sh and runtime/k8s/cmd/cluster-agent.
    quota_url: str = field(default_factory=lambda: _env("QUOTA_URL", "http://localhost:8081"))
    sched_url: str = field(default_factory=lambda: _env("SCHED_URL", "http://localhost:8082"))
    registry_url: str = field(default_factory=lambda: _env("REGISTRY_URL", "http://localhost:8083"))

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

    # Stop condition. 0 = unlimited: runs until the agent decides it's done or the platform
    # experiment closes and submissions start getting rejected (see core.py's stop_reason).
    max_wall_hours: float = field(default_factory=lambda: _env_float("MAX_WALL_HOURS", 0))

    def validate(self) -> None:
        for name, value in (("AGENT_ID", self.agent_id),
                            ("PLATFORM_EXPERIMENT_ID", self.platform_experiment_id)):
            if not value:
                raise SystemExit(f"agent: missing required env var: {name}")
