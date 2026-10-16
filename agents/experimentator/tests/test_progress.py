import asyncio
import contextlib
import io
import os
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

from claude_agent_sdk import AssistantMessage, StreamEvent, TextBlock

from hypothesisloop_agent import progress
from test_require_joined import AGENT_TOKEN
from hypothesisloop_agent.claude import run


class Clock:
    def __init__(self):
        self.now = 0.0

    def __call__(self):
        return self.now


class ProgressTest(unittest.TestCase):
    def setUp(self):
        self.clock, self.lines, self.states = Clock(), [], []
        self.wall = Clock()
        self.wall.now = 1760000000.0
        self.progress = progress.Progress(quiet_limit_s=100, clock=self.clock, wall=self.wall, emit=lambda line: (
            self.states if line.startswith(progress.STATE_PREFIX + " ") else self.lines).append(line))

    def test_every_beat_prints_a_marker_the_launcher_reads_and_no_two_are_the_same(self):
        for _ in range(3):
            self.progress.beat()
        self.assertEqual(self.lines, ["hl-progress 1", "hl-progress 2", "hl-progress 3"])
        for line in self.lines:
            # agentlaunch.progressPattern on the launcher's side.
            self.assertRegex(line, r"^hl-progress [A-Za-z0-9._:-]{1,64}$")

    def test_waiting_between_turns_is_no_silence_however_long(self):
        self.progress.beat()
        self.clock.now = 10_000_000
        self.progress.beat()
        self.assertEqual(len(self.lines), 2)

    def test_a_turn_silent_past_the_limit_stops_the_marker_and_the_next_message_starts_it_again(self):
        self.progress.awaiting()
        self.clock.now = 100
        self.progress.beat()
        self.clock.now = 101
        self.progress.beat()
        self.progress.beat()
        self.assertEqual(self.lines, ["hl-progress 1"], "a turn silent for longer than the limit still printed progress")
        self.progress.awaiting()
        self.progress.beat()
        self.clock.now = 10_000
        self.progress.resting()
        self.progress.beat()
        self.assertEqual(self.lines, ["hl-progress 1", "hl-progress 2", "hl-progress 3"])

    # What an agent was doing was read from its machine's log. The harness says it in one line
    # the launcher reads with the marker: at once when it changes, at most every few seconds, and
    # again with every beat, so the newest is never far back in the output.
    def test_what_the_agent_is_doing_is_said_when_it_changes_and_with_every_beat(self):
        self.progress.beat()
        self.progress.doing("thinking")
        self.progress.doing("thinking")
        self.assertEqual(self.lines, ["hl-progress 1", "hl-activity thinking"])
        # A change inside the interval waits for the beat, and only the latest is said.
        self.clock.now = 1
        self.progress.doing("reading train.py")
        self.progress.doing("running hl capacity")
        self.assertEqual(len(self.lines), 2)
        self.clock.now = 2
        self.progress.beat()
        self.assertEqual(self.lines[2:], ["hl-progress 2", "hl-activity running hl capacity"])
        self.clock.now = 2 + progress.ACTIVITY_INTERVAL_S
        self.progress.doing("waiting for platform updates")
        self.assertEqual(self.lines[4:], ["hl-activity waiting for platform updates"])
        self.progress.beat()
        self.assertEqual(self.lines[5:], ["hl-progress 3", "hl-activity waiting for platform updates"])

    def test_a_sentence_is_one_printable_line_of_bounded_length(self):
        self.progress.doing("reading\n a\tfile \x1b[31m" + "x" * 300)
        [line] = self.lines
        said = line.removeprefix("hl-activity ")
        self.assertTrue(said.startswith("reading a file [31mxxx"), said)
        self.assertEqual(len(said), progress.MAX_ACTIVITY_LENGTH)
        # agentlaunch.activityPattern on the launcher's side.
        self.assertRegex(line, r"^hl-activity .+$")
        self.assertNotIn("\n", line)

    # Nothing outside the harness knew whether a lead had subagents at work or a turn in flight,
    # so a lead with neither looked like one at work. The launcher hands the newest state line to
    # the platform with every beat; launcherapi.parseHarnessState reads this format.
    def test_the_state_is_said_with_every_beat(self):
        self.progress.beat()
        self.assertEqual(self.states, ["hl-state subagents=0 turn=waiting since=1760000000"])
        self.wall.now += 30
        self.progress.awaiting()
        self.progress.subagents(3)
        self.wall.now += 30
        self.progress.awaiting()
        self.progress.model("allowed_warning", "seven_day", 0.95, 1760090000)
        self.progress.beat()
        self.assertEqual(self.states[-1], "hl-state subagents=3 turn=working since=1760000030 model_status=allowed_warning "
                                          "model_type=seven_day model_utilization=0.95 model_resets_at=1760090000")
        self.wall.now += 30
        self.progress.resting()
        self.progress.model("rejected", None, None, None)
        self.progress.beat()
        self.assertEqual(self.states[-1], "hl-state subagents=3 turn=waiting since=1760000090 model_status=rejected")
        self.assertEqual(len(self.states), 3)
        for line in self.states:
            self.assertLessEqual(len(line.removeprefix("hl-state ")), 200)

    # launcherapi.parseHarnessState reads instructions_digest and the launcher keeps 200 characters.
    def test_the_state_carries_the_digest_of_the_instructions_it_runs_within_the_launchers_limit(self):
        self.progress.instructions("0123456789abcdef" * 4, 12)
        self.progress.awaiting()
        self.progress.model("allowed_warning", "seven_day_opus", 0.95, 1760090000)
        self.progress.subagents(12)
        self.progress.beat()
        self.assertEqual(self.states[-1], "hl-state subagents=12 turn=working since=1760000000 instructions_digest=0123456789ab version=12 "
                                          "model_status=allowed_warning model_type=seven_day_opus model_utilization=0.95 model_resets_at=1760090000")
        self.assertLessEqual(len(self.states[-1].removeprefix("hl-state ")), 200)

    def test_a_harness_that_has_said_nothing_prints_no_activity(self):
        self.progress.beat()
        self.progress.doing("  ")
        self.progress.beat()
        self.assertEqual(self.lines, ["hl-progress 1", "hl-progress 2"])


