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
from test_require_joined import AGENT_TOKEN


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
                           client=SimpleNamespace(get_platform_experiment=lambda _id: {"status": "running"}, get_signup=lambda *_: {}),
                           cfg=SimpleNamespace(agent_id="agent-a", flavor="generalist", api_url="http://x", token=AGENT_TOKEN, platform_experiment_id="pe-1",
                                               max_wall_hours=0))


# A machine stopped while its agent's run goes on is told exactly that, ahead of what to do with its turn.
LAST = ("You are being stopped while your run on the platform goes on: the platform ended nothing of yours. "
        "Only this machine is being stopped, and the platform does not say why. " + run._SHUTDOWN_PROMPT)


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
        self.assertEqual(client.queries, ["Begin.", LAST])
        self.assertTrue(client.disconnected)

    async def test_the_last_turn_is_cut_off_at_its_deadline(self):
        client = await self.stop_mid_turn(hangs=True)
        self.assertEqual(client.queries, ["Begin.", LAST])
        self.assertTrue(client.disconnected)

    # A researcher that had just finished was interrupted in its closing message and given a
    # turn to file summaries the platform had already checked it did not owe.
    async def test_a_stop_after_the_agent_finished_lets_its_turn_end_and_gives_no_other(self):
        stop_flag = [False]
        asked = asyncio.Event()

        def signup(*_):
            asked.set()
            return {"ended": "finished"}

        class FinishingClient(StoppableClient):
            async def receive_response(self):
                yield AssistantMessage(content=[TextBlock(text="finished, closing words")], model="m")
                await asked.wait()
                yield _result()

        setup = _setup()
        setup.client.get_signup = signup
        with mock.patch.object(run, "ClaudeSDKClient", FinishingClient), \
                mock.patch.object(run.asyncio, "sleep", mock.AsyncMock(side_effect=lambda _: stop_flag.__setitem__(0, True))), \
                contextlib.redirect_stdout(io.StringIO()):
            await asyncio.wait_for(run._run_one_session(setup, "m", None, 0.0, stop_flag), timeout=5)
        client = HangingClient.instances[-1]
        self.assertFalse(client.interrupted.is_set())
        self.assertEqual(client.queries, ["Begin."])
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
        with mock.patch.object(run, "ClaudeSDKClient", IdleClient), \
                mock.patch.object(run.updates, "next_batch", stopped_while_waiting), \
                mock.patch.object(run.updates, "drain", mock.AsyncMock(return_value=finished_since)), \
                contextlib.redirect_stdout(io.StringIO()):
            await asyncio.wait_for(run._run_one_session(setup, "m", None, time.time(), stop_flag), timeout=5)
        return HangingClient.instances[-1].queries

    async def test_a_stop_between_turns_gives_one_last_turn(self):
        self.assertEqual(await self.stop_while_idle([], []), ["Begin.", LAST])

    async def test_the_last_turn_opens_with_what_finished_since_the_last_delivery(self):
        done = {"kind": "experiment.status", "subject": "exp-1", "value": "COMPLETED", "detail": "acc best 0.9 last 0.9"}
        comment = {"kind": "comment.new", "subject": "hyp-1", "value": "human"}
        self.assertEqual(await self.stop_while_idle([comment], [done]), [
            "Begin.", LAST + "\n\nPlatform updates:\ncomment.new hyp-1 human\n"
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


class ReconnectTest(unittest.IsolatedAsyncioTestCase):
    async def test_a_raised_session_is_followed_by_what_it_left_on_disk_and_a_good_turn_resets_the_backoff(self):
        setup = _setup()
        stop_flag = [False]
        resumed, sleeps = [], []

        async def session(setup, model, resume_id, started_at, stop_flag_, initial_prompt, on_good_turn):
            resumed.append(resume_id)
            if len(resumed) == 1:
                run._save_resume_id(setup, "s-2")
                raise run.ProcessError("the CLI exited", exit_code=1)
            if len(resumed) == 2:
                raise Exception("the SDK's own word for a CLI that died mid-stream")
            on_good_turn()
            if len(resumed) == 4:
                stop_flag[0] = True
            return None, False

        async def sleep(seconds):
            sleeps.append(seconds)

        with mock.patch.object(run, "_run_one_session", session), \
                mock.patch.object(run.core, "install_signal_handler", lambda: stop_flag), \
                mock.patch.object(run.asyncio, "sleep", sleep), \
                self.assertLogs("llm_agent"):
            await asyncio.wait_for(run._run(setup, "m"), timeout=5)
        self.assertEqual(resumed, [None, "s-2", "s-2", "s-2"])
        self.assertEqual(sleeps, [30, 60, 30])

    # A bug in the harness once read as a flaky connection and was retried for the whole run.
    async def test_an_error_that_is_no_lost_connection_ends_the_process(self):
        async def session(*_):
            raise KeyError("session_id")

        with mock.patch.object(run, "_run_one_session", session), \
                mock.patch.object(run.core, "install_signal_handler", lambda: [False]), \
                self.assertRaises(KeyError):
            await asyncio.wait_for(run._run(_setup(), "m"), timeout=5)

    def test_a_handoff_opens_the_next_session_only_while_no_session_id_is_saved(self):
        setup = _setup()
        with open(run._handoff_file(setup), "w") as f:
            f.write("state so far")
        self.assertEqual(run._restart_state(setup),
                         (None, "Continue this experiment from the following handoff:\n\nstate so far"))
        run._save_resume_id(setup, "s-3")
        self.assertEqual(run._restart_state(setup), ("s-3", None))


if __name__ == "__main__":
    unittest.main()
