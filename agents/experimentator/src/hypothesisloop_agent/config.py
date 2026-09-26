from __future__ import annotations

import base64
import binascii
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


# What a platform experiment's coordinator runs as. It is no flavor of an agent: a coordinator is
# not in the experiment, and HL_TOKEN is the platform experiment's coordinator token.
COORDINATOR_FLAVOR = "coordinator"
# What the agent of a draft is started as. It is an agent of the platform experiment like any
# other, with its own token, and its briefing is its own: it plans the experiment
# the others will later run in.
PLANNER_FLAVOR = "planner"


def token_scope(token: str) -> tuple[str, str, str]:
    """What a token the platform derived says of its holder: ("agent", platform experiment, agent)
    or ("coordinator", platform experiment, ""). It is hls_<scope>.<mac>, the scope being
    base64url of agent:<pe>/<agent> or coordinator:<pe>. The platform alone verifies it."""
    scope = ""
    if token.startswith("hls_") and "." in token:
        part = token[len("hls_"):].split(".", 1)[0]
        try:
            scope = base64.urlsafe_b64decode(part + "=" * (-len(part) % 4)).decode()
        except (binascii.Error, UnicodeDecodeError):
            scope = ""
    kind, _, rest = scope.partition(":")
    pe, _, agent = rest.partition("/")
    if kind == "agent" and pe and agent:
        return kind, pe, agent
    if kind == "coordinator" and pe and not agent:
        return kind, pe, ""
    raise SystemExit("agent: HL_TOKEN is neither an agent's token nor a coordinator's. An agent gets its token when "
                     "it is added with `hl platform-experiments agents add`.")


@dataclass
class Config:
    """What the harness is started with is API_URL and HL_TOKEN. The token says which agent of
    which platform experiment this is, or that it is the experiment's coordinator, and the rest is
    read from the platform at startup (core.prepare)."""
    # The agent's own `hl` reads API_URL and HL_TOKEN from this same environment.
    api_url: str = field(default_factory=lambda: _required("API_URL"))
    token: str = field(default_factory=lambda: _required("HL_TOKEN"))

    # Stop condition. 0 = unlimited: runs until the agent decides it's done or the platform
    # experiment closes and submissions start getting rejected (see core.py's stop_reason).
    max_wall_hours: float = field(default_factory=lambda: _env_float("MAX_WALL_HOURS", 0))

    # From the token. A coordinator has no agent id: agent_id is then its flavor, which names it
    # in the activity log and its working directory and is never sent to the platform.
    agent_id: str = field(init=False)
    platform_experiment_id: str = field(init=False)

    # From the agent's signup, or for a coordinator from its platform experiment. flavor selects
    # the brief inlined into the prompt (prompts/flavors/<flavor>.md), model is what it runs on,
    # and hyperparameters is a small JSON object of sweep knobs the agent reads back in its prompt.
    flavor: str = field(init=False, default="")
    model: str = field(init=False, default="")
    hyperparameters: dict = field(init=False, default_factory=dict)

    def __post_init__(self):
        kind, self.platform_experiment_id, agent_id = token_scope(self.token)
        if kind == "coordinator":
            self.agent_id = self.flavor = COORDINATOR_FLAVOR
        else:
            self.agent_id = agent_id

    def run_as(self, flavor, model, hyperparameters=None) -> None:
        """Takes what the platform says this one runs as. Neither flavor nor model has a default:
        the roster shows both, and a default here would run something the roster does not say."""
        if not flavor or not model:
            raise SystemExit(f"agent: the platform names no flavor or no model for {self.agent_id} "
                             f"(flavor {flavor!r}, model {model!r})")
        self.flavor, self.model, self.hyperparameters = flavor, model, hyperparameters or {}


def coordinates(cfg) -> bool:
    return cfg.flavor == COORDINATOR_FLAVOR


def plans(cfg) -> bool:
    return cfg.flavor == PLANNER_FLAVOR


def watched_agent_id(cfg) -> str:
    """Whose resources the harness watches: the agent's own, or with "" the whole platform
    experiment's."""
    return "" if coordinates(cfg) else cfg.agent_id
