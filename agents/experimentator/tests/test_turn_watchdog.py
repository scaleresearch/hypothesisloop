import asyncio
import contextlib
import io
import json
import os
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

from claude_agent_sdk import AssistantMessage, ResultMessage, StreamEvent, SystemMessage, TextBlock

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

    async def receive_messages(self):
        await asyncio.Event().wait()
        yield


def _setup():
    return SimpleNamespace(system_prompt="", workdir=tempfile.mkdtemp(), statedir=tempfile.mkdtemp(),
                           client=SimpleNamespace(get_platform_experiment=lambda _id: {"status": "running"}, get_signup=lambda *_: {}),
                           cfg=SimpleNamespace(agent_id="agent-a", flavor="generalist", role="competitor", api_url="http://x", token=AGENT_TOKEN, platform_experiment_id="pe-1"))


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
            await asyncio.wait_for(run._run_one_session(_setup(), "m", None, stop_flag), timeout=5)
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
    async def stop_after_finishing(self, client_class, sleep) -> StoppableClient:
        setup = _setup()
        setup.client.get_signup = lambda *_: {"ended": "finished"}
        with mock.patch.object(run, "ClaudeSDKClient", client_class), \
                mock.patch.object(run.asyncio, "sleep", sleep), \
                contextlib.redirect_stdout(io.StringIO()):
            await asyncio.wait_for(run._run_one_session(setup, "m", None, self.stop_flag), timeout=5)
        return HangingClient.instances[-1]

    async def test_a_stop_after_the_agent_finished_lets_its_turn_end_and_gives_no_other(self):
        self.stop_flag = [False]
        given_time = asyncio.Event()

        async def sleep(seconds):
            if seconds == run._SHUTDOWN_TURN_S:
                given_time.set()
                await asyncio.Event().wait()
            self.stop_flag[0] = True

        class FinishingClient(StoppableClient):
            async def receive_response(self):
                yield AssistantMessage(content=[TextBlock(text="finished, closing words")], model="m")
                await given_time.wait()
                yield _result()

        client = await self.stop_after_finishing(FinishingClient, sleep)
        self.assertFalse(client.interrupted.is_set())
        self.assertEqual(client.queries, ["Begin."])
        self.assertTrue(client.disconnected)

    async def test_a_finished_agent_that_works_on_is_interrupted_at_the_deadline_and_gets_no_other_turn(self):
        self.stop_flag = [False]
        client = await self.stop_after_finishing(StoppableClient, mock.AsyncMock(side_effect=lambda _: self.stop_flag.__setitem__(0, True)))
        self.assertTrue(client.interrupted.is_set())
        self.assertEqual(client.queries, ["Begin."])


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
                mock.patch.object(run.updates, "jobs_in_flight", mock.AsyncMock(return_value=1)), \
                mock.patch.object(run.updates, "drain", mock.AsyncMock(return_value=finished_since)), \
                contextlib.redirect_stdout(io.StringIO()):
            await asyncio.wait_for(run._run_one_session(setup, "m", None, stop_flag), timeout=5)
        return HangingClient.instances[-1].queries

    async def test_a_stop_between_turns_gives_one_last_turn(self):
        self.assertEqual(await self.stop_while_idle([], []), ["Begin.", LAST])

    # A run that reached its end with no signal yet ended the process at the turn boundary, so
    # a coordinator never wrote the summary its briefing promises a turn for. A token the
    # platform refuses has nothing left to file with.
    async def run_ends_while_idle(self, flavor: str, signup: dict | Exception, experiment: dict) -> list[str]:
        async def nothing(env, stopped, idle_s):
            return []

        def get_signup(*_):
            if isinstance(signup, Exception):
                raise signup
            return signup

        setup = _setup()
        setup.cfg.flavor = flavor
        setup.client = SimpleNamespace(get_platform_experiment=lambda _id: experiment, get_signup=get_signup)
        with mock.patch.object(run, "ClaudeSDKClient", IdleClient), \
                mock.patch.object(run.updates, "next_batch", nothing), \
                mock.patch.object(run.updates, "drain", mock.AsyncMock(return_value=[])), \
                contextlib.redirect_stdout(io.StringIO()):
            await asyncio.wait_for(run._run_one_session(setup, "m", None, [False]), timeout=5)
        return HangingClient.instances[-1].queries

    async def test_a_run_the_platform_ended_gets_its_last_turn_with_no_signal(self):
        closed = {"status": "closed", "closed_at": "2026-01-01T02:48:05Z", "close_reason": "end_time_reached"}
        self.assertEqual(await self.run_ends_while_idle("coordinator", {}, closed), [
            "Begin.", "You are being stopped because the platform experiment reached its end time and the platform closed it at "
                      "2026-01-01T02:48:05Z. " + run._COORDINATOR_SHUTDOWN_PROMPT])
        self.assertEqual(await self.run_ends_while_idle("generalist", {"ended": "cut"}, {"status": "running"}), [
            "Begin.", "You are being stopped because you were cut from the platform experiment at a stage boundary. " + run._SHUTDOWN_PROMPT])

    # Run 6: the close arrived as an update, was handed over as a turn of its own and cut by the
    # stop four seconds later, and only then did the last turn start. A batch that comes with the
    # end of the run opens the last turn, with the batch in it: not handed over, it is read again.
    async def test_a_batch_that_comes_with_the_close_opens_the_last_turn(self):
        closed_event = {"kind": "platform_experiment.status", "subject": "pe-1", "value": "closed"}
        experiment = {"status": "running"}

        async def the_close(env, stopped, idle_s):
            experiment.update(status="closed", closed_at="2026-01-01T02:48:05Z", close_reason="end_time_reached")
            return [closed_event]

        setup = _setup()
        setup.cfg.flavor = "coordinator"
        setup.client = SimpleNamespace(get_platform_experiment=lambda _id: experiment, get_signup=lambda *_: {})
        with mock.patch.object(run, "ClaudeSDKClient", IdleClient), \
                mock.patch.object(run.updates, "next_batch", the_close), \
                mock.patch.object(run.updates, "drain", mock.AsyncMock(return_value=[closed_event])), \
                contextlib.redirect_stdout(io.StringIO()):
            await asyncio.wait_for(run._run_one_session(setup, "m", None, [False]), timeout=5)
        self.assertEqual(HangingClient.instances[-1].queries, [
            "Begin.", "You are being stopped because the platform experiment reached its end time and the platform closed it at "
                      "2026-01-01T02:48:05Z. " + run._COORDINATOR_SHUTDOWN_PROMPT + "\n\nPlatform updates:\n"
                      "platform_experiment.status pe-1 closed"])

    async def test_a_refused_token_gets_no_last_turn(self):
        refused = run.core.api_client.APIError("GET", "http://api/x", 401, "unauthenticated")
        self.assertEqual(await self.run_ends_while_idle("generalist", refused, {"status": "running"}), ["Begin."])

    async def test_the_last_turn_opens_with_what_finished_since_the_last_delivery(self):
        done = {"kind": "experiment.status", "subject": "exp-1", "value": "COMPLETED", "detail": "acc 0.9"}
        comment = {"kind": "comment.new", "subject": "hyp-1", "value": "human"}
        self.assertEqual(await self.stop_while_idle([comment], [comment, done]), [
            "Begin.", LAST + "\n\nPlatform updates:\ncomment.new hyp-1 human\n"
                           "experiment.status exp-1 COMPLETED acc 0.9"])


