"""The instructions an agent runs with: the prompt of the current version of its agent type and its
settings, as the platform serves them. They are read at the start of each connection, and again at
the next turn boundary once agent.instructions says they changed: the type has a new current
version, or the platform's rules do."""
from __future__ import annotations

import dataclasses
import json
import logging
import os
from dataclasses import dataclass

from hypothesisloop_agent import api_client, config, progress, prompt

log = logging.getLogger("llm_agent")

# The event that says what the agent is served changed. Its subject is the agent, or
# config.COORDINATOR for the coordinator's own.
EVENT = "agent.instructions"
# Where the instructions the session in the state directory runs with are kept, so that a session
# resumed on another machine is compared with what it was started with.
FILE = ".instructions.json"
# The effort only a session started with runs at.
MAX_EFFORT = "max"


@dataclass(frozen=True)
class Instructions:
    agent_type: str
    version: int
    role: str
    # The served prompt with the harness's placeholders filled.
    prompt: str
    model: str
    max_subagents: int
    effort: str
    hyperparameters: dict
    # The platform's digest of the served prompt and the settings.
    digest: str

    def needs_new_session(self, other: Instructions) -> bool:
        """Whether running other takes a new session: a session keeps its model, whose signed
        thinking another model refuses. The subagent limits and the effort change in the session,
        except to and from max: the CLI's effortLevel setting, which a session is changed with,
        holds low to xhigh and drops anything else without a word (Claude Code 2.1.294)."""
        max_changes = self.effort != other.effort and MAX_EFFORT in (self.effort, other.effort)
        return self.model != other.model or max_changes

    def needs_new_prompt(self, other: Instructions) -> bool:
        """Whether running other takes the session restarted with another system prompt."""
        return self.prompt != other.prompt


# The fields of the platform's answer (quota.Instructions) and of its settings
# (domain.AgentTypeSettings) the harness knows. One missing is an incompatibility and ends the
# harness; one it does not know is a feature it does not have yet, said once and passed over, so
# that a control plane deployed before the harness image stops no agent. rules_version is not
# kept: the digest holds it, and the digest is what the harness compares. pacing_subagents is not
# kept: the model proxy paces the model allowance.
_ANSWER = frozenset({"agent_type", "agent_type_version", "rules_version", "role", "prompt", "placeholders", "settings", "digest"})
_SETTINGS = frozenset({"model", "max_subagents", "pacing_subagents", "effort", "hyperparameters"})
_said_unknown: set[tuple[str, tuple[str, ...]]] = set()


def _known(what: str, given, expected: frozenset) -> dict:
    """given with only the fields of expected, or an exit when it lacks one of them. Fields it
    holds beyond them are said once, on stderr and as the agent's activity."""
    if not isinstance(given, dict):
        raise SystemExit(f"agent: {what} is no JSON object")
    if missing := sorted(expected - given.keys()):
        raise SystemExit(f"agent: {what} lack the fields {', '.join(missing)}, which this harness runs with")
    unknown = tuple(sorted(given.keys() - expected))
    if unknown and (what, unknown) not in _said_unknown:
        _said_unknown.add((what, unknown))
        log.warning("%s hold fields this harness does not know, passed over: %s", what, ", ".join(unknown))
        progress.announce(f"{what} hold fields this harness does not know: {', '.join(unknown)}")
    return {key: given[key] for key in expected}


def path(cfg: config.Config) -> str:
    pe = cfg.platform_experiment_id
    if config.coordinates(cfg):
        return f"/platform-experiments/{pe}/coordinator/instructions"
    return f"/platform-experiments/{pe}/agents/{cfg.agent_id}/instructions"


async def read(client: api_client.PlatformClient, cfg: config.Config) -> dict:
    """The instructions as the platform answers them. An agent the platform has none for ends the
    harness: it has nothing to run with. Whether the token is accepted is checked once, at the
    start (core.prepare), and later by core.run_over."""
    try:
        return await client.get_json(path(cfg))
    except api_client.APIError as e:
        if e.status != 404:
            raise
        raise SystemExit(f"agent: the platform serves {cfg.agent_id} no instructions (HTTP {e.status}): {e.body[:300]}") from None


def complete(answer: dict, cfg: config.Config, commands: str, workdir: str) -> Instructions:
    """answer, as read(), with the prompt filled for this machine. Anything the harness cannot run
    with ends it."""
    answer = _known("the platform's instructions", answer, _ANSWER)
    settings = _known("the platform's instruction settings", answer["settings"], _SETTINGS)
    try:
        cfg.run_as(answer["role"])
        instructions = Instructions(
            agent_type=answer["agent_type"], version=int(answer["agent_type_version"]), role=answer["role"],
            prompt=prompt.fill(answer["prompt"], answer["placeholders"], {prompt.HL_COMMANDS: commands, prompt.WORKDIR: workdir}),
            model=settings["model"], max_subagents=int(settings["max_subagents"]),
            effort=settings["effort"],
            hyperparameters=settings["hyperparameters"] or {}, digest=answer["digest"])
    except (KeyError, TypeError, ValueError) as e:
        raise SystemExit(f"agent: the platform's instructions for {cfg.agent_id} are not what the harness runs with: {e!r}") from None
    if not instructions.model:
        raise SystemExit(f"agent: the platform's instructions for {cfg.agent_id} name no model")
    return instructions


async def fetch(client: api_client.PlatformClient, cfg: config.Config, commands: str, workdir: str) -> Instructions:
    return complete(await read(client, cfg), cfg, commands, workdir)


def addressed(event: dict, cfg: config.Config) -> bool:
    """Whether event says this agent's own instructions changed."""
    return event.get("kind") == EVENT and event.get("subject") == cfg.agent_id


def save(statedir: str, instructions: Instructions) -> None:
    with open(os.path.join(statedir, FILE), "w") as f:
        json.dump(dataclasses.asdict(instructions), f)


def load(statedir: str) -> Instructions | None:
    """What save() kept, None for nothing kept, read by the rule the platform's answer is read by:
    a field it lacks ends the harness, and one it holds beyond them, as one kept by a harness of
    another version, is said once and passed over."""
    try:
        with open(os.path.join(statedir, FILE)) as f:
            kept = json.load(f)
    except FileNotFoundError:
        return None
    return Instructions(**_known(f"the instructions kept in {FILE}", kept, frozenset(field.name for field in dataclasses.fields(Instructions))))
