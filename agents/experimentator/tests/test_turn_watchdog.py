import asyncio
import contextlib
import io
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

from claude_agent_sdk import AssistantMessage, TextBlock

from hypothesisloop_agent.claude import run


class HangingClient:
    """Answers the first query with one assistant message, then never sends another."""

    instances = []

    def __init__(self, options):
        self.disconnected = False
        HangingClient.instances.append(self)

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        self.disconnected = True
        return False

    async def query(self, prompt):
        pass

    async def receive_response(self):
        yield AssistantMessage(content=[TextBlock(text="pulling metrics")], model="m")
        await asyncio.Event().wait()


class TurnWatchdogTest(unittest.IsolatedAsyncioTestCase):
    async def test_silent_cli_ends_session_with_turn_stalled(self):
        setup = SimpleNamespace(system_prompt="", workdir=tempfile.mkdtemp(),
                                cfg=SimpleNamespace(agent_id="agent-a", api_url="http://x", platform_experiment_id="pe-1"))
        with mock.patch.object(run, "ClaudeSDKClient", HangingClient), \
                mock.patch.object(run, "_TURN_SILENCE_TIMEOUT_S", 0.05), \
                contextlib.redirect_stdout(io.StringIO()) as out:
            with self.assertRaises(run.TurnStalled):
                await asyncio.wait_for(
                    run._run_one_session(setup, "m", None, 0.0, [False]), timeout=5)
        self.assertTrue(HangingClient.instances[-1].disconnected)
        self.assertIn("agent-a say   pulling metrics", out.getvalue())


if __name__ == "__main__":
    unittest.main()