class IdleNudgeTest(unittest.IsolatedAsyncioTestCase):
    async def test_the_wait_before_a_nudge_doubles_while_nothing_comes_and_an_update_resets_it(self):
        stop_flag = [False]
        waits = []
        batches = [[], [], [], [{"kind": "comment.new", "subject": "hyp-1", "value": "human"}], [], []]

        async def next_batch(env, stopped, idle_s):
            waits.append(idle_s)
            if len(waits) == len(batches):
                stop_flag[0] = True
            return batches[len(waits) - 1]

        with mock.patch.object(run, "ClaudeSDKClient", IdleClient), \
                mock.patch.object(run.updates, "next_batch", next_batch), \
                mock.patch.object(run.updates, "jobs_in_flight", mock.AsyncMock(return_value=1)), \
                mock.patch.object(run.updates, "drain", mock.AsyncMock(return_value=[])), \
                contextlib.redirect_stdout(io.StringIO()), self.assertLogs("llm_agent"):
            await asyncio.wait_for(run._run_one_session(_setup(), "m", None, stop_flag), timeout=5)
        nudge = run.updates.IDLE_NUDGE_S
        self.assertEqual(waits, [nudge, 2 * nudge, 4 * nudge, 4 * nudge, nudge, 2 * nudge])
        queries = HangingClient.instances[-1].queries
        self.assertEqual(queries[1:4], [run.updates.IDLE_PROMPT.format(minutes=m) for m in (30, 60, 120)])
        self.assertEqual(queries[5], run.updates.IDLE_PROMPT.format(minutes=30))


