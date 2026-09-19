import asyncio
import contextlib
import io
import tempfile
import time
import unittest
from types import SimpleNamespace
from unittest import mock

from claude_agent_sdk import AssistantMessage, ResultMessage, TextBlock

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


def _setup():
    return SimpleNamespace(system_prompt="", workdir=tempfile.mkdtemp(),
                           cfg=SimpleNamespace(agent_id="agent-a", api_url="http://x", platform_experiment_id="pe-1",
                                               max_wall_hours=0))


def _result():
    return ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=False, num_turns=1, session_id="s-1")


class StoppableClient(HangingClient):
    """Works on a turn until interrupted. Its turn after that ends at once, or never when hangs."""

    hangs = False

    def __init__(self, options):
        super().__init__(options)
        self.queries = []
        self.interrupted = asyncio.Event()

    async def query(self, prompt):
        self.queries.append(prompt)

    async def interrupt(self):
        self.interrupted.set()

    async def receive_response(self):
        if len(self.queries) == 1:
            yield AssistantMessage(content=[TextBlock(text="submitting the next job")], model="m")
            await self.interrupted.wait()
        elif self.hangs:
            await asyncio.Event().wait()
        yield _result()


class StopTest(unittest.IsolatedAsyncioTestCase):
    async def stop_mid_turn(self, hangs: bool) -> StoppableClient:
        stop_flag = [False]
        with mock.patch.object(run, "ClaudeSDKClient", StoppableClient), \
                mock.patch.object(run.updates, "drain", mock.AsyncMock(return_value=[])), \
                mock.patch.object(StoppableClient, "hangs", hangs), \
                mock.patch.object(run, "_SHUTDOWN_TURN_S", 0.05), \
                mock.patch.object(run.asyncio, "sleep", mock.AsyncMock(side_effect=lambda _: stop_flag.__setitem__(0, True))), \
                contextlib.redirect_stdout(io.StringIO()):
            await asyncio.wait_for(run._run_one_session(_setup(), "m", None, 0.0, stop_flag), timeout=5)
        return HangingClient.instances[-1]

    async def test_a_stop_interrupts_the_turn_and_gives_one_last_turn(self):
        client = await self.stop_mid_turn(hangs=False)
        self.assertTrue(client.interrupted.is_set())
        self.assertEqual(client.queries, ["Begin.", run._SHUTDOWN_PROMPT])
        self.assertTrue(client.disconnected)

    async def test_the_last_turn_is_cut_off_at_its_deadline(self):
        client = await self.stop_mid_turn(hangs=True)
        self.assertEqual(client.queries, ["Begin.", run._SHUTDOWN_PROMPT])
        self.assertTrue(client.disconnected)


class IdleClient(StoppableClient):
    """Ends every turn at once."""

    async def receive_response(self):
        yield _result()


class IdleStopTest(unittest.IsolatedAsyncioTestCase):
    async def stop_while_idle(self, waiting: list[dict], finished_since: list[dict]) -> list[str]:
        stop_flag = [False]

        async def stopped_while_waiting(env, stopped, idle_s):
            stop_flag[0] = True
            return waiting

        setup = _setup()
        setup.client = SimpleNamespace(get_platform_experiment=lambda _id: {"status": "running"})
        with mock.patch.object(run, "ClaudeSDKClient", IdleClient), \
                mock.patch.object(run.updates, "next_batch", stopped_while_waiting), \
                mock.patch.object(run.updates, "drain", mock.AsyncMock(return_value=finished_since)), \
                contextlib.redirect_stdout(io.StringIO()):
            await asyncio.wait_for(run._run_one_session(setup, "m", None, time.time(), stop_flag), timeout=5)
        return HangingClient.instances[-1].queries

    async def test_a_stop_between_turns_gives_one_last_turn(self):
        self.assertEqual(await self.stop_while_idle([], []), ["Begin.", run._SHUTDOWN_PROMPT])

    async def test_the_last_turn_opens_with_what_finished_since_the_last_delivery(self):
        done = {"kind": "experiment.status", "subject": "exp-1", "value": "COMPLETED", "detail": "acc best 0.9 last 0.9"}
        comment = {"kind": "comment.new", "subject": "hyp-1", "value": "human"}
        self.assertEqual(await self.stop_while_idle([comment], [done]), [
            "Begin.", run._SHUTDOWN_PROMPT + "\n\nPlatform updates:\ncomment.new hyp-1 human\n"
                                            "experiment.status exp-1 COMPLETED acc best 0.9 last 0.9"])


class TurnWatchdogTest(unittest.IsolatedAsyncioTestCase):
    async def test_silent_cli_ends_session_with_turn_stalled(self):
        setup = _setup()
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
