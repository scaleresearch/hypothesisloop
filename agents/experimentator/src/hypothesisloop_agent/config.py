from __future__ import annotations

import json
import os
from dataclasses import dataclass, field


def _required(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise SystemExit(f"agent: missing required env var: {name}")
    return value


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


# What a platform experiment's coordinator is started as. It is no flavor of an agent: a
# coordinator is not in the experiment, so it has no AGENT_ID, and HL_TOKEN is the platform
# experiment's coordinator token.
COORDINATOR_FLAVOR = "coordinator"
# What the agent of a draft is started as. It is an agent of the platform experiment like any
# other, with its own AGENT_ID and token, and its briefing is its own: it plans the experiment
# the others will later run in.
PLANNER_FLAVOR = "planner"


def _agent_id() -> str:
    if os.environ.get("AGENT_FLAVOR") != COORDINATOR_FLAVOR:
        return _required("AGENT_ID")
    if os.environ.get("AGENT_ID"):
        raise SystemExit("agent: AGENT_ID is set and AGENT_FLAVOR is coordinator: a coordinator is no agent of its "
                         "platform experiment, and hl would act as that agent")
    return COORDINATOR_FLAVOR


@dataclass
class Config:
    # The agent's own `hl` reads API_URL and HL_TOKEN from this same environment.
    api_url: str = field(default_factory=lambda: _required("API_URL"))

    # Who this agent is and the token that acts as it. Both are handed out when the agent is added
    # to its platform experiment (POST /platform-experiments/{id}/agents): the harness registers
    # nothing and signs up nowhere.
    # A coordinator has none: agent_id is then its flavor, which names it in the activity log
    # and its working directory and is never sent to the platform.
    agent_id: str = field(default_factory=_agent_id)
    token: str = field(default_factory=lambda: _required("HL_TOKEN"))

    # Which platform experiment to compete in: an agent competes in exactly the one it
    # was launched for. Everything about that experiment — objective, metric, job spec, rules —
    # the agent discovers itself from the experiment's `description` (see core.py's prompt).
    platform_experiment_id: str = field(default_factory=lambda: _required("PLATFORM_EXPERIMENT_ID"))

    # Which specialization this agent runs: the coordinator decides it and passes it in, the agent
    # never picks its own. Every flavor competes identically — nothing here is a ranking axis — it
    # only selects which brief is inlined into the prompt (prompts/flavors/<flavor>.md), which
    # holds the specialized approach: what kind of trial to run, what to vary, what "winning"
    # looks like for that specialization.
    # Required, like the model below: both are decided when the agent is added and the platform
    # shows them on its roster, so a default here would run something the roster does not say.
    flavor: str = field(default_factory=lambda: _required("AGENT_FLAVOR"))

    # The model this agent runs on, whichever provider's backend runs it.
    model: str = field(default_factory=lambda: _required("AGENT_MODEL"))

    # Hyperparameters: a small JSON object of sweep knobs (batch_size, risk_tolerance, ...) this
    # one agent is handed on top of its flavor when it is added. A sweep is several agents of the
    # same flavor, each with its own. The agent reads it back via {hyperparameters} in the system
    # prompt and decides for itself what to do with it.
    hyperparameters: dict = field(default_factory=lambda: _env_json_object("AGENT_HYPERPARAMETERS"))

    # Stop condition. 0 = unlimited: runs until the agent decides it's done or the platform
    # experiment closes and submissions start getting rejected (see core.py's stop_reason).
    max_wall_hours: float = field(default_factory=lambda: _env_float("MAX_WALL_HOURS", 0))


def coordinates(cfg) -> bool:
    return cfg.flavor == COORDINATOR_FLAVOR


def plans(cfg) -> bool:
    return cfg.flavor == PLANNER_FLAVOR


def watched_agent_id(cfg) -> str:
    """Whose resources the harness watches: the agent's own, or with "" the whole platform
    experiment's."""
    return "" if coordinates(cfg) else cfg.agent_id
