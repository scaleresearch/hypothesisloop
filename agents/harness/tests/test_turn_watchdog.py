import asyncio
import contextlib
import io
import json
import os
import tempfile
import time
import unittest
from types import SimpleNamespace
from unittest import mock

from claude_agent_sdk import (AssistantMessage, RateLimitEvent, RateLimitInfo, ResultMessage, StreamEvent, SystemMessage,
                              TextBlock, UserMessage)

from hypothesisloop_agent import api_client, core, updates
from hypothesisloop_agent.claude import cli, connection, hooks, rotation, run, shutdown, store as store_module, stream, transcripts, turns
from hypothesisloop_agent.claude.store import Store
from conftest import SITUATION, FakePlatform, connect, served
from test_require_joined import AGENT_TOKEN

# What closes every prompt of a role in a run.
S = "\n\n" + SITUATION


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
    return SimpleNamespace(commands="usage: hl", workdir=tempfile.mkdtemp(), statedir=tempfile.mkdtemp(),
                           client=FakePlatform(),
                           cfg=SimpleNamespace(agent_id="agent-a", role="researcher", api_url="http://x", token=AGENT_TOKEN, platform_experiment_id="pe-1"))


# A machine stopped while its agent's run goes on is told exactly that, ahead of what to do with its turn.
LAST = ("You are being stopped while your run on the platform goes on: the platform ended nothing of yours. "
        "Only this machine is being stopped, and the platform does not say why. " + shutdown._SHUTDOWN_PROMPT + S)


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
        with mock.patch.object(connection, "ClaudeSDKClient", StoppableClient), \
                mock.patch.object(updates, "drain", mock.AsyncMock(return_value=[])), \
                mock.patch.object(StoppableClient, "hangs", hangs), \
                mock.patch.object(shutdown, "_SHUTDOWN_TURN_S", 0.05), \
                mock.patch.object(asyncio, "sleep", mock.AsyncMock(side_effect=lambda _: stop_flag.__setitem__(0, True))), \
                contextlib.redirect_stdout(io.StringIO()):
            await asyncio.wait_for(connect(_setup(), None, stop_flag), timeout=5)
        return HangingClient.instances[-1]

    async def test_a_stop_interrupts_the_turn_and_gives_one_last_turn(self):
        client = await self.stop_mid_turn(hangs=False)
        self.assertTrue(client.interrupted.is_set())
        self.assertEqual(client.queries, ["Begin." + S, LAST])
        self.assertTrue(client.disconnected)

    async def test_the_last_turn_is_cut_off_at_its_deadline(self):
        client = await self.stop_mid_turn(hangs=True)
        self.assertEqual(client.queries, ["Begin." + S, LAST])
        self.assertTrue(client.disconnected)

    # A researcher that had just finished was interrupted in its closing message and given a
    # turn to file summaries the platform had already checked it did not owe.
    async def stop_after_finishing(self, client_class, sleep) -> StoppableClient:
        setup = _setup()
        setup.client.signup = {"ended": "finished"}
        with mock.patch.object(connection, "ClaudeSDKClient", client_class), \
                mock.patch.object(asyncio, "sleep", sleep), \
                contextlib.redirect_stdout(io.StringIO()):
            await asyncio.wait_for(connect(setup, None, self.stop_flag), timeout=5)
        return HangingClient.instances[-1]

    async def test_a_stop_after_the_agent_finished_lets_its_turn_end_and_gives_no_other(self):
        self.stop_flag = [False]
        given_time = asyncio.Event()

        async def sleep(seconds):
            if seconds == shutdown._SHUTDOWN_TURN_S:
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
        self.assertEqual(client.queries, ["Begin." + S])
        self.assertTrue(client.disconnected)

    async def test_a_finished_agent_that_works_on_is_interrupted_at_the_deadline_and_gets_no_other_turn(self):
        self.stop_flag = [False]
        client = await self.stop_after_finishing(StoppableClient, mock.AsyncMock(side_effect=lambda _: self.stop_flag.__setitem__(0, True)))
        self.assertTrue(client.interrupted.is_set())
        self.assertEqual(client.queries, ["Begin." + S])


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
        with mock.patch.object(connection, "ClaudeSDKClient", IdleClient), \
                mock.patch.object(updates, "next_batch", stopped_while_waiting), \
                mock.patch.object(updates, "jobs_in_flight", mock.AsyncMock(return_value=1)), \
                mock.patch.object(updates, "drain", mock.AsyncMock(return_value=finished_since)), \
                contextlib.redirect_stdout(io.StringIO()):
            await asyncio.wait_for(connect(setup, None, stop_flag), timeout=5)
        return HangingClient.instances[-1].queries

    async def test_a_stop_between_turns_gives_one_last_turn(self):
        self.assertEqual(await self.stop_while_idle([], []), ["Begin." + S, LAST])

    # A run that reached its end with no signal yet ended the process at the turn boundary, so
    # a coordinator never wrote the summary its briefing promises a turn for. A token the
    # platform refuses has nothing left to file with.
    async def run_ends_while_idle(self, role: str, signup: dict | Exception, experiment: dict) -> list[str]:
        async def nothing(env, stopped, idle_s):
            return []

        setup = _setup()
        setup.cfg.role = role
        setup.client = FakePlatform(experiment, signup)
        with mock.patch.object(connection, "ClaudeSDKClient", IdleClient), \
                mock.patch.object(updates, "next_batch", nothing), \
                mock.patch.object(updates, "drain", mock.AsyncMock(return_value=[])), \
                contextlib.redirect_stdout(io.StringIO()):
            await asyncio.wait_for(connect(setup, None, [False]), timeout=5)
        return HangingClient.instances[-1].queries

    async def test_a_run_the_platform_ended_gets_its_last_turn_with_no_signal(self):
        closed = {"status": "closed", "closed_at": "2026-01-01T02:48:05Z", "close_reason": "end_time_reached"}
        self.assertEqual(await self.run_ends_while_idle("coordinator", {}, closed), [
            "Begin." + S, "You are being stopped because the platform experiment reached its end time and the platform closed it at "
                      "2026-01-01T02:48:05Z. " + shutdown._COORDINATOR_SHUTDOWN_PROMPT + S])
        self.assertEqual(await self.run_ends_while_idle("researcher", {"ended": "cut"}, {"status": "running"}), [
            "Begin." + S, "You are being stopped because you were cut from the platform experiment at a stage boundary. " + shutdown._SHUTDOWN_PROMPT + S])

    # In a live run the close arrived as an update, was handed over as a turn of its own and cut by the
    # stop four seconds later, and only then did the last turn start. A batch that comes with the
    # end of the run opens the last turn, with the batch in it: not handed over, it is read again.
    async def test_a_batch_that_comes_with_the_close_opens_the_last_turn(self):
        closed_event = {"kind": "platform_experiment.status", "subject": "pe-1", "value": "closed"}
        experiment = {"status": "running"}

        async def the_close(env, stopped, idle_s):
            experiment.update(status="closed", closed_at="2026-01-01T02:48:05Z", close_reason="end_time_reached")
            return [closed_event]

        setup = _setup()
        setup.cfg.role = "coordinator"
        setup.client = FakePlatform(experiment)
        with mock.patch.object(connection, "ClaudeSDKClient", IdleClient), \
                mock.patch.object(updates, "next_batch", the_close), \
                mock.patch.object(updates, "drain", mock.AsyncMock(return_value=[closed_event])), \
                contextlib.redirect_stdout(io.StringIO()):
            await asyncio.wait_for(connect(setup, None, [False]), timeout=5)
        self.assertEqual(HangingClient.instances[-1].queries, [
            "Begin." + S, "You are being stopped because the platform experiment reached its end time and the platform closed it at "
                      "2026-01-01T02:48:05Z. " + shutdown._COORDINATOR_SHUTDOWN_PROMPT + "\n\nPlatform updates:\n"
                      "platform_experiment.status pe-1 closed" + S])

    async def test_a_refused_token_gets_no_last_turn(self):
        refused = api_client.APIError("GET", "http://api/x", 401, "unauthenticated")
        self.assertEqual(await self.run_ends_while_idle("researcher", refused, {"status": "running"}), ["Begin." + S])

    async def test_the_last_turn_opens_with_what_finished_since_the_last_delivery(self):
        done = {"kind": "experiment.status", "subject": "exp-1", "value": "COMPLETED", "detail": "acc 0.9"}
        comment = {"kind": "comment.new", "subject": "hyp-1", "value": "human"}
        self.assertEqual(await self.stop_while_idle([comment], [comment, done]), [
            "Begin." + S, LAST.removesuffix(S) + "\n\nPlatform updates:\ncomment.new hyp-1 human\n"
                           "experiment.status exp-1 COMPLETED acc 0.9" + S])