class ContextOverflowTest(unittest.IsolatedAsyncioTestCase):
    async def test_a_session_that_overflows_is_replaced_by_a_fresh_one_from_its_last_words(self):
        home = tempfile.mkdtemp()
        os.makedirs(os.path.join(home, "projects/p/s-1/subagents"))
        with open(os.path.join(home, "projects/p/s-1/subagents/a.jsonl"), "w") as f:
            f.write("{}\n")
        with open(os.path.join(home, "projects/p/s-1.jsonl"), "w") as f:
            for record in ({"type": "user", "message": {"content": "Begin."}},
                           {"type": "assistant", "message": {"content": [{"type": "text", "text": "job exp-7 runs lr 3e-4"}]}},
                           {"type": "assistant", "isSidechain": True, "message": {"content": [{"type": "text", "text": "a subagent's"}]}},
                           {"type": "assistant", "message": {"content": [{"type": "tool_use", "name": "Bash"}]}}):
                f.write(json.dumps(record) + "\n")
            f.write("not json\n")

        class OverflowingClient(StoppableClient):
            async def receive_response(self):
                yield ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=True, num_turns=1,
                                    session_id="s-1", result="Prompt is too long")

        setup = _setup()
        with mock.patch.object(run, "ClaudeSDKClient", OverflowingClient), \
                mock.patch.dict(os.environ, {"CLAUDE_CONFIG_DIR": home}), \
                contextlib.redirect_stdout(io.StringIO()), self.assertLogs("llm_agent"):
            self.assertEqual(await asyncio.wait_for(run._run_one_session(setup, "m", "s-1", [False]), timeout=5), (None, False))
            resume_id, prompt = run._restart_state(setup)
            self.assertIsNone(resume_id)
            self.assertIn("job exp-7 runs lr 3e-4", prompt)
            self.assertNotIn("a subagent's", prompt)
            self.assertEqual(run._transcript_files("s-1"), ["projects/p/s-1.jsonl"], "a subagent's transcript is stored with no session")
            run._delete_retired_transcripts()
        self.assertEqual(os.listdir(os.path.join(home, "projects/p")), [])

    def test_the_rotation_point_follows_a_smaller_context_window(self):
        def result(model_usage):
            return ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=False, num_turns=1,
                                 session_id="s-1", model_usage=model_usage)

        self.assertEqual(run._rotation_threshold(result(None), "claude-x"), run._CONTEXT_COMPACTION_THRESHOLD_TOKENS)
        self.assertEqual(run._rotation_threshold(result({"claude-x-2026": {"contextWindow": 1_000_000},
                                                         "claude-h": {"contextWindow": 200_000}}), "claude-x"),
                         run._CONTEXT_COMPACTION_THRESHOLD_TOKENS)
        self.assertEqual(run._rotation_threshold(result({"claude-x-2026": {"contextWindow": 200_000}}), "claude-x"), 150_000)


class TurnWatchdogTest(unittest.IsolatedAsyncioTestCase):
    async def test_silent_cli_ends_session_with_turn_stalled(self):
        setup = _setup()
        with mock.patch.object(run, "ClaudeSDKClient", HangingClient), \
                mock.patch.object(run, "_TURN_SILENCE_TIMEOUT_S", 0.05), \
                contextlib.redirect_stdout(io.StringIO()) as out:
            with self.assertRaises(run.TurnStalled):
                await asyncio.wait_for(
                    run._run_one_session(setup, "m", None, [False]), timeout=5)
        self.assertTrue(HangingClient.instances[-1].disconnected)
        self.assertIn("agent-a say   pulling metrics", out.getvalue())


def _event(kind):
    return StreamEvent(uuid="u", session_id="s", event={"type": kind})


class StallingClient:
    """One CLI stream: each call it is asked for starts and then goes silent until interrupted,
    the first stalls times; a call after those answers."""

    def __init__(self, stalls):
        self.stalls, self.queries, self.stream = stalls, [], asyncio.Queue()
        self.ask()

    def ask(self):
        for message in (SystemMessage(subtype="status", data={"status": "requesting"}), _event("message_start")):
            self.stream.put_nowait(message)
        if self.stalls:
            self.stalls -= 1
            return
        for message in (AssistantMessage(content=[TextBlock(text="answered")], model="m"), _event("message_stop"), _result()):
            self.stream.put_nowait(message)

    async def query(self, prompt):
        self.queries.append(prompt)
        self.ask()

    async def interrupt(self):
        self.stream.put_nowait(_event("message_stop"))
        self.stream.put_nowait(_result())

    async def receive_response(self):
        while not isinstance(message := await self.stream.get(), ResultMessage):
            yield message
        yield message


