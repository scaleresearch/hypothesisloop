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
                  max_subagents=4, pacing_subagents=2, effort="high", hyperparameters={}, digest="digest-1")
    fields.update(changes)
    return instructions.Instructions(**fields)


@pytest.fixture(autouse=True)
def situation():
    """No test reaches the hl on PATH for the situation: every prompt reads this one."""
    with mock.patch.object(updates, "situation", mock.AsyncMock(return_value=SITUATION)) as read, \
            mock.patch.object(updates, "standing", mock.AsyncMock(return_value=None)):
        yield read


@pytest.fixture(autouse=True)
def instructions_served():
    """Every connection a test opens reads instructions for its role from no platform."""
    with mock.patch.object(instructions, "fetch", lambda client, cfg, commands, workdir: served(cfg)) as fetch:
        yield fetch