class ReviewTest(unittest.IsolatedAsyncioTestCase):
    async def waits(self, role: str, batches: list) -> tuple[list, list]:
        stop_flag = [False]
        waits = []

        async def next_batch(env, stopped, idle_s):
            waits.append(idle_s)
            if len(waits) == len(batches):
                stop_flag[0] = True
            return batches[len(waits) - 1]

        setup = _setup()
        setup.cfg.role = role
        with mock.patch.object(connection, "ClaudeSDKClient", IdleClient), \
                mock.patch.object(updates, "next_batch", next_batch), \
                mock.patch.object(updates, "jobs_in_flight", mock.AsyncMock(return_value=1)), \
                mock.patch.object(updates, "drain", mock.AsyncMock(return_value=[])), \
                contextlib.redirect_stdout(io.StringIO()):
            await asyncio.wait_for(connect(setup, None, stop_flag), timeout=5)
        return waits, HangingClient.instances[-1].queries

    # A lead rested from 16:00 to the close while a nudge that doubled waited. A coordinator
    # reviews the run at a fixed interval that nothing woke it, with the situation.
    async def test_a_coordinator_is_reviewed_at_a_fixed_interval_and_an_update_is_its_wake(self):
        comment = {"kind": "comment.new", "subject": "hyp-1", "value": "human"}
        waits, queries = await self.waits("coordinator", [[], [], [comment], [], []])
        self.assertEqual(waits, [updates.REVIEW_S] * 5)
        review = updates.REVIEW_PROMPT.format(minutes=20) + S
        self.assertEqual(queries[1:5], [review, review, "Platform updates:\ncomment.new hyp-1 human" + S, review])

    # Free places are the platform's to tell (capacity.idle): a coordinator that leaves them is
    # reviewed at the same interval and told nothing of them at the end of its turn.
    async def test_free_places_change_neither_the_review_nor_the_end_of_a_turn(self):
        idle = {"run": {"minutes_left": 200}, "objective": [{"gap_to_target_pct": 27.3}],
                "capacity": {"fits_now": 8, "jobs_in_flight_run": {"queued": 0, "running": 0}}}
        comment = {"kind": "comment.new", "subject": "hyp-1", "value": "human"}
        with mock.patch.object(updates, "standing", mock.AsyncMock(return_value=idle)):
            waits, queries = await self.waits("coordinator", [[], [comment], []])
        review = updates.review_prompt(20, idle) + S
        self.assertEqual(waits, [updates.REVIEW_S] * 3)
        self.assertEqual(queries[1:3], [review, "Platform updates:\ncomment.new hyp-1 human" + S])

    # The platform's agent.idle, kept in its store, is the one timer of an agent.
    async def test_an_agent_waits_for_an_update_with_no_limit_of_its_own(self):
        comment = {"kind": "comment.new", "subject": "hyp-1", "value": "human"}
        waits, queries = await self.waits("researcher", [[comment], [comment]])
        self.assertEqual(waits, [float("inf")] * 2)
        self.assertEqual(queries[1], "Platform updates:\ncomment.new hyp-1 human" + S)


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
        with mock.patch.object(connection, "ClaudeSDKClient", OverflowingClient), \
                mock.patch.dict(os.environ, {"CLAUDE_CONFIG_DIR": home}), \
                contextlib.redirect_stdout(io.StringIO()), self.assertLogs("llm_agent"):
            store = Store(setup)
            self.assertEqual(await asyncio.wait_for(connect(setup, "s-1", [False], store), timeout=5), False)
            resume_id, prompt = store.restart_state()
            self.assertIsNone(resume_id)
            self.assertIn("job exp-7 runs lr 3e-4", prompt)
            self.assertNotIn("a subagent's", prompt)
            self.assertEqual(transcripts.files("s-1"), ["projects/p/s-1.jsonl"], "a subagent's transcript is stored with no session")
            store.delete_retired()
        self.assertEqual(os.listdir(os.path.join(home, "projects/p")), [])

    # Measured on the bundled CLI: a subagent that ends as the handoff's prompt is sent starts a
    # turn of its own first, and the handoff is the result of the turn after, which says its prompt.
    async def test_the_handoff_is_read_from_the_turn_that_took_its_prompt(self):
        def result(text):
            return ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=False, num_turns=1,
                                 session_id="s-1", result=text)

        turns = [[AssistantMessage(content=[TextBlock(text="worked")], model="m", usage={"input_tokens": 10_000_000}),
                  result("worked")],
                 [SystemMessage("task_notification", {"task_id": "sub-1", "status": "completed"}), result("sub-1 done")],
                 [UserMessage(content=rotation.COMPACTION_PROMPT), result("where things stand")]]

        class RotatingClient(StoppableClient):
            async def receive_response(self):
                for message in turns.pop(0):
                    yield message

        setup = _setup()
        store = Store(setup)
        with mock.patch.object(connection, "ClaudeSDKClient", RotatingClient), \
                contextlib.redirect_stdout(io.StringIO()), self.assertLogs("llm_agent"):
            self.assertEqual(await asyncio.wait_for(connect(setup, None, [False], store), timeout=5), True)
        self.assertEqual(store.handoff(), "where things stand")

    # A rotation ends the CLI and every subagent at work with it, whose ideas the lead then
    # started again. Past the rotation point it waits for them, up to the ceiling, and the next
    # session is told which stopped.
    async def test_a_rotation_waits_for_live_subagents_until_the_ceiling(self):
        def result(text):
            return ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=False, num_turns=1,
                                 session_id="s-1", result=text, model_usage={"m-1": {"contextWindow": 1_000_000}})

        def turn(tokens):
            return [SystemMessage("background_tasks_changed", {"tasks": [
                        {"task_id": "sub-1", "task_type": "local_agent", "description": "lr warmup on hyp-1"},
                        {"task_id": "sub-2", "task_type": "local_agent", "description": "fp16 on hyp-2"},
                        {"task_id": "b-1", "task_type": "local_bash", "description": "make"}]}),
                    AssistantMessage(content=[TextBlock(text="worked")], model="m", usage={"input_tokens": tokens}),
                    result("worked")]

        # sub-2 completes during the handoff's turn and is not started again.
        turns = [turn(450_000), turn(rotation._CONTEXT_CEILING_TOKENS),
                 [SystemMessage("background_tasks_changed", {"tasks": [
                     {"task_id": "sub-1", "task_type": "local_agent", "description": "lr warmup on hyp-1"}]}),
                  UserMessage(content=rotation.COMPACTION_PROMPT), result("where things stand")]]

        class RotatingClient(StoppableClient):
            async def receive_response(self):
                for message in turns.pop(0):
                    yield message

        comment = [{"kind": "comment.new", "subject": "hyp-1", "value": "human"}]
        setup = _setup()
        store = Store(setup)
        with mock.patch.object(connection, "ClaudeSDKClient", RotatingClient), \
                mock.patch.object(updates, "next_batch", mock.AsyncMock(return_value=comment)), \
                mock.patch.object(updates, "drain", mock.AsyncMock(return_value=[])), \
                contextlib.redirect_stdout(io.StringIO()), self.assertLogs("llm_agent") as logged:
            self.assertEqual(await asyncio.wait_for(connect(setup, None, [False], store), timeout=5), True)
        self.assertIn("rotation deferred: 3 subagents live", "\n".join(logged.output))
        self.assertEqual(HangingClient.instances[-1].queries[1:], ["Platform updates:\ncomment.new hyp-1 human" + S, rotation.COMPACTION_PROMPT])
        self.assertEqual(store.handoff(), "where things stand\n\n" + rotation._STOPPED_SUBAGENTS.format(
            tasks="- lr warmup on hyp-1"))
        self.assertIn("Start each again with Agent", store.restart_state()[1])

    # Subagents, which never rotate, re-read 400-450K on every call when the CLI compacted only near
    # the model's 1M window. It compacts every session past the configured window, and a lead with
    # no subagent live rotates with the platform's handoff before that.
    def test_the_cli_compacts_sessions_past_a_bounded_window(self):
        window = cli.AUTO_COMPACT_WINDOW_TOKENS
        self.assertEqual(cli.CLI_ENV["CLAUDE_CODE_AUTO_COMPACT_WINDOW"], str(window))
        self.assertGreaterEqual(window, 100_000)
        self.assertLess(rotation._CONTEXT_COMPACTION_THRESHOLD_TOKENS, window - 33_000)

    def test_the_rotation_point_follows_a_smaller_context_window(self):
        def result(model_usage):
            return ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=False, num_turns=1,
                                 session_id="s-1", model_usage=model_usage)

        threshold, ceiling = rotation._CONTEXT_COMPACTION_THRESHOLD_TOKENS, rotation._CONTEXT_CEILING_TOKENS
        self.assertEqual(rotation.limits(result(None), "claude-x"), (threshold, ceiling))
        self.assertEqual(rotation.limits(result({"claude-x-2026": {"contextWindow": 1_000_000},
                                                      "claude-h": {"contextWindow": 200_000}}), "claude-x"), (threshold, ceiling))
        with mock.patch.object(rotation, "_CONTEXT_CEILING_TOKENS", 120_000):
            self.assertEqual(rotation.limits(result({"claude-x": {"contextWindow": 1_000_000}}), "claude-x"), (120_000, 120_000))
        self.assertEqual(rotation.limits(result({"claude-x-2026": {"contextWindow": 200_000}}), "claude-x"), (150_000, 180_000))