class SilentClient:
    """Answers with one message and then nothing, as a model session that hung does."""

    def __init__(self, options):
        pass

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False

    async def query(self, prompt):
        pass

    async def receive_response(self):
        yield AssistantMessage(content=[TextBlock(text="thinking")], model="m")
        await asyncio.Event().wait()


class BeatInTheRunLoopTest(unittest.IsolatedAsyncioTestCase):
    async def test_the_run_loop_prints_markers_while_a_turn_is_alive_and_none_once_it_is_silent_too_long(self):
        printed = []
        beating = progress.Progress(quiet_limit_s=0.15, emit=printed.append)
        setup = SimpleNamespace(commands="usage: hl", workdir=tempfile.mkdtemp(), statedir=tempfile.mkdtemp(),
                                client=SimpleNamespace(get_signup=lambda *_: {}),
                                cfg=SimpleNamespace(agent_id="agent-a", role="researcher", api_url="http://x", token=AGENT_TOKEN,
                                                    platform_experiment_id="pe-1"))
        with mock.patch.object(run, "ClaudeSDKClient", SilentClient), \
                mock.patch.object(run, "_progress", beating), \
                mock.patch.object(progress, "INTERVAL_S", 0.02), \
                mock.patch.object(run.core, "stop_reason", lambda *a: None), \
                mock.patch.object(run.core, "install_signal_handler", lambda: [False]), \
                mock.patch.object(run, "_live_session_id", None), \
                mock.patch.dict(os.environ, {"CLAUDE_CONFIG_DIR": tempfile.mkdtemp()}), \
                contextlib.redirect_stdout(io.StringIO()):
            task = asyncio.ensure_future(run._run(setup))
            await asyncio.sleep(0.1)
            alive = len(printed)
            await asyncio.sleep(0.3)
            stopped_at = len(printed)
            await asyncio.sleep(0.2)
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await task
        self.assertGreaterEqual(alive, 2, "no marker while the turn was alive")
        self.assertEqual(len(printed), stopped_at, "markers kept coming from a turn silent past the limit")
        markers = [line for line in printed if line.startswith(progress.PREFIX + " ")]
        self.assertEqual(len(set(markers)), len(markers))
        # Beside each marker the loop says what the agent is at: a turn in flight is thinking.
        self.assertEqual({line for line in printed if line not in markers and not line.startswith(progress.STATE_PREFIX)},
                         {"hl-activity thinking"})
        self.assertRegex([line for line in printed if line.startswith(progress.STATE_PREFIX)][-1],
                         r"^hl-state subagents=0 turn=working since=\d+ instructions_digest=digest-1 version=1$")


class StreamingClient:
    """Answers with a thinking block, then a minute of stream events for one tool call."""

    def __init__(self, clock):
        self.clock = clock

    async def receive_response(self):
        self.clock.now += 100
        yield AssistantMessage(content=[TextBlock(text="thought")], model="m")
        for _ in range(6):
            self.clock.now += 10
            yield StreamEvent(uuid="u", session_id="s", event={"type": "content_block_delta"})
        yield AssistantMessage(content=[TextBlock(text="wrote")], model="m")


class StreamedCallTest(unittest.IsolatedAsyncioTestCase):
    # A model call that streams is still awaited until it answers: the slow call is said with its
    # whole length, and a turn that streams is no silence to the watchdog.
    async def test_stream_events_leave_the_call_awaited(self):
        clock = Clock()
        awaited = progress.Progress(quiet_limit_s=1000, clock=clock, emit=lambda line: None)
        waited = []
        with mock.patch.object(run, "_progress", awaited):
            async for message in run._receive_turn(StreamingClient(clock)):
                if isinstance(message, AssistantMessage):
                    waited.append(awaited.waited())
        self.assertEqual(waited, [100, 60])


if __name__ == "__main__":
    unittest.main()
