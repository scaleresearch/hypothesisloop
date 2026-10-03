import asyncio
import contextlib
import io
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

from claude_agent_sdk import AssistantMessage, TextBlock

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
        self.clock, self.lines = Clock(), []
        self.progress = progress.Progress(quiet_limit_s=100, clock=self.clock, emit=self.lines.append)

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
        setup = SimpleNamespace(system_prompt="", workdir=tempfile.mkdtemp(), statedir=tempfile.mkdtemp(),
                                client=SimpleNamespace(get_signup=lambda *_: {}),
                                cfg=SimpleNamespace(agent_id="agent-a", flavor="generalist", api_url="http://x", token=AGENT_TOKEN,
                                                    platform_experiment_id="pe-1"))
        with mock.patch.object(run, "ClaudeSDKClient", SilentClient), \
                mock.patch.object(run, "_progress", beating), \
                mock.patch.object(progress, "INTERVAL_S", 0.02), \
                mock.patch.object(run.core, "stop_reason", lambda *a: None), \
                mock.patch.object(run.core, "install_signal_handler", lambda: [False]), \
                contextlib.redirect_stdout(io.StringIO()):
            task = asyncio.ensure_future(run._run(setup, "m"))
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
        self.assertEqual({line for line in printed if line not in markers}, {"hl-activity thinking"})


if __name__ == "__main__":
    unittest.main()
