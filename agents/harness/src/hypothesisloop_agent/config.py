from __future__ import annotations

import base64
import binascii
import os
from dataclasses import dataclass, field

from hypothesisloop_agent import platform


def required(name: str, what: str = "") -> str:
    value = os.environ.get(name)
    if not value:
        raise SystemExit(f"agent: missing required env var: {name}" + (f", {what}" if what else ""))
    return value


# The roles the platform serves instructions for. A coordinator is no agent of the run: HL_TOKEN is
# the platform experiment's coordinator token, and its agent_id is this name, which is also the
# subject of its own agent.instructions (domain.CoordinatorRecipient).
COORDINATOR = platform.ROLE_COORDINATOR
# The agent of a draft: it plans the experiment the others will later run in.
PLANNER = platform.ROLE_PLANNER
BASELINE = platform.ROLE_BASELINE
RESEARCHER = platform.ROLE_RESEARCHER
_AGENT_ROLES = (RESEARCHER, BASELINE, PLANNER)


def token_scope(token: str) -> tuple[str, str, str]:
    """What a token the platform derived says of its holder: ("agent", platform experiment, agent)
    or ("coordinator", platform experiment, ""). It is hls_<scope>.<mac>, the scope being
    base64url of agent:<pe>/<agent> or coordinator:<pe>. The platform alone verifies it."""
    scope = ""
    if token.startswith(platform.DERIVED_TOKEN_PREFIX) and "." in token:
        part = token[len(platform.DERIVED_TOKEN_PREFIX):].split(".", 1)[0]
        try:
            scope = base64.urlsafe_b64decode(part + "=" * (-len(part) % 4)).decode()
        except (binascii.Error, UnicodeDecodeError):
            scope = ""
    kind, _, rest = scope.partition(":")
    pe, _, agent = rest.partition("/")
    if kind == platform.SCOPE_AGENT and pe and agent:
        return kind, pe, agent
    if kind == platform.SCOPE_COORDINATOR and pe and not agent:
        return kind, pe, ""
    raise SystemExit("agent: HL_TOKEN is neither an agent's token nor a coordinator's. An agent gets its token when "
                     "it is added with `hl platform-experiments agents add`.")


@dataclass
class Config:
    """What the harness is started with is API_URL and HL_TOKEN. The token says which agent of
    which platform experiment this is, or that it is the experiment's coordinator, and the rest is
    read from the platform: its instructions say what it runs as (instructions.py)."""
    # The agent's own `hl` reads API_URL and HL_TOKEN from this same environment.
    api_url: str = field(default_factory=lambda: required(platform.ENV_API_URL))
    token: str = field(default_factory=lambda: required(platform.ENV_TOKEN))

    # From the token. A coordinator has no agent id: agent_id is then COORDINATOR, which names it
    # in the activity log and its working directory and is never sent to the platform as an id.
    agent_id: str = field(init=False)
    platform_experiment_id: str = field(init=False)

    # The role its instructions name, which no edit of its agent type changes. A coordinator's is
    # known from its token.
    role: str = field(init=False, default="")

    def __post_init__(self):
        kind, self.platform_experiment_id, agent_id = token_scope(self.token)
        if kind == platform.SCOPE_COORDINATOR:
            self.agent_id = self.role = COORDINATOR
        else:
            self.agent_id = agent_id

    def run_as(self, role: str) -> None:
        """Takes the role the platform's instructions name. A coordinator's token runs a
        coordinator and an agent's one of the agents' roles, and a role once taken stays."""
        allowed = (COORDINATOR,) if coordinates(self) else _AGENT_ROLES
        if role not in allowed or (self.role and role != self.role):
            raise SystemExit(f"agent: the platform's instructions for {self.agent_id} name the role {role!r}, "
                             f"which it cannot run as (it runs as {self.role or 'one of ' + ', '.join(allowed)})")
        self.role = role


def coordinates(cfg) -> bool:
    return cfg.role == COORDINATOR


def plans(cfg) -> bool:
    return cfg.role == PLANNER


def measures_baseline(cfg) -> bool:
    """An agent of role baseline measures the control before the run starts."""
    return cfg.role == BASELINE


def submits_jobs(cfg) -> bool:
    """A researcher or a baseline agent: whoever has jobs of its own to wait for."""
    return not coordinates(cfg) and not plans(cfg)
