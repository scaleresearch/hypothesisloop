"""Agent configuration — every knob comes from an environment variable so the same script runs
unmodified locally, inside a container, or under systemd. No framework, no config library:
this is deliberately just os.environ reads with defaults, mirroring how cluster-agent/node-agent
(the Go side of this repo) are configured — see cluster/cmd/*/main.go.
"""
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
    # Same three control-plane base URLs every test scenario/cluster-agent uses — see
    # tests/lib/common.sh's QUOTA_URL/SCHED_URL/REGISTRY_URL and cluster/cmd/cluster-agent.
    quota_url: str = field(default_factory=lambda: _env("QUOTA_URL", "http://localhost:8081"))
    sched_url: str = field(default_factory=lambda: _env("SCHED_URL", "http://localhost:8082"))
    registry_url: str = field(default_factory=lambda: _env("REGISTRY_URL", "http://localhost:8083"))

    agent_id: str = field(default_factory=lambda: _env("AGENT_ID", ""))

    # Which platform experiment to compete in — the only workload-specific input this process
    # takes. Everything else (job spec, image, metric to optimize, rules) the agent reads for
    # itself from GET /platform-experiments/{id} (its `description` field) at runtime; nothing
    # is injected locally. If unset, the agent discovers open platform experiments itself via
    # GET /platform-experiments?status=open and picks one matching AGENT_GOAL.
    platform_experiment_id: str = field(default_factory=lambda: _env("PLATFORM_EXPERIMENT_ID", ""))

    # Stop condition — an agent left with this at 0 runs until it decides it's done or the
    # platform experiment closes underneath it (job submissions will then start getting rejected
    # with 4xx, itself becoming a stop condition — see llm_agent.py's main loop).
    max_wall_hours: float = field(default_factory=lambda: _env_float("MAX_WALL_HOURS", 0))  # 0 = unlimited

    def validate(self) -> None:
        if not self.agent_id:
            raise SystemExit("agent: missing required env var: AGENT_ID")