class CallStallTest(unittest.IsolatedAsyncioTestCase):
    async def receive(self, client):
        received = []
        with mock.patch.object(run, "_CALL_STALL_S", 0.05):
            async for message in run._receive_turn(client):
                received.append(message)
        return received

    async def test_a_stalled_call_is_interrupted_and_asked_for_again_once(self):
        client = StallingClient(stalls=1)
        received = await self.receive(client)
        self.assertEqual(client.queries, [run._STALL_PROMPT])
        self.assertEqual([m.content[0].text for m in received if isinstance(m, AssistantMessage)], ["answered"])
        self.assertEqual(sum(isinstance(m, ResultMessage) for m in received), 1)

    async def test_a_call_that_stalls_again_ends_the_session(self):
        with self.assertRaises(run.TurnStalled):
            await self.receive(StallingClient(stalls=2))

    async def test_a_tool_running_after_its_call_ended_is_no_stall(self):
        class SlowTool(StallingClient):
            def ask(self):
                self.stream.put_nowait(_event("message_start"))
                self.stream.put_nowait(_event("message_stop"))
                asyncio.get_running_loop().call_later(0.2, self.stream.put_nowait, _result())

        client = SlowTool(stalls=0)
        await self.receive(client)
        self.assertEqual(client.queries, [])


class ReconnectTest(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        # A machine of its own: no session in flight from another test, and a model home.
        for patcher in (mock.patch.object(run, "_live_session_id", None),
                        mock.patch.dict(os.environ, {"CLAUDE_CONFIG_DIR": tempfile.mkdtemp()})):
            patcher.start()
            self.addCleanup(patcher.stop)

    async def test_a_raised_session_is_followed_by_what_it_left_on_disk_and_a_good_turn_resets_the_backoff(self):
        setup = _setup()
        stop_flag = [False]
        resumed, sleeps = [], []

        async def session(setup, model, resume_id, stop_flag_, initial_prompt, on_good_turn):
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

        really_sleep = asyncio.sleep

        async def sleep(seconds):
            # The beat and the session store sleep too, here for less than any backoff.
            if seconds >= run._RECONNECT_BACKOFF_INITIAL_S:
                sleeps.append(seconds)
            await really_sleep(0)

        async def never(setup, stop_flag):
            await asyncio.Event().wait()

        with mock.patch.object(run, "_run_one_session", session), \
                mock.patch.object(run.core, "until_lost", never), \
                mock.patch.object(run, "_SESSION_STORE_INTERVAL_S", 1), \
                mock.patch.object(run.core, "install_signal_handler", lambda: stop_flag), \
                mock.patch.object(run.asyncio, "sleep", sleep), \
                self.assertLogs("llm_agent"):
            await asyncio.wait_for(run._run(setup, "m"), timeout=5)
        self.assertEqual(resumed, [None, "s-2", "s-2", "s-2"])
        self.assertEqual(sleeps, [30, 60, 30])

    # A planner's stored session lacked its closing reply: the store at the end of the turn ran
    # before the CLI had written it, and its run ended with that turn.
    async def test_the_session_is_stored_once_more_after_the_cli_has_exited(self):
        order = []

        async def session(*_):
            order.append("the CLI exited")
            return None, False

        async def store(setup):
            order.append("stored")

        answers = [{}, {"ended": "planned"}, {"ended": "planned"}]
        setup = _setup()
        setup.client.get_signup = lambda *_: answers.pop(0) if len(answers) > 1 else answers[0]
        with mock.patch.object(run, "_run_one_session", session), \
                mock.patch.object(run, "_store_session", store), \
                self.assertLogs("llm_agent"):
            await asyncio.wait_for(run._run_until_stopped(setup, "m", [False]), timeout=5)
        self.assertEqual(order, ["the CLI exited", "stored"])

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

    def test_the_session_in_flight_is_resumed_over_the_one_saved_once_its_transcript_holds_the_turn(self):
        setup = _setup()
        run._save_resume_id(setup, "s-saved")
        with mock.patch.object(run, "_live_session_id", "s-live"):
            self.assertEqual(run._restart_state(setup), ("s-saved", None), "a session in flight with no transcript")
            transcript = os.path.join(os.environ["CLAUDE_CONFIG_DIR"], "projects", "p", "s-live.jsonl")
            os.makedirs(os.path.dirname(transcript))
            with open(transcript, "w") as f:
                f.write("the turn so far\n")
            self.assertEqual(run._restart_state(setup), ("s-live", None))


if __name__ == "__main__":
    unittest.main()
