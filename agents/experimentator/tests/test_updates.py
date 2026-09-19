import asyncio
import os
import stat
import tempfile
import time
import unittest
from datetime import datetime, timezone

from hypothesisloop_agent import updates
from hypothesisloop_agent.claude import activity

COMPLETED = '{"kind":"experiment.status","subject":"exp-1","value":"COMPLETED","detail":"","agent_id":"agent-a","position":5,"cursor":5}'
COMMENT = '{"kind":"comment.new","subject":"hyp-1","value":"agent","detail":"","agent_id":"agent-b","position":6,"cursor":6}'


def fake_hl(script: str, jobs: str = "") -> tuple[list[str], str]:
    """A fake hl. `hl watch` runs script once per call, with $RUNS counting the calls so far;
    `hl job status` prints jobs and records its arguments."""
    directory = tempfile.mkdtemp()
    path = os.path.join(directory, "hl")
    with open(path + ".jobs", "w") as f:
        f.write(jobs)
    with open(path, "w") as f:
        f.write("#!/bin/sh\n[ \"$1\" = job ] && { echo \"$*\" >> \"$0.reads\"; cat \"$0.jobs\"; exit 0; }\n"
                "count=$(cat \"$0.runs\" 2>/dev/null || echo 0)\nRUNS=$((count + 1))\n"
                "echo $RUNS > \"$0.runs\"\n" + script)
    os.chmod(path, os.stat(path).st_mode | stat.S_IEXEC)
    return [path, "watch"], path + ".runs"


def runs(counter: str) -> int:
    try:
        with open(counter) as f:
            return int(f.read())
    except (FileNotFoundError, ValueError):
        return 0


class NextBatchTest(unittest.IsolatedAsyncioTestCase):
    async def test_a_batch_is_returned_as_events(self):
        command, _ = fake_hl(f"echo 'hl watch: connected' >&2\necho '{COMPLETED}'\necho '{COMMENT}'\n")
        events = await updates.next_batch(os.environ.copy(), lambda: False, 10, command=command)
        self.assertEqual([e["kind"] for e in events], ["experiment.status", "comment.new"])

    async def test_an_empty_window_returns_nothing_and_watches_again(self):
        command, counter = fake_hl("exit 0\n")
        events = await updates.next_batch(os.environ.copy(), lambda: runs(counter) > 1, 60, command=command)
        self.assertEqual(events, [])
        self.assertGreater(runs(counter), 1)
        self.assertEqual(await updates.next_batch(os.environ.copy(), lambda: False, 0, command=command), [])

    async def test_a_failed_watch_is_retried(self):
        command, counter = fake_hl(f"[ $RUNS -eq 1 ] && {{ echo refused >&2; exit 2; }}\necho '{COMPLETED}'\n")
        events = await updates.next_batch(os.environ.copy(), lambda: False, 10, command=command, retry_s=0.1)
        self.assertEqual([e["subject"] for e in events], ["exp-1"])
        self.assertEqual(runs(counter), 2)

    async def test_stopping_ends_a_watch_in_progress(self):
        command, _ = fake_hl("exec sleep 30\n")
        stop_at = time.monotonic() + 0.3
        started = time.monotonic()
        events = await asyncio.wait_for(
            updates.next_batch(os.environ.copy(), lambda: time.monotonic() > stop_at, 60, command=command), timeout=10)
        self.assertEqual(events, [])
        self.assertLess(time.monotonic() - started, 5)


class MetricsTest(unittest.IsolatedAsyncioTestCase):
    JOBS = ('{"id":"exp-1","status":"COMPLETED","metrics":[{"name":"acc","best":0.9,"last":0.85,"basis":"raw"},'
            '{"name":"loss","best":0.2608416958176093,"last":0.31,"basis":"per_token"}]}\n'
            '{"id":"exp-2","status":"COMPLETED"}\n')

    async def test_completed_jobs_arrive_with_their_metrics_from_one_read(self):
        second = COMPLETED.replace("exp-1", "exp-2").replace(":5", ":7")
        command, counter = fake_hl(f"echo '{COMPLETED}'\necho '{COMMENT}'\necho '{second}'\necho '{COMPLETED}'\n", self.JOBS)
        events = await updates.next_batch(os.environ.copy(), lambda: False, 10, command=command)
        self.assertEqual(updates.message(events, "agent-a"), "Platform updates:\n"
                         "experiment.status exp-1 COMPLETED acc best 0.9 last 0.85, loss best 0.260842 last 0.31 (per_token)\n"
                         "comment.new hyp-1 agent by agent-b\n"
                         "experiment.status exp-2 COMPLETED")
        with open(counter.replace(".runs", ".reads")) as f:
            self.assertEqual(f.read(), "job status exp-1 exp-2\n")

    async def test_a_batch_without_a_completed_job_reads_no_job(self):
        command, counter = fake_hl(f"echo '{COMMENT}'\n", self.JOBS)
        events = await updates.drain(os.environ.copy(), command=command)
        self.assertEqual([e["kind"] for e in events], ["comment.new"])
        self.assertFalse(os.path.exists(counter.replace(".runs", ".reads")))

    async def test_a_job_that_could_not_be_read_keeps_its_update(self):
        command, _ = fake_hl(f"echo '{COMPLETED}'\n")
        events = await updates.drain(os.environ.copy(), command=command)
        self.assertEqual(updates.message(events, "agent-a"), "Platform updates:\nexperiment.status exp-1 COMPLETED")


