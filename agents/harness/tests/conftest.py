from unittest import mock

import pytest

from hypothesisloop_agent import instructions, updates

# What a stubbed `hl situation` reads, at the end of every prompt of a role in a run.
SITUATION = "Situation:\nrun: 120 minutes left"

# The platform's instructions as tests read them unless one serves its own: fetch as the module has it.
FETCH = instructions.fetch


def served(cfg, **changes) -> instructions.Instructions:
    """Instructions as the platform would serve them for cfg's role."""
    fields = dict(agent_type="generalist", version=1, role=cfg.role, prompt="the system prompt", model="m",
                  max_subagents=4, effort="high", hyperparameters={}, digest="digest-1")
    fields.update(changes)
    return instructions.Instructions(**fields)


@pytest.fixture(autouse=True)
def situation():
    """No test reaches the hl on PATH for the situation: every prompt reads this one."""
    with mock.patch.object(updates, "situation", mock.AsyncMock(return_value=SITUATION)) as read, \
            mock.patch.object(updates, "standing", mock.AsyncMock(return_value=None)):
        yield read


@pytest.fixture(autouse=True)
def model_home(tmp_path, monkeypatch):
    """Each test's model CLI keeps its sessions in a home of its own."""
    monkeypatch.setenv("CLAUDE_CONFIG_DIR", str(tmp_path / "model-home"))


@pytest.fixture(autouse=True)
def instructions_served():
    """Every connection a test opens reads instructions for its role from no platform."""
    async def fetch(client, cfg, commands, workdir):
        return served(cfg)

    with mock.patch.object(instructions, "fetch", fetch) as fetch:
        yield fetch


def _answer(value):
    value = value() if callable(value) else value
    if isinstance(value, Exception):
        raise value
    return value


class FakePlatform:
    """The platform calls of the harness answered with experiment and signup, each a dict, an error
    to raise, or a function returning one, and each session put kept in stored."""

    def __init__(self, experiment=None, signup=None):
        self.experiment = {"status": "running"} if experiment is None else experiment
        self.signup = {} if signup is None else signup
        self.stored = []

    async def get_platform_experiment(self, _id):
        return _answer(self.experiment)

    async def get_signup(self, *_):
        return _answer(self.signup)

    async def get_bytes(self, _path):
        return None

    async def put_bytes(self, path, content):
        self.stored.append(path)


async def connect(setup, resume_id, stop_flag, store=None, initial_prompt=None, on_good_turn=lambda: None) -> bool:
    """One connection of the Claude harness for setup, as its run loop opens it."""
    from hypothesisloop_agent.claude import connection
    from hypothesisloop_agent.claude.running import Running
    from hypothesisloop_agent.claude.store import Store

    return await connection.run(setup, store or Store(setup), Running(setup), resume_id, stop_flag, initial_prompt, on_good_turn)