# A researcher ran 3 to 10 subagents at once, and its session grew past its rotation since some
# were always live. Starts past the cap are refused, those of one step included: the CLI lists a
# subagent only after the hooks of the step that started it ran.
class SubagentCapTest(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.now = 0.0
        self.background = {}
        self.running = SimpleNamespace(current=None)
        self.hooks = hooks._subagent_hooks(hooks.Subagents(self.background, clock=lambda: self.now), self.running)

    async def start(self, use: str) -> dict:
        [hook] = self.hooks["PreToolUse"].hooks
        self.assertEqual(self.hooks["PreToolUse"].matcher, "Agent")
        return await hook({"hook_event_name": "PreToolUse", "tool_name": "Agent", "tool_input": {}}, use, {})

    async def returned(self, use: str, response: dict | None) -> None:
        event = "PostToolUse" if response is not None else "PostToolUseFailure"
        [hook] = self.hooks[event].hooks
        self.assertEqual(await hook({"hook_event_name": event, "tool_name": "Agent", "tool_response": response}, use, {}), {})

    def listed(self, *ids: str) -> None:
        stream.note_background(SystemMessage("background_tasks_changed", {"tasks": [
            {"task_id": i, "task_type": "local_agent"} for i in ids] + [{"task_id": "b-1", "task_type": "local_bash"}]}),
            self.background)

    async def test_starts_past_the_cap_are_refused_until_one_ends(self):
        self.running.current = served(_setup().cfg, max_subagents=3)
        for use in ("t-1", "t-2", "t-3"):
            self.assertEqual(await self.start(use), {}, use)
        told = (await self.start("t-4"))["hookSpecificOutput"]
        self.assertEqual(told["permissionDecision"], "deny")
        self.assertEqual(told["permissionDecisionReason"], hooks.SUBAGENT_CAP.format(live=3))
        for use, agent in (("t-1", "a-1"), ("t-2", "a-2"), ("t-3", "a-3")):
            await self.returned(use, {"isAsync": True, "status": "async_launched", "agentId": agent})
        self.listed("a-1", "a-2", "a-3")
        self.assertIn("deny", str(await self.start("t-5")), "each listed once, a command not counted")
        self.listed("a-1", "a-3")
        self.assertEqual(await self.start("t-6"), {})

    async def test_a_start_that_failed_or_ran_in_the_foreground_counts_no_more(self):
        self.running.current = served(_setup().cfg, max_subagents=2)
        await self.start("t-1")
        await self.start("t-2")
        await self.returned("t-1", None)
        await self.returned("t-2", {"status": "completed", "content": []})
        self.assertEqual(await self.start("t-3"), {})
        await self.returned("t-3", {"status": "async_launched", "agentId": "a-3"})
        await self.start("t-4")
        self.assertIn("deny", str(await self.start("t-5")))
        self.now += hooks._STARTING_S
        self.assertEqual(await self.start("t-5"), {}, "a start the CLI never listed counts for a while only")


class TurnWatchdogTest(unittest.IsolatedAsyncioTestCase):
    async def test_silent_cli_ends_session_with_turn_stalled(self):
        setup = _setup()
        with mock.patch.object(connection, "ClaudeSDKClient", HangingClient), \
                mock.patch.object(stream, "TURN_SILENCE_TIMEOUT_S", 0.05), \
                contextlib.redirect_stdout(io.StringIO()) as out:
            with self.assertRaises(turns.TurnStalled):
                await asyncio.wait_for(
                    connect(setup, None, [False]), timeout=5)
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
        with mock.patch.object(turns, "_CALL_STALL_S", 0.05):
            async for message in turns.receive(client):
                received.append(message)
        return received

    async def test_a_stalled_call_is_interrupted_and_asked_for_again_once(self):
        client = StallingClient(stalls=1)
        received = await self.receive(client)
        self.assertEqual(client.queries, [turns._STALL_PROMPT])
        self.assertEqual([m.content[0].text for m in received if isinstance(m, AssistantMessage)], ["answered"])
        self.assertEqual(sum(isinstance(m, ResultMessage) for m in received), 1)

    async def test_a_call_that_stalls_again_ends_the_session(self):
        with self.assertRaises(turns.TurnStalled):
            await self.receive(StallingClient(stalls=2))

    # message_start comes with the first token, after the prefill of the whole context: resumed
    # 700K-token sessions with a cold cache were cut there, on every call.
    async def test_a_call_waiting_for_its_first_event_is_no_stall(self):
        class SlowPrefill(StallingClient):
            def ask(self):
                self.stream.put_nowait(SystemMessage(subtype="status", data={"status": "requesting"}))
                later = (_event("message_start"), AssistantMessage(content=[TextBlock(text="answered")], model="m"),
                         _event("message_stop"), _result())
                asyncio.get_running_loop().call_later(0.2, lambda: [self.stream.put_nowait(m) for m in later])

        client = SlowPrefill(stalls=0)
        received = await self.receive(client)
        self.assertEqual(client.queries, [])
        self.assertEqual([m.content[0].text for m in received if isinstance(m, AssistantMessage)], ["answered"])

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
    async def test_a_raised_session_is_followed_by_what_it_left_on_disk_and_a_good_turn_resets_the_backoff(self):
        setup = _setup()
        store = Store(setup)
        stop_flag = [False]
        resumed, sleeps = [], []

        async def session(setup, store, running, resume_id, stop_flag_, initial_prompt, on_good_turn):
            resumed.append(resume_id)
            if len(resumed) == 1:
                store.save_resume_id("s-2")
                raise turns.ProcessError("the CLI exited", exit_code=1)
            if len(resumed) == 2:
                raise Exception("the SDK's own word for a CLI that died mid-stream")
            on_good_turn()
            if len(resumed) == 4:
                stop_flag[0] = True
            return False

        really_sleep = asyncio.sleep

        async def sleep(seconds):
            await really_sleep(0)

        async def pause(seconds, stop_flag_):
            sleeps.append(seconds)

        async def never(setup, stop_flag):
            await asyncio.Event().wait()

        with mock.patch.object(connection, "run", session), \
                mock.patch.object(core, "until_lost", never), \
                mock.patch.object(store_module, "_STORE_INTERVAL_S", 1), \
                mock.patch.object(core, "install_signal_handler", lambda: stop_flag), \
                mock.patch.object(asyncio, "sleep", sleep), \
                mock.patch.object(run, "_pause", pause), \
                self.assertLogs("llm_agent"):
            await asyncio.wait_for(run._run(setup, store), timeout=5)
        self.assertEqual(resumed, [None, "s-2", "s-2", "s-2"])
        self.assertEqual(sleeps, [30, 60, 30])

    # A planner's stored session lacked its closing reply: the store at the end of the turn ran
    # before the CLI had written it, and its run ended with that turn.
    async def test_the_session_is_stored_once_more_after_the_cli_has_exited(self):
        order = []

        async def session(*_):
            order.append("the CLI exited")
            return False

        async def save():
            order.append("stored")

        answers = [{}, {"ended": "planned"}, {"ended": "planned"}]
        setup = _setup()
        setup.client.signup = lambda: answers.pop(0) if len(answers) > 1 else answers[0]
        store = Store(setup)
        store.save = save
        with mock.patch.object(connection, "run", session), self.assertLogs("llm_agent"):
            await asyncio.wait_for(run._run_until_stopped(setup, store, [False]), timeout=5)
        self.assertEqual(order, ["the CLI exited", "stored"])

    # A bug in the harness once read as a flaky connection and was retried for the whole run.
    async def test_an_error_that_is_no_lost_connection_ends_the_process(self):
        async def session(*_):
            raise KeyError("session_id")

        setup = _setup()
        with mock.patch.object(connection, "run", session), \
                mock.patch.object(core, "install_signal_handler", lambda: [False]), \
                self.assertRaises(KeyError):
            await asyncio.wait_for(run._run(setup, Store(setup)), timeout=5)

    def test_a_handoff_opens_the_next_session_only_while_no_session_id_is_saved(self):
        setup = _setup()
        store = Store(setup)
        with open(os.path.join(setup.statedir, store_module._HANDOFF_FILE), "w") as f:
            f.write("state so far")
        self.assertEqual(store.restart_state(), (None, "Continue this experiment from the following handoff:\n\nstate so far"))
        store.save_resume_id("s-3")
        self.assertEqual(store.restart_state(), ("s-3", None))

    def test_the_session_in_flight_is_resumed_over_the_one_saved_once_its_transcript_holds_the_turn(self):
        setup = _setup()
        store = Store(setup)
        store.save_resume_id("s-saved")
        store.seen(SimpleNamespace(session_id="s-live"))
        self.assertEqual(store.restart_state(), ("s-saved", None), "a session in flight with no transcript")
        transcript = os.path.join(os.environ["CLAUDE_CONFIG_DIR"], "projects", "p", "s-live.jsonl")
        os.makedirs(os.path.dirname(transcript))
        with open(transcript, "w") as f:
            f.write("the turn so far\n")
        self.assertEqual(store.restart_state(), ("s-live", None))


def _refusal(resets_at: float) -> RateLimitEvent:
    return RateLimitEvent(rate_limit_info=RateLimitInfo(status="rejected", resets_at=int(resets_at), rate_limit_type="seven_day",
                                                        utilization=1.0), uuid="u", session_id="s-1")


class RefusedClient(HangingClient):
    """Answers every turn as the CLI does once the model provider refuses the account's calls."""

    resets_at = 0.0

    async def receive_response(self):
        yield _refusal(RefusedClient.resets_at)
        yield ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=True, num_turns=1,
                            session_id="s-1", result="You've hit your weekly limit")