class DrainTest(unittest.IsolatedAsyncioTestCase):
    async def test_a_drain_returns_what_waits_without_listening_for_more(self):
        command, _ = fake_hl(f"[ \"$2\" = '--no-wait' ] || exit 2\necho '{COMPLETED}'\n")
        events = await updates.drain(os.environ.copy(), command=command)
        self.assertEqual([e["subject"] for e in events], ["exp-1"])

    async def test_a_failed_drain_returns_nothing(self):
        command, _ = fake_hl("echo refused >&2; exit 2\n")
        self.assertEqual(await updates.drain(os.environ.copy(), command=command), [])


class HookTest(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        from unittest import mock

        from hypothesisloop_agent.claude import run

        self.waiting = []
        self.drains = 0

        async def drain(env):
            self.drains += 1
            await asyncio.sleep(0)
            events, self.waiting = self.waiting, []
            return events

        patch = mock.patch.object(updates, "drain", drain)
        patch.start()
        self.addCleanup(patch.stop)
        self.hook = run._updates_hook({}, "agent-a")

    async def call(self, **hook_input):
        from contextlib import redirect_stdout
        from io import StringIO

        with redirect_stdout(StringIO()):
            return await self.hook({"hook_event_name": "PostToolUse", **hook_input}, "tool-1", {"signal": None})

    async def test_waiting_updates_reach_the_agent_once_with_repeats_collapsed(self):
        event = {"kind": "job.underperforming", "subject": "job-1", "value": "val_accuracy", "agent_id": "agent-a"}
        self.waiting = [event, dict(event, position=2)]
        self.assertEqual(await self.call(), {"hookSpecificOutput": {
            "hookEventName": "PostToolUse",
            "additionalContext": "Platform updates:\njob.underperforming job-1 val_accuracy"}})
        self.assertEqual(await self.call(), {})

    async def test_parallel_tool_calls_share_one_drain(self):
        self.waiting = [{"kind": "comment.new", "subject": "hyp-1", "value": "human"}]
        outputs = await asyncio.gather(self.call(), self.call(), self.call())
        self.assertEqual(self.drains, 1)
        self.assertEqual([bool(o) for o in outputs], [True, False, False])

    async def test_a_subagents_tool_call_leaves_the_updates_waiting(self):
        self.waiting = [{"kind": "comment.new", "subject": "hyp-1", "value": "human"}]
        self.assertEqual(await self.call(agent_id="sub-1"), {})
        self.assertEqual(self.drains, 0)


class FormatTest(unittest.TestCase):
    def test_one_compact_line_per_event_naming_only_other_actors(self):
        events = [{"kind": "experiment.status", "subject": "exp-1", "value": "COMPLETED", "detail": "",
                   "agent_id": "agent-a", "position": 5, "cursor": 5},
                  {"kind": "hypothesis.link", "subject": "hyp-2", "value": "builds_on", "detail": "hyp-1",
                   "agent_id": "agent-b", "position": 6, "cursor": 6}]
        self.assertEqual(updates.message(events, "agent-a"),
                         "Platform updates:\nexperiment.status exp-1 COMPLETED\nhypothesis.link hyp-2 builds_on hyp-1 by agent-b")

    def test_a_repeated_event_is_delivered_once(self):
        event = {"kind": "job.underperforming", "subject": "job-1", "value": "val_accuracy", "agent_id": "agent-a"}
        self.assertEqual(updates.message([event, dict(event, position=2), dict(event, position=3)], "agent-a"),
                         "Platform updates:\njob.underperforming job-1 val_accuracy")

    def test_activity_logs_each_delivered_event(self):
        now = datetime(2026, 10, 2, 6, 21, 30, tzinfo=timezone.utc)
        events = [{"kind": "comment.new", "subject": "hyp-1", "value": "human"}]
        self.assertEqual(activity.event_lines(events, "agent-a", now),
                         ["2026-10-02T06:21:30Z agent-a event comment.new hyp-1 human"])


if __name__ == "__main__":
    unittest.main()


class SessionDeliveryTest(unittest.IsolatedAsyncioTestCase):
    async def test_a_turn_end_hands_the_next_batch_to_the_session_and_a_stop_during_it_ends_the_session_after_one_last_turn(self):
        from contextlib import redirect_stdout
        from io import StringIO
        from types import SimpleNamespace
        from unittest import mock

        from claude_agent_sdk import ResultMessage

        from hypothesisloop_agent.claude import run

        stop_flag = [False]
        sent = []

        class Client:
            def __init__(self, options):
                pass

            async def __aenter__(self):
                return self

            async def __aexit__(self, *exc):
                return False

            async def query(self, prompt):
                sent.append(prompt)
                if len(sent) == 2:
                    stop_flag[0] = True

            async def receive_response(self):
                yield ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=False,
                                    num_turns=1, session_id="s")

        async def one_batch(env, stopped, idle_s):
            return [{"kind": "experiment.status", "subject": "exp-1", "value": "COMPLETED", "agent_id": "agent-a"}]

        setup = SimpleNamespace(system_prompt="", workdir=tempfile.mkdtemp(),
                                client=SimpleNamespace(get_platform_experiment=lambda _id: {"status": "running"}),
                                cfg=SimpleNamespace(agent_id="agent-a", api_url="http://x", platform_experiment_id="pe-1",
                                                    max_wall_hours=0))
        with mock.patch.object(run, "ClaudeSDKClient", Client), \
                mock.patch.object(updates, "next_batch", one_batch), \
                mock.patch.object(updates, "drain", mock.AsyncMock(return_value=[])), \
                redirect_stdout(StringIO()) as out:
            await asyncio.wait_for(run._run_one_session(setup, "m", None, time.time(), stop_flag), timeout=5)
        self.assertEqual(sent, ["Begin.", "Platform updates:\nexperiment.status exp-1 COMPLETED", run._SHUTDOWN_PROMPT])
        self.assertIn("agent-a event experiment.status exp-1 COMPLETED", out.getvalue())
