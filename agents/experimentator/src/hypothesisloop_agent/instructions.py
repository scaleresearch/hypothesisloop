"""The instructions an agent runs with: the prompt of the current version of its agent type and its
settings, as the platform serves them. They are read at the start of each connection, and again at
the next turn boundary once agent.instructions says they changed: the type has a new current
version, or the platform's rules do."""
from __future__ import annotations

import dataclasses
import json
import os
from dataclasses import dataclass

from hypothesisloop_agent import api_client, config, prompt

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
    pacing_subagents: int
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
# (domain.AgentTypeSettings), each one the harness has decided on: a field the platform adds or
# drops ends the harness until it is decided here. rules_version is not kept: the digest holds it,
# and the digest is what the harness compares. pacing_subagents is kept and read by nothing: the
# model proxy paces the model allowance.
_ANSWER = frozenset({"agent_type", "agent_type_version", "rules_version", "role", "prompt", "placeholders", "settings", "digest"})
_SETTINGS = frozenset({"model", "max_subagents", "pacing_subagents", "effort", "hyperparameters"})


def _schema(cfg: config.Config, what: str, given, expected: frozenset) -> None:
    if not isinstance(given, dict):
        raise SystemExit(f"agent: {what} for {cfg.agent_id} is no JSON object")
    unknown, missing = sorted(given.keys() - expected), sorted(expected - given.keys())
    if unknown or missing:
        raise SystemExit(f"agent: {what} for {cfg.agent_id} hold fields the harness has not decided on {unknown} "
                         f"and lack fields it runs with {missing}")


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
    _schema(cfg, "the platform's instructions", answer, _ANSWER)
    _schema(cfg, "the platform's instruction settings", answer["settings"], _SETTINGS)
    try:
        settings = answer["settings"]
        cfg.run_as(answer["role"])
        instructions = Instructions(
            agent_type=answer["agent_type"], version=int(answer["agent_type_version"]), role=answer["role"],
            prompt=prompt.fill(answer["prompt"], answer["placeholders"], {prompt.HL_COMMANDS: commands, prompt.WORKDIR: workdir}),
            model=settings["model"], max_subagents=int(settings["max_subagents"]),
            pacing_subagents=int(settings["pacing_subagents"]), effort=settings["effort"],
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
    """What save() kept, None for nothing kept. A file kept with other fields than Instructions
    has, by a harness of another version, ends the harness: what it holds has to be decided on."""
    try:
        with open(os.path.join(statedir, FILE)) as f:
            kept = json.load(f)
    except FileNotFoundError:
        return None
    fields = frozenset(field.name for field in dataclasses.fields(Instructions))
    if not isinstance(kept, dict) or kept.keys() != fields:
        raise SystemExit(f"agent: {FILE} of the state directory holds {sorted(kept) if isinstance(kept, dict) else kept!r}, "
                         f"and the harness keeps {sorted(fields)}")
    return Instructions(**kept)