# An account neared its 7-day allowance. A refused agent must wait for the reset the
# provider states, spend no restart and no reconnect on the way, and resume.
class RateLimitTest(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        for patcher in (mock.patch.object(stream, "_rate_limit", None), mock.patch.object(stream, "limit_since", None),
                        mock.patch.object(stream.progress, "_model", {})):
            patcher.start()
            self.addCleanup(patcher.stop)

    async def test_a_refused_turn_ends_the_session_and_the_run_loop_waits_until_the_reset(self):
        resets_at = time.time() + 3 * 86400
        with mock.patch.object(connection, "ClaudeSDKClient", RefusedClient), mock.patch.object(RefusedClient, "resets_at", resets_at), \
                contextlib.redirect_stdout(io.StringIO()), self.assertLogs("llm_agent"):
            self.assertFalse(await asyncio.wait_for(connect(_setup(), None, [False]), timeout=5))
        self.assertEqual(stream.refused_until(), int(resets_at))

        setup, stop_flag, openings, pauses = _setup(), [False], [], []
        store = Store(setup)

        async def session(setup, store, running, resume_id, stop_flag_, initial_prompt, on_good_turn):
            from hypothesisloop_agent.claude import prompts

            openings.append(await prompts.opening(setup, store, "s-1", None))
            if len(openings) == 2:
                stream.limit_lifted()
                stop_flag[0] = True
            return False

        async def pause(seconds, stop_flag_):
            pauses.append(seconds)

        said = []
        with mock.patch.object(connection, "run", session), mock.patch.object(run, "_pause", pause), \
                mock.patch.object(stream.progress, "doing", said.append), self.assertLogs("llm_agent"):
            await asyncio.wait_for(run._run_until_stopped(setup, store, stop_flag), timeout=5)
        [waited] = pauses
        self.assertAlmostEqual(waited, 3 * 86400, delta=60)
        self.assertIn("paused until", said[-1])
        self.assertIn("seven_day limit", said[-1])
        self.assertNotIn("refused your calls", openings[0])
        self.assertIn("refused your calls", openings[1])
        self.assertIsNone(stream.limit_since, "a good turn after the wait still says it")

    async def test_a_reset_that_has_passed_refuses_nothing(self):
        stream._rate_limit = _refusal(time.time() - 1).rate_limit_info
        self.assertIsNone(stream.refused_until())

    async def test_a_stop_ends_a_wait_at_once(self):
        stop_flag = [False]
        asyncio.get_running_loop().call_later(0.05, stop_flag.__setitem__, 0, True)
        await asyncio.wait_for(run._pause(3600, stop_flag), timeout=5)


if __name__ == "__main__":
    unittest.main()
