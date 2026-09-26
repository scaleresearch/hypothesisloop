import asyncio
import contextlib
import io
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

from claude_agent_sdk import AssistantMessage, TextBlock

from hypothesisloop_agent import progress
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
        setup = SimpleNamespace(system_prompt="", workdir=tempfile.mkdtemp(),
                                cfg=SimpleNamespace(agent_id="agent-a", flavor="generalist", api_url="http://x",
                                                    platform_experiment_id="pe-1", max_wall_hours=0))
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
        self.assertEqual(len(set(printed)), len(printed))


if __name__ == "__main__":
    unittest.main()
