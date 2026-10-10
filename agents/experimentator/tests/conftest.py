from unittest import mock

import pytest

from hypothesisloop_agent import updates

# What a stubbed `hl situation` reads, at the end of every prompt of a role in a run.
SITUATION = "Situation:\nrun: 120 minutes left"


@pytest.fixture(autouse=True)
def situation():
    """No test reaches the hl on PATH for the situation: every prompt reads this one."""
    with mock.patch.object(updates, "situation", mock.AsyncMock(return_value=SITUATION)) as read, \
            mock.patch.object(updates, "standing", mock.AsyncMock(return_value=None)):
        yield read
