import asyncio
import base64
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


ENV = {**os.environ, "HL_TOKEN": "hls_" + base64.urlsafe_b64encode(b"agent:pe-1/agent-a").decode().rstrip("=") + ".mac"}


def fake_hl(script: str, status: str = "", stats: str = "", quota: str = "", read_exit: int = 0) -> tuple[list[str], str]:
    """A fake hl. `hl watch` runs script once per call, with $RUNS counting the calls so far;
    `hl job status`, `hl job stats` and `hl quota` print status, stats and quota, record their
    arguments and exit read_exit. A new one has had no outage."""
    updates._unreachable_since = None
    directory = tempfile.mkdtemp()
    path = os.path.join(directory, "hl")
    for name, text in (("status", status), ("stats", stats), ("quota", quota)):
        with open(f"{path}.{name}", "w") as f:
            f.write(text)
    with open(path, "w") as f:
        f.write("#!/bin/sh\n[ \"$1\" = job ] && { echo \"$*\" >> \"$0.reads\"; cat \"$0.$2\"; exit %d; }\n"
                "[ \"$1\" = quota ] && { echo \"$*\" >> \"$0.reads\"; cat \"$0.quota\"; exit %d; }\n" % (read_exit, read_exit) +
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
        events = await updates.next_batch(ENV, lambda: False, 10, command=command)
        self.assertEqual([e["kind"] for e in events], ["experiment.status", "comment.new"])

    async def test_an_empty_window_returns_nothing_and_watches_again(self):
        command, counter = fake_hl("exit 0\n")
        events = await updates.next_batch(ENV, lambda: runs(counter) > 1, 60, command=command)
        self.assertEqual(events, [])
        self.assertGreater(runs(counter), 1)
        self.assertEqual(await updates.next_batch(ENV, lambda: False, 0, command=command), [])

    # A coordinator woken for every job's outcome and again for its finding made 129 model
    # calls where 35 had done.
    async def test_only_the_roles_waking_kinds_end_the_wait_and_an_idle_one_is_handed_what_waited(self):
        command, counter = fake_hl(f"echo \"$*\" >> \"$0.args\"\n[ \"$2\" = --no-wait ] && echo '{COMMENT}'\nexit 0\n")
        events = await updates.next_batch(ENV, lambda: False, 0.2, command=command)
        self.assertEqual([e["kind"] for e in events], ["comment.new"])
        with open(counter.replace(".runs", ".args")) as f:
            asked = f.read().splitlines()
        self.assertEqual(asked[0], "watch --wake " + ",".join(updates.WAKES["agent"]))
        self.assertEqual(asked[-1], "watch --no-wait --timeout 5")
        self.assertIn("agent.finished", updates.WAKES["coordinator"])
        # A job that waited an hour for cores no node had told neither its agent nor the coordinator.
        for role in ("agent", "coordinator"):
            self.assertIn("experiment.blocked", updates.WAKES[role])
        self.assertIn("job.held", updates.WAKES["agent"])
        # A coordinator was woken for each job a researcher's missing summary held, which only
        # the researcher clears.
        self.assertIn("job.held!=summary_gate", updates.WAKES["coordinator"])
        self.assertNotIn("job.held", updates.WAKES["coordinator"])
        self.assertFalse([wake for wake in updates.WAKES["coordinator"] if wake.startswith("experiment.status")])
        self.assertNotIn("finding.new", updates.WAKES["coordinator"] + updates.WAKES["agent"])

    # Three baseline jobs failed, one of them for the image the brief named, and the coordinator
    # heard of none until the agent gave up. Woken by every job that ended, it was woken by the
    # one that completed too, before the agent's finish it has to wait for.
    async def test_a_coordinator_of_an_open_experiment_is_woken_by_a_job_that_failed_and_of_a_running_one_is_not(self):
        coordinator = {**os.environ, "HL_TOKEN": "hls_" + base64.urlsafe_b64encode(b"coordinator:pe-1").decode().rstrip("=") + ".mac"}
        for status, wakes in (("open", updates.WAKES["coordinator"] + ("experiment.status=FAILED",)), ("running", updates.WAKES["coordinator"])):
            command, counter = fake_hl('[ "$1" = platform-experiments ] && { echo \'{\n "status": "%s"\n}\'; exit 0; }\n'
                                       'echo "$*" >> "$0.args"\nexit 0\n' % status)
            await updates.next_batch(coordinator, lambda: False, 0.2, command=command)
            with open(counter.replace(".runs", ".args")) as f:
                self.assertEqual(f.read().splitlines()[0], "watch --wake " + ",".join(wakes))

    # A platform experiment that could not be read was taken as running, so a coordinator of an
    # open one waited without the wake for a failed job and nothing said so.
    async def test_a_wait_that_cannot_be_set_up_is_a_poll_that_failed(self):
        coordinator = {**os.environ, "HL_TOKEN": "hls_" + base64.urlsafe_b64encode(b"coordinator:pe-1").decode().rstrip("=") + ".mac"}
        refused, counter = fake_hl('[ "$1" = platform-experiments ] && { [ -e "$0.args" ] || exit 1; echo \'{"status": "open"}\'; exit 0; }\n'
                                   'echo "$*" >> "$0.args"\nexit 0\n')
        events = await updates.next_batch(coordinator, lambda: False, 10, command=refused, retry_s=0.1)
        self.assertEqual([e["kind"] for e in events], ["platform.reachable"])
        with open(counter.replace(".runs", ".args")) as f:
            self.assertEqual(f.read().splitlines(), ["watch --no-wait --timeout 5"])

    async def test_a_failed_watch_is_retried(self):
        command, counter = fake_hl(f"[ $RUNS -eq 1 ] && {{ echo refused >&2; exit 2; }}\necho '{COMPLETED}'\n")
        events = await updates.next_batch(ENV, lambda: False, 10, command=command, retry_s=0.1)
        self.assertEqual([e.get("subject") for e in events], ["exp-1", None])
        self.assertEqual(runs(counter), 2)

    # In an outage researchers with no job in flight retried in sleep loops: one that ended its
    # turn would have heard nothing until the idle nudge, half an hour later.
    async def test_the_first_poll_answered_after_a_failed_one_wakes_with_the_outage(self):
        command, counter = fake_hl('echo "$*" >> "$0.args"\n[ $RUNS -le 2 ] && { echo unreachable >&2; exit 1; }\nexit 0\n')
        events = await updates.next_batch(ENV, lambda: False, 10, command=command, retry_s=0.1)
        self.assertRegex(updates.message(events, "agent-a"), r"^Platform updates:\nplatform\.reachable again: it could not be reached "
                         r"from \d\d:\d\d:\d\d to \d\d:\d\d:\d\d UTC\. Retry what was refused then\.$")
        self.assertEqual(updates._clock(3725.0), "01:02:05")
        with open(counter.replace(".runs", ".args")) as f:
            asked = f.read().splitlines()
        # While it fails the poll waits for no event, so the first answer ends the wait.
        self.assertEqual([line.split()[1] for line in asked], ["--wake", "--no-wait", "--no-wait"])
        self.assertEqual(await updates.next_batch(ENV, lambda: False, 0.2, command=command), [])

    # The poll after the refused tool call fails too, and the agent then ends its turn.
    async def test_an_outage_seen_by_a_drain_is_ended_by_the_wait_that_follows(self):
        command, _ = fake_hl("[ $RUNS -eq 1 ] && exit 1\nexit 0\n")
        self.assertEqual(await updates.drain(ENV, command=command), [])
        events = await updates.next_batch(ENV, lambda: False, 10, command=command)
        self.assertEqual([e["kind"] for e in events], ["platform.reachable"])
        self.assertEqual(await updates.drain(ENV, command=command), [])

    async def test_stopping_ends_a_watch_in_progress(self):
        command, _ = fake_hl("exec sleep 30\n")
        stop_at = time.monotonic() + 0.3
        started = time.monotonic()
        events = await asyncio.wait_for(
            updates.next_batch(ENV, lambda: time.monotonic() > stop_at, 60, command=command), timeout=10)
        self.assertEqual(events, [])
        self.assertLess(time.monotonic() - started, 5)


class JobStateTest(unittest.IsolatedAsyncioTestCase):
    JOBS = ('{"id":"exp-1","status":"COMPLETED","metrics":[{"name":"acc","value":0.9,"basis":"raw"},'
            '{"name":"loss","value":0.2608416958176093,"basis":"per_token"}],"ran_on":{"node":"rtx4090-0","machine":'
            '{"instance_id":"54493630","host_id":"16358","accelerator_model":"RTX 4090","cpu_model":"AMD Ryzen"}}}\n'
            '{"id":"exp-3","status":"EVICTED","ran_on":{"node":"blackhole-1"}}\n'
            '{"id":"exp-2","status":"COMPLETED","metrics":[{"name":"acc","value":0.95,"basis":"raw"}],"rankable":false,'
            '"not_rankable_reason":"constraint exact_match is 0.5, and must be at least 1"}\n')
    STATS = '{"total":9,"by_status":{"COMPLETED":6,"RUNNING":2,"QUEUED":1}}\n'
    QUOTA = ('{"agent_id":"agent-a","share_accelerator_hours":2,"remaining_guaranteed_accelerator_hours":1.85,"remaining_burst_accelerator_hours":1.85,'
             '"guaranteed_accelerator_hours":2,"burst_accelerator_hours":4,'
             '"used_guaranteed_acch":0.11336604166666665,"used_burst_acch":0}\n')

    async def test_ended_jobs_arrive_with_their_metrics_and_what_is_unfinished_from_one_read_each(self):
        second = COMPLETED.replace("exp-1", "exp-2").replace(":5", ":7")
        evicted = COMPLETED.replace("exp-1", "exp-3").replace("COMPLETED", "EVICTED").replace('"detail":""', '"detail":"oom"')
        command, counter = fake_hl("\n".join(f"echo '{e}'" for e in (COMPLETED, COMMENT, second, evicted, COMPLETED)) + "\n",
                                   self.JOBS, self.STATS, self.QUOTA)
        events = await updates.next_batch(ENV, lambda: False, 10, command=command)
        self.assertEqual(updates.message(events, "agent-a"), "Platform updates:\n"
                         "experiment.status exp-1 COMPLETED acc 0.9, loss 0.260842 (per_token) "
                         "on host 16358 RTX 4090\n"
                         "comment.new hyp-1 agent by agent-b\n"
                         "experiment.status exp-2 COMPLETED acc 0.95, "
                         "ranks nowhere: constraint exact_match is 0.5, and must be at least 1\n"
                         "experiment.status exp-3 EVICTED oom on node blackhole-1\n"
                         "unfinished_jobs 1 QUEUED, 2 RUNNING\n"
                         "quota_used 0.113 of 2 AccH, room for a job of 1.85 guaranteed or 1.85 burst")
        with open(counter.replace(".runs", ".reads")) as f:
            self.assertEqual(f.read(), "job status exp-1 exp-2 exp-3\njob stats --agent agent-a\nquota\n")

    async def test_the_last_job_ending_says_none_is_unfinished(self):
        command, _ = fake_hl(f"echo '{COMPLETED}'\n", self.JOBS, '{"total":1,"by_status":{"COMPLETED":1}}\n')
        events = await updates.drain(ENV, command=command)
        self.assertEqual(updates.message(events, "agent-a").splitlines()[-1], "unfinished_jobs none")

    async def test_a_share_that_a_later_stage_raises_says_to_what_and_when(self):
        quota = self.QUOTA.replace('"used_burst_acch":0', '"used_burst_acch":0,"next_stage_starts_at":"2026-10-06T21:43:44Z",'
                                                          '"next_stage_share_accelerator_hours":2.829331')
        command, _ = fake_hl(f"echo '{COMPLETED}'\n", self.JOBS, self.STATS, quota)
        events = await updates.drain(ENV, command=command)
        self.assertEqual(updates.message(events, "agent-a").splitlines()[-1],
                         "quota_used 0.113 of 2 AccH, room for a job of 1.85 guaranteed or 1.85 burst, "
                         "and the share rises to 2.83 AccH by 2026-10-06T21:43:44Z")

    async def test_a_batch_without_an_ended_job_reads_no_job(self):
        command, counter = fake_hl(f"echo '{COMMENT}'\n", self.JOBS, self.STATS)
        events = await updates.drain(ENV, command=command)
        self.assertEqual([e["kind"] for e in events], ["comment.new"])
        self.assertFalse(os.path.exists(counter.replace(".runs", ".reads")))

    async def test_a_job_that_could_not_be_read_keeps_its_update(self):
        command, _ = fake_hl(f"echo '{COMPLETED}'\n")
        events = await updates.drain(ENV, command=command)
        self.assertEqual(updates.message(events, "agent-a"), "Platform updates:\nexperiment.status exp-1 COMPLETED")

    # The exit code of each read was ignored, so what a failed one had printed was taken for the
    # platform's answer.
    async def test_a_read_that_failed_is_logged_and_adds_nothing_to_the_batch(self):
        command, _ = fake_hl(f"echo '{COMPLETED}'\n", self.JOBS, self.STATS, self.QUOTA, read_exit=1)
        with self.assertLogs("llm_agent", "WARNING") as logged:
            events = await updates.drain(ENV, command=command)
        self.assertEqual(updates.message(events, "agent-a"), "Platform updates:\nexperiment.status exp-1 COMPLETED")
        self.assertEqual(len(logged.records), 3)


class DrainTest(unittest.IsolatedAsyncioTestCase):
    async def test_a_drain_returns_what_waits_without_listening_for_more(self):
        command, _ = fake_hl(f"[ \"$2\" = '--no-wait' ] || exit 2\necho '{COMPLETED}'\n")
        events = await updates.drain(ENV, command=command)
        self.assertEqual([e["subject"] for e in events], ["exp-1"])

    async def test_a_failed_drain_returns_nothing(self):
        command, _ = fake_hl("echo refused >&2; exit 2\n")
        self.assertEqual(await updates.drain(ENV, command=command), [])

    async def test_an_outage_is_logged_by_the_first_failed_drain_only(self):
        command, _ = fake_hl("echo never connected >&2; exit 1\n")
        with self.assertLogs("llm_agent", "WARNING") as logged:
            for _ in range(3):
                await updates.drain(ENV, command=command)
        self.assertEqual(len(logged.records), 1)


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
        from types import SimpleNamespace

        self.draft = {"status": "draft", "status_reason": {"reason": "proposed"}, "revisions": [{"message": "Make it fit one accelerator."}]}
        self.cfg = SimpleNamespace(agent_id="agent-a", flavor="generalist", role="competitor")
        self.setup = SimpleNamespace(cfg=self.cfg, client=SimpleNamespace(get_platform_experiment=lambda _id: self.draft),
                                     workdir=tempfile.mkdtemp())
        self.cfg.platform_experiment_id = "pe-1"
        self.hook = run._updates_hook(self.setup, {})

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

    # A planner kept on its machine after its answer is told a revision by an event that only
    # points at the draft. It is handed what the person asked, and the same event replayed once
    # the planner has answered is handed as nothing.
    async def test_a_revision_reaches_a_planner_as_what_was_asked_and_an_answered_one_as_nothing(self):
        from hypothesisloop_agent import config

        revision = {"kind": updates.REVISION, "subject": "pe-1", "value": ""}
        self.cfg.flavor = config.PLANNER_FLAVOR
        self.waiting = [revision]
        self.assertEqual(await self.call(), {})

        self.draft["status_reason"] = {"reason": "revising"}
        self.waiting = [revision, {"kind": "comment.new", "subject": "hyp-1", "value": "human"}]
        told = (await self.call())["hookSpecificOutput"]["additionalContext"]
        self.assertTrue(told.startswith("The person read your answer and asks for this revision:\n\nMake it fit one accelerator.\n\n"), told)
        self.assertTrue(told.endswith("Platform updates:\ncomment.new hyp-1 human"), told)
        self.assertNotIn("platform_experiment.revision", told)
        self.assertNotIn("gone", told)


# A coordinator sent a researcher a message with the CLI's SendMessage, was answered "No agent
# named ... Spawn a new one or use the agent ID", and sent it again under the agent's id.
class UndeliveredMessageTest(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        from hypothesisloop_agent.claude import run

        self.harness = run

    async def call(self, **hook_input):
        return await self.harness._undelivered_hook({"tool_name": "SendMessage", **hook_input}, "tool-1", {"signal": None})

    async def test_a_message_that_reached_nobody_is_answered_with_the_update_a_comment_arrives_in(self):
        for event, said in (("PostToolUse", {"tool_response": {"success": False, "message": "No agent named 'ag-1'"}}),
                            ("PostToolUseFailure", {"error": "No agent named 'ag-1' is reachable."})):
            told = (await self.call(hook_event_name=event, **said))["hookSpecificOutput"]
            self.assertEqual(told["hookEventName"], event)
            self.assertIn("send none again", told["additionalContext"])
            self.assertIn("`hl hypothesis comment ID --text T` on a hypothesis of that agent: its owner and the "
                          "coordinator get it as `comment.new` in their next update", told["additionalContext"])

    async def test_a_message_its_own_subagent_got_is_left_alone(self):
        self.assertEqual(await self.call(hook_event_name="PostToolUse", tool_response={"success": True}), {})

    def test_the_hook_listens_to_send_message_alone(self):
        self.assertEqual(self.harness._UNDELIVERED_MATCHER.matcher, "SendMessage")


class FormatTest(unittest.TestCase):
    def test_one_compact_line_per_event_naming_only_other_actors(self):
        events = [{"kind": "experiment.status", "subject": "exp-1", "value": "COMPLETED", "detail": "",
                   "agent_id": "agent-a", "position": 5, "cursor": 5},
                  {"kind": "hypothesis.link", "subject": "hyp-2", "value": "builds_on", "detail": "hyp-1",
                   "agent_id": "agent-b", "position": 6, "cursor": 6}]
        self.assertEqual(updates.message(events, "agent-a"),
                         "Platform updates:\nexperiment.status exp-1 COMPLETED\nhypothesis.link hyp-2 builds_on hyp-1 by agent-b")

    def test_a_held_or_blocked_job_is_one_line_with_the_job_the_code_and_the_sentence(self):
        sentence = "cluster k3s-1 is short of 8 cores, held by 2 jobs of this platform experiment and 1 of others"
        held = {"kind": "job.held", "subject": "exp-1", "value": "capacity_unavailable", "detail": sentence,
                "agent_id": "agent-a", "position": 7, "cursor": 7}
        blocked = {"kind": "experiment.blocked", "subject": "exp-2", "value": "no_cluster_for_image",
                   "detail": "no cluster runs image torch", "agent_id": "agent-a", "position": 8, "cursor": 8}
        self.assertEqual(updates.message([held, blocked], "agent-a"), "Platform updates:\n"
                         f"job.held exp-1 capacity_unavailable {sentence}\n"
                         "experiment.blocked exp-2 no_cluster_for_image no cluster runs image torch")
        self.assertEqual(updates.event_line(held, ""), f"job.held exp-1 capacity_unavailable {sentence} by agent-a")

    def test_an_evicted_or_requeued_job_is_one_line_with_the_reason_code_and_its_sentence(self):
        evicted = {"kind": "experiment.status", "subject": "exp-1", "value": "EVICTED", "agent_id": "agent-a",
                   "detail": "no_offer: no machine to rent for shape rtx4090 in 3m0s of asking"}
        requeued = {"kind": "experiment.status", "subject": "exp-2", "value": "QUEUED", "agent_id": "agent-a",
                    "detail": "code_unreachable: command exited with code 75: hl-clone: could not fetch the code"}
        self.assertEqual(updates.message([evicted, requeued], "agent-a"), "Platform updates:\n"
                         "experiment.status exp-1 EVICTED no_offer: no machine to rent for shape rtx4090 in 3m0s of asking\n"
                         "experiment.status exp-2 QUEUED code_unreachable: command exited with code 75: hl-clone: could not fetch the code")

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

        setup = SimpleNamespace(system_prompt="", workdir=tempfile.mkdtemp(), statedir=tempfile.mkdtemp(),
                                client=SimpleNamespace(get_platform_experiment=lambda _id: {"status": "running"}, get_signup=lambda *_: {}),
                                cfg=SimpleNamespace(agent_id="agent-a", flavor="generalist", api_url="http://x", token=ENV["HL_TOKEN"], platform_experiment_id="pe-1",
                                                    max_wall_hours=0))
        with mock.patch.object(run, "ClaudeSDKClient", Client), \
                mock.patch.object(updates, "next_batch", one_batch), \
                mock.patch.object(updates, "drain", mock.AsyncMock(return_value=[])), \
                redirect_stdout(StringIO()) as out:
            await asyncio.wait_for(run._run_one_session(setup, "m", None, time.time(), stop_flag), timeout=5)
        self.assertEqual(sent, ["Begin.", "Platform updates:\nexperiment.status exp-1 COMPLETED", run._shutdown_prompt(setup)])
        self.assertIn("agent-a event experiment.status exp-1 COMPLETED", out.getvalue())
        self.assertEqual(os.listdir(setup.workdir), [], "the harness wrote into the directory the agent clones into")
        self.assertIn(".claude_session_id", os.listdir(setup.statedir))

    # A planner whose machine is kept after its answer waits like any agent whose turn ended. A
    # revision is its next update and reaches the session it already has. One it answered before,
    # which a restored cursor replays, starts no turn.
    async def test_a_revision_is_the_next_turn_of_a_planner_kept_after_its_answer(self):
        from contextlib import redirect_stdout
        from io import StringIO
        from types import SimpleNamespace
        from unittest import mock

        from claude_agent_sdk import ResultMessage

        from hypothesisloop_agent import config
        from hypothesisloop_agent.claude import run

        stop_flag = [False]
        sent = []
        draft = {"status": "draft", "status_reason": {"reason": "proposed"}, "revisions": [{"message": "Make it fit one accelerator."}]}
        batches = 0

        class Client:
            def __init__(self, options):
                pass

            async def __aenter__(self):
                return self

            async def __aexit__(self, *exc):
                return False

            async def query(self, prompt):
                sent.append(prompt)

            async def receive_response(self):
                yield ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=False,
                                    num_turns=1, session_id="s")

        async def revised(env, stopped, idle_s):
            # The first batch replays a revision answered already, the second is one asked now.
            nonlocal batches
            batches += 1
            if batches == 2:
                draft["status_reason"] = {"reason": "revising"}
            if batches == 3:
                stop_flag[0] = True
                return []
            return [{"kind": updates.REVISION, "subject": "pe-1", "value": ""}]

        setup = SimpleNamespace(system_prompt="", workdir=tempfile.mkdtemp(), statedir=tempfile.mkdtemp(),
                                client=SimpleNamespace(get_platform_experiment=lambda _id: draft, get_signup=lambda *_: {}),
                                cfg=SimpleNamespace(agent_id="agent-a", flavor=config.PLANNER_FLAVOR, role="planner", api_url="http://x",
                                                    token=ENV["HL_TOKEN"], platform_experiment_id="pe-1", max_wall_hours=0))
        with mock.patch.object(run, "ClaudeSDKClient", Client), \
                mock.patch.object(updates, "next_batch", revised), \
                mock.patch.object(updates, "drain", mock.AsyncMock(return_value=[])), \
                redirect_stdout(StringIO()):
            await asyncio.wait_for(run._run_one_session(setup, "m", None, time.time(), stop_flag), timeout=5)
        self.assertEqual(batches, 3)
        self.assertEqual(sent[0], "Begin.")
        self.assertEqual(sent[1], "The person read your answer and asks for this revision:\n\nMake it fit one accelerator.\n\n"
                                  "Read the draft and the capacity again and answer with a new proposal.")
