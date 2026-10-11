import asyncio
import base64
import os
import stat
import tempfile
import time
import unittest

from conftest import SITUATION
from hypothesisloop_agent import outage, updates

# The read itself, which conftest stubs for every test.
REAL_SITUATION = updates.situation
REAL_STANDING = updates.standing
S = "\n\n" + SITUATION

COMPLETED = '{"kind":"experiment.status","subject":"exp-1","value":"COMPLETED","detail":"","agent_id":"agent-a","position":5,"cursor":5}'
COMMENT = '{"kind":"comment.new","subject":"hyp-1","value":"agent","detail":"","agent_id":"agent-b","position":6,"cursor":6}'


ENV = {**os.environ, "HL_TOKEN": "hls_" + base64.urlsafe_b64encode(b"agent:pe-1/agent-a").decode().rstrip("=") + ".mac"}


def fake_hl(script: str, status: str = "", stats: str = "", quota: str = "", read_exit: int = 0, quota_refusal: str = "") -> tuple[list[str], str]:
    """A fake hl. `hl watch` runs script once per call, with $RUNS counting the calls so far;
    `hl job status`, `hl job stats` and `hl quota` print status, stats and quota, record their
    arguments and exit read_exit. With quota_refusal, `hl quota` says it on stderr and exits 1.
    A new one has had no outage."""
    outage._since, outage._over, outage._suspected = None, None, False
    directory = tempfile.mkdtemp()
    path = os.path.join(directory, "hl")
    for name, text in (("status", status), ("stats", stats), ("quota", quota), ("refusal", quota_refusal)):
        with open(f"{path}.{name}", "w") as f:
            f.write(text)
    with open(path, "w") as f:
        f.write("#!/bin/sh\n[ \"$1\" = job ] && { echo \"$*\" >> \"$0.reads\"; cat \"$0.$2\"; exit %d; }\n"
                "[ \"$1\" = quota ] && { echo \"$*\" >> \"$0.reads\"; [ -s \"$0.refusal\" ] && { cat \"$0.refusal\" >&2; exit 1; }; "
                "cat \"$0.quota\"; exit %d; }\n" % (read_exit, read_exit) +
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
        # A researcher worked 28 minutes with nothing on the platform and nobody was woken by it.
        # The notice repeats while the stretch lasts: it wakes the agent each time, and the
        # coordinator for the first, the repeats riding along with its next batch.
        self.assertIn("agent.idle", updates.WAKES["agent"])
        self.assertIn("agent.idle=new", updates.WAKES["coordinator"])
        self.assertNotIn("agent.idle", updates.WAKES["coordinator"])
        # A job that waited an hour for cores no node had told neither its agent nor the coordinator.
        for role in ("agent", "coordinator"):
            self.assertIn("experiment.blocked", updates.WAKES[role])
        for role in ("agent", "coordinator"):
            self.assertIn("job.held", updates.WAKES[role])
        self.assertFalse([wake for wake in updates.WAKES["coordinator"] if wake.startswith("experiment.status")])
        self.assertNotIn("finding.new", updates.WAKES["coordinator"] + updates.WAKES["agent"])
        # An idea a person posted wakes a coordinator; one of an agent or of itself does not.
        for kind in ("user", "key", "staff"):
            self.assertIn(f"hypothesis.new={kind}", updates.WAKES["coordinator"])
        self.assertFalse([wake for wake in updates.WAKES["coordinator"] if wake in ("hypothesis.new", "hypothesis.new=agent", "hypothesis.new=coordinator")])
        # A baseline raised mid-run moves the bar every researcher is ranked against.
        for role in ("agent", "coordinator"):
            self.assertIn("platform_experiment.baseline", updates.WAKES[role])

    # Three baseline jobs failed, one of them for the image the brief named, and the coordinator
    # heard of none until the agent gave up. Woken by every job that ended, it was woken by the
    # one that completed too, before the agent's finish it has to wait for.
    async def test_a_coordinator_of_an_open_experiment_is_woken_by_a_job_that_failed_and_of_a_running_one_is_not(self):
        coordinator = {**os.environ, "HL_TOKEN": "hls_" + base64.urlsafe_b64encode(b"coordinator:pe-1").decode().rstrip("=") + ".mac"}
        for status, wakes in (("open", updates.WAKES["coordinator"] + ("experiment.status=FAILED",)), ("running", updates.WAKES["coordinator"])):
            command, counter = fake_hl('[ "$1" = platform-experiments ] && { echo \'{\n "status": "%s"\n}\'; exit 0; }\n'
                                       'echo "$*" >> "$0.args"\nexit 0\n' % status)
            await updates.next_batch(coordinator, lambda: False, 0.2, command=command, coalesce_s=0)
            with open(counter.replace(".runs", ".args")) as f:
                self.assertEqual(f.read().splitlines()[0], "watch --wake " + ",".join(wakes))

    # A coordinator slept until ends_at in a run whose last researcher's machine had stopped for
    # good: nothing it wakes on was sent. The platform sends the status event again when the
    # status_reason begins to wait for the coordinator, with the reason as its detail.
    async def test_a_status_reason_that_waits_for_the_coordinator_wakes_it_and_names_the_reason(self):
        coordinator = {**os.environ, "HL_TOKEN": "hls_" + base64.urlsafe_b64encode(b"coordinator:pe-1").decode().rstrip("=") + ".mac"}
        done = ('{"kind":"platform_experiment.status","subject":"pe-1","value":"running","detail":"researchers_done",'
                '"agent_id":"","position":9,"cursor":9}')
        command, counter = fake_hl('[ "$1" = platform-experiments ] && { echo \'{"status": "running"}\'; exit 0; }\n'
                                   f'echo "$*" >> "$0.args"\necho \'{done}\'\n')
        events = await updates.next_batch(coordinator, lambda: False, 10, command=command, coalesce_s=0)
        self.assertEqual(updates.message(events, ""), "Platform updates:\nplatform_experiment.status pe-1 running researchers_done")
        with open(counter.replace(".runs", ".args")) as f:
            self.assertIn("platform_experiment.status", f.read().splitlines()[0].split(" ")[-1].split(","))

    # A coordinator was woken about 11 times an hour, often for one event of several that came
    # within a minute. What follows the waking event closely comes in the same wake; an agent's
    # batch, a message among it, comes at once.
    async def test_a_coordinators_batch_holds_what_arrives_soon_after_the_event_that_woke_it(self):
        coordinator = {**os.environ, "HL_TOKEN": "hls_" + base64.urlsafe_b64encode(b"coordinator:pe-1").decode().rstrip("=") + ".mac"}
        finished = COMMENT.replace("comment.new", "agent.finished")
        script = ('[ "$1" = platform-experiments ] && { echo \'{"status": "running"}\'; exit 0; }\n'
                  f'echo "$*" >> "$0.args"\n[ "$2" = --no-wait ] && {{ echo \'{finished}\'; exit 0; }}\necho \'{COMMENT}\'\n')
        command, _ = fake_hl(script)
        started = time.monotonic()
        events = await updates.next_batch(coordinator, lambda: False, 10, command=command, coalesce_s=0.3)
        self.assertGreaterEqual(time.monotonic() - started, 0.3)
        self.assertEqual([e["kind"] for e in events], ["comment.new", "agent.finished"])
        command, counter = fake_hl(script)
        events = await updates.next_batch(ENV, lambda: False, 10, command=command, coalesce_s=30)
        self.assertEqual([e["kind"] for e in events], ["comment.new"])
        with open(counter.replace(".runs", ".args")) as f:
            self.assertEqual(len(f.read().splitlines()), 1)

    # A platform experiment that could not be read was taken as running, so a coordinator of an
    # open one waited without the wake for a failed job and nothing said so.
    async def test_a_wait_that_cannot_be_set_up_is_a_poll_that_failed(self):
        coordinator = {**os.environ, "HL_TOKEN": "hls_" + base64.urlsafe_b64encode(b"coordinator:pe-1").decode().rstrip("=") + ".mac"}
        refused, counter = fake_hl('[ "$1" = platform-experiments ] && { [ -e "$0.asked" ] || { touch "$0.asked"; exit 1; }; echo \'{"status": "open"}\'; exit 0; }\n'
                                   f'echo "$*" >> "$0.args"\necho \'{COMMENT}\'\n')
        events = await updates.next_batch(coordinator, lambda: False, 10, command=refused, retry_s=0.1, coalesce_s=0)
        self.assertEqual([e["kind"] for e in events], ["comment.new", "comment.new"])
        with open(counter.replace(".runs", ".args")) as f:
            self.assertEqual(f.read().splitlines(), ["watch --wake " + ",".join(updates.WAKES["coordinator"] + updates.OPEN_WAKES),
                                                     "watch --no-wait --timeout 5"])

    # hl exits 1 for a refusal as for a platform that is down, so a failed watch starts no outage:
    # it is retried, and the read that keeps the outage clock is asked for at once.
    async def test_a_failed_watch_is_retried_and_asks_whether_the_platform_answers(self):
        command, counter = fake_hl(f"[ $RUNS -eq 1 ] && {{ echo refused >&2; exit 2; }}\necho '{COMPLETED}'\n")
        events = await updates.next_batch(ENV, lambda: False, 10, command=command, retry_s=0.1)
        self.assertEqual([e.get("subject") for e in events], ["exp-1"])
        self.assertEqual(runs(counter), 2)
        self.assertFalse(outage.under_way())
        await asyncio.wait_for(outage.next_read(60), timeout=1)

    # In an outage researchers with no job in flight retried in sleep loops: one that ended its
    # turn would have heard nothing until the idle nudge, half an hour later. Told of its end by
    # hl's exit code while the harness's own read still counted toward its exit, one was told
    # the platform was back by one clock and ended by the other.
    async def test_an_outage_is_waited_out_without_a_poll_and_its_end_wakes_with_what_waits(self):
        command, counter = fake_hl(f'echo "$*" >> "$0.args"\necho \'{COMMENT}\'\n')
        outage.unanswered()
        asyncio.get_running_loop().call_later(0.2, outage.answered)
        events = await updates.next_batch(ENV, lambda: False, 10, command=command, retry_s=0.05)
        self.assertRegex(updates.message(events, "agent-a"), r"^Platform updates:\ncomment\.new hyp-1 agent by agent-b\n"
                         r"platform\.reachable again: it could not be reached "
                         r"from \d\d:\d\d:\d\d to \d\d:\d\d:\d\d UTC\. Retry what was refused then\.$")
        self.assertEqual(outage._clock(3725.0), "01:02:05")
        with open(counter.replace(".runs", ".args")) as f:
            self.assertEqual(f.read().splitlines(), ["watch --no-wait --timeout 5"])

    # The poll after the refused tool call fails too. An agent that works on is told of the
    # outage's end by the drain after its next tool call.
    async def test_a_failed_drain_asks_whether_the_platform_answers_and_a_later_one_says_the_outage_ended(self):
        command, _ = fake_hl("[ $RUNS -eq 1 ] && exit 1\nexit 0\n")
        self.assertEqual(await updates.drain(ENV, command=command), [])
        self.assertFalse(outage.under_way())
        await asyncio.wait_for(outage.next_read(60), timeout=1)
        outage.unanswered()
        outage.answered()
        self.assertEqual([e["kind"] for e in await updates.drain(ENV, command=command)], ["platform.reachable"])
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

    async def test_ended_jobs_arrive_with_their_metrics_from_one_read(self):
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
                         "experiment.status exp-3 EVICTED oom on node blackhole-1")
        with open(counter.replace(".runs", ".reads")) as f:
            self.assertEqual(f.read(), "job status exp-1 exp-2 exp-3\n", "what stands around the batch comes with the situation")

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
    # platform's answer. Left out and only logged, a failed read handed the agent COMPLETED with
    # no metric, which reads as a job that reported none.
    async def test_a_read_that_failed_is_named_in_the_batch_with_the_command_that_reads_it(self):
        command, _ = fake_hl(f"echo '{COMPLETED}'\n", self.JOBS, self.STATS, self.QUOTA, read_exit=1)
        with self.assertLogs("llm_agent", "WARNING") as logged:
            events = await updates.drain(ENV, command=command)
        self.assertEqual(updates.message(events, "agent-a"), "Platform updates:\nexperiment.status exp-1 COMPLETED\n"
                         "not_read the metrics and machine of the jobs that ended: `hl job status exp-1` reads it")
        self.assertEqual(len(logged.records), 1)


# Capacity, budget, time, standings and allowances were each a separate read the agents had to
# think of. `hl situation` closes every prompt as it prints it; a read that failed says so.
class SituationTest(unittest.IsolatedAsyncioTestCase):
    async def read(self, script: str) -> str:
        directory = tempfile.mkdtemp()
        path = os.path.join(directory, "hl")
        with open(path, "w") as f:
            f.write("#!/bin/sh\n[ \"$1\" = situation ] || exit 9\n" + script)
        os.chmod(path, os.stat(path).st_mode | stat.S_IEXEC)
        return await REAL_SITUATION(ENV, hl=path)

    async def test_the_situation_is_pasted_as_hl_prints_it(self):
        self.assertEqual(await self.read("printf 'run: 90 minutes left\\ncapacity: fits_now 3\\n'\necho hint >&2\n"),
                         "Situation:\nrun: 90 minutes left\ncapacity: fits_now 3")

    async def test_a_read_that_failed_is_said_in_its_place(self):
        with self.assertLogs("llm_agent", "WARNING"):
            said = await self.read("echo 'hl: situation: unavailable: the platform did not answer' >&2\nexit 1\n")
        self.assertEqual(said, "Situation: not read, `hl situation` failed: hl: situation: unavailable: the platform did not answer")


# In a live run a coordinator ended a review with "nothing to act on" while eight jobs fit, none
# was in flight and the best was 27% short of the target, and wrote no summary of the results
# that had come in. Its harness reads the situation as the platform answers it to say so.
class StandingTest(unittest.IsolatedAsyncioTestCase):
    IDLE = {"run": {"minutes_left": 200}, "objective": [{"metric": "ratio", "gap_to_target_pct": 27.3}],
            "capacity": {"fits_now": 8, "jobs_in_flight_run": {"queued": 1, "running": 0}},
            "coordinator": {"jobs_completed_since_summary": 3}, "ideas_from_people": {"open": 2}}

    async def test_the_situation_is_read_as_json_and_a_failed_read_is_none(self):
        directory = tempfile.mkdtemp()
        path = os.path.join(directory, "hl")
        with open(path, "w") as f:
            f.write("#!/bin/sh\n[ \"$*\" = 'situation --json' ] || exit 9\necho '{\"run\": {\"minutes_left\": 5}}'\n")
        os.chmod(path, os.stat(path).st_mode | stat.S_IEXEC)
        self.assertEqual(await REAL_STANDING(ENV, hl=path), {"run": {"minutes_left": 5}})
        with open(path, "w") as f:
            f.write("#!/bin/sh\necho down >&2\nexit 1\n")
        with self.assertLogs("llm_agent", "WARNING"):
            self.assertIsNone(await REAL_STANDING(ENV, hl=path))

    def test_free_places_short_of_the_target_with_time_left_are_said_with_their_numbers(self):
        said = updates.capacity_idle(self.IDLE)
        self.assertEqual(said, updates.CAPACITY_IDLE.format(free=7, queued=1, running=0, gap=27.3, minutes=200))
        self.assertIn("7 more jobs fit now beside the 1 queued and 0 running, the best is 27.3% short", said)
        self.assertEqual(updates.review_s(self.IDLE), updates.REVIEW_IDLE_S)

    def test_free_places_past_the_target_are_said_too(self):
        reached = dict(self.IDLE, objective=[{"gap_to_target_pct": -53.5}])
        said = updates.capacity_idle(reached)
        self.assertEqual(said, updates.CAPACITY_IDLE_REACHED.format(free=7, queued=1, running=0, gap=53.5, minutes=200))
        self.assertIn("the best is 53.5% past the target", said)

    def test_a_full_capacity_no_measure_no_time_or_no_read_says_nothing(self):
        full = dict(self.IDLE, capacity={"fits_now": 1, "jobs_in_flight_run": {"queued": 1, "running": 7}})
        reached = dict(self.IDLE, objective=[{"gap_to_target_pct": -2}])
        unmeasured = dict(self.IDLE, objective=[{"metric": "ratio"}])
        over = dict(self.IDLE, run={"minutes_left": 0, "closed_at": "2026-10-10T23:00:00Z"})
        for situation in (full, unmeasured, over, None, {}):
            self.assertEqual(updates.capacity_idle(situation), "", situation)
        self.assertEqual(updates.review_s(full), updates.REVIEW_S)
        self.assertEqual(updates.review_s(None), updates.REVIEW_S)
        self.assertEqual(updates.review_s(reached), updates.REVIEW_IDLE_S, "free places past the target are still reviewed sooner")

    def test_the_review_adds_the_free_places_and_the_findings_the_summary_lacks(self):
        self.assertEqual(updates.review_prompt(10, self.IDLE), " ".join([
            updates.REVIEW_PROMPT.format(minutes=10), updates.capacity_idle(self.IDLE), updates.SUMMARY_STALE.format(jobs=3),
            updates.IDEAS_OPEN.format(ideas=2)]))
        written = dict(self.IDLE, capacity={"fits_now": 1, "jobs_in_flight_run": {"queued": 1, "running": 7}},
                       coordinator={"jobs_completed_since_summary": 0},
                       ideas_from_people={"open": 0})
        self.assertEqual(updates.review_prompt(20, written), updates.REVIEW_PROMPT.format(minutes=20))
        self.assertEqual(updates.review_prompt(20, None), updates.REVIEW_PROMPT.format(minutes=20))


class JobsInFlightTest(unittest.IsolatedAsyncioTestCase):
    async def test_queued_submitted_and_running_jobs_count_and_ended_ones_do_not(self):
        command, counter = fake_hl("", stats='{"total":9,"by_status":{"COMPLETED":6,"RUNNING":2,"SUBMITTED":1,"QUEUED":1}}\n')
        self.assertEqual(await updates.jobs_in_flight(ENV, hl=command[0]), 4)
        with open(counter.replace(".runs", ".reads")) as f:
            self.assertEqual(f.read(), "job stats --agent agent-a\n")
        command, _ = fake_hl("", stats='{"total":1,"by_status":{"COMPLETED":1}}\n')
        self.assertEqual(await updates.jobs_in_flight(ENV, hl=command[0]), 0)

    # Nothing is said on a guess: a read that failed is no "nothing in flight".
    async def test_a_read_that_failed_is_none_and_logged(self):
        command, _ = fake_hl("", stats=JobStateTest.STATS, read_exit=1)
        with self.assertLogs("llm_agent", "WARNING"):
            self.assertIsNone(await updates.jobs_in_flight(ENV, hl=command[0]))


class DrainTest(unittest.IsolatedAsyncioTestCase):
    async def test_a_drain_returns_what_waits_without_listening_for_more(self):
        command, _ = fake_hl(f"[ \"$2\" = '--no-wait' ] || exit 2\necho '{COMPLETED}'\n")
        events = await updates.drain(ENV, command=command)
        self.assertEqual([e["subject"] for e in events], ["exp-1"])

    async def test_a_drain_that_fails_in_an_outage_is_not_logged(self):
        command, _ = fake_hl("echo never connected >&2; exit 1\n")
        with self.assertLogs("llm_agent", "WARNING") as logged:
            await updates.drain(ENV, command=command)
            outage.unanswered()
            self.addCleanup(setattr, outage, "_since", None)
            for _ in range(3):
                await updates.drain(ENV, command=command)
        self.assertEqual(len(logged.records), 1)

    # A batch the harness took and never handed over, as when the CLI started a turn first or the
    # session died, is read again: the cursor its watch saved moves into place only once delivered.
    async def test_the_cursor_moves_only_once_what_was_read_is_delivered(self):
        command, _ = fake_hl(f"echo \"$RUNS\" > \"$HL_STATE_DIR/watch-pe-1-agent-a.json\"\necho '{COMPLETED}'\n")
        state = os.path.join(tempfile.mkdtemp(), ".hl-state")
        env = {**ENV, "HL_STATE_DIR": state}
        cursor = os.path.join(state, "watch-pe-1-agent-a.json")

        await updates.drain(updates.taking(env, "waiting"), command=command)
        self.assertFalse(os.path.exists(cursor))
        taken = updates.taking(env, "waiting")
        await updates.drain(taken, command=command)
        updates.delivered(taken)
        with open(cursor) as f:
            self.assertEqual(f.read().strip(), "2")
        taken = updates.taking(env, "draining")
        with open(os.path.join(taken["HL_STATE_DIR"], "watch-pe-1-agent-a.json")) as f:
            self.assertEqual(f.read().strip(), "2", "a read starts from the cursor delivered")

    # A turn the CLI starts by itself can drain while the wait holds a batch it took before: the
    # batch is the drain's to hand over, and the wait's cursor, now behind, never moves back.
    async def test_a_cursor_never_moves_back_over_a_batch_another_reader_delivered(self):
        command, _ = fake_hl(f"echo \"$RUNS\" > \"$HL_STATE_DIR/watch-pe-1-agent-a.json\"\necho '{COMPLETED}'\n")
        state = os.path.join(tempfile.mkdtemp(), ".hl-state")
        env = {**ENV, "HL_STATE_DIR": state}

        waiting = updates.taking(env, "waiting")
        await updates.drain(waiting, command=command)
        draining = updates.taking(env, "draining")
        await updates.drain(draining, command=command)
        await updates.drain(draining, command=command)
        self.assertFalse(updates.superseded(waiting))
        self.assertTrue(updates.delivered(draining))
        self.assertTrue(updates.superseded(waiting))
        self.assertFalse(updates.delivered(waiting))
        with open(os.path.join(state, "watch-pe-1-agent-a.json")) as f:
            self.assertEqual(f.read().strip(), "3")


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
        self.handing = asyncio.Lock()
        self.hook = run._updates_hook(self.setup, {"HL_STATE_DIR": os.path.join(tempfile.mkdtemp(), ".hl-state")}, self.handing)

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
            "additionalContext": "Platform updates:\njob.underperforming job-1 val_accuracy" + S}})
        self.assertEqual(await self.call(), {})

    async def test_an_update_handed_over_lets_every_job_be_read_again(self):
        from hypothesisloop_agent.claude import run

        run._forget_job_reads()
        self.assertIsNone(run._repeated_read("hl job status job-1", 0))
        await self.call()
        self.assertIsNotNone(run._repeated_read("hl job status job-1", 1))
        self.waiting = [{"kind": "experiment.status", "subject": "job-1", "value": "RUNNING", "agent_id": "agent-a"}]
        await self.call()
        self.assertIsNone(run._repeated_read("hl job status job-1", 2))

    async def test_parallel_tool_calls_share_one_drain(self):
        self.waiting = [{"kind": "comment.new", "subject": "hyp-1", "value": "human"}]
        outputs = await asyncio.gather(self.call(), self.call(), self.call())
        self.assertEqual(self.drains, 1)
        self.assertEqual([bool(o) for o in outputs], [True, False, False])

    async def test_a_step_during_another_handover_leaves_the_updates_waiting(self):
        self.waiting = [{"kind": "comment.new", "subject": "hyp-1", "value": "human"}]
        async with self.handing:
            self.assertEqual(await self.call(), {})
        self.assertEqual(self.drains, 0)

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
        self.assertTrue(told.endswith("Platform updates:\ncomment.new hyp-1 human"), told)  # a planner has no situation
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
            # SendMessage reaches a subagent of this session only, and a restart stopped those
            # started before it: a lead sent its stopped subagents messages nobody read.
            self.assertIn("start it again with Agent, from its hypothesis, branch and jobs", told["additionalContext"])
            self.assertIn("`hl hypothesis comment ID --text T` on one of its hypotheses: its owner and the "
                          "coordinator get it as `comment.new` in their next update", told["additionalContext"])

    async def test_a_message_its_own_subagent_got_is_left_alone(self):
        self.assertEqual(await self.call(hook_event_name="PostToolUse", tool_response={"success": True}), {})


# A baseline agent slept 23 times around `hl job status` for a job being built, though hl job
# submit had said the result arrives as an update when it ends its turn.
class WaitTest(unittest.IsolatedAsyncioTestCase):
    async def call(self, command):
        from hypothesisloop_agent.claude import run

        return await run._wait_hook({"tool_name": "Bash", "tool_input": {"command": command}}, "tool-1", {"signal": None})

    async def test_a_command_that_waits_is_refused_with_how_to_wait(self):
        for command in ("sleep 90; hl job status job-1", "sleep 2m && hl job logs job-1", "sleep 20; sleep 15",
                        "hl job wait job-1 --timeout 540"):
            told = (await self.call(command))["hookSpecificOutput"]
            self.assertEqual(told["permissionDecision"], "deny", command)
            self.assertIn("To wait, reply with no command in it", told["permissionDecisionReason"])

    async def test_a_backgrounded_command_is_refused_and_a_subagent_may_wait_for_its_jobs(self):
        from hypothesisloop_agent.claude import run

        told = (await run._wait_hook({"tool_name": "Bash", "tool_input": {"command": "make", "run_in_background": True}},
                                     "tool-1", {}))["hookSpecificOutput"]
        self.assertEqual((told["permissionDecision"], told["permissionDecisionReason"]), ("deny", run.BACKGROUND_COMMAND))
        told = (await run._wait_hook({"tool_name": "Bash", "agent_id": "sub-1", "tool_input": {"command": "make", "run_in_background": True}},
                                     "tool-1", {}))["hookSpecificOutput"]
        self.assertEqual((told["permissionDecision"], told["permissionDecisionReason"]), ("deny", run.SUBAGENT_BACKGROUND_COMMAND))
        for command in ("hl job wait job-1", "sleep 90; hl job status job-1"):
            self.assertEqual(await run._wait_hook({"tool_name": "Bash", "agent_id": "sub-1", "tool_input": {"command": command}},
                                                  "tool-1", {}), {}, command)

    # Measured on the bundled CLI: a subagent is given TaskStop too, and its call reaches the hook with its agent_id.
    async def test_the_agent_stops_a_subagent_and_a_subagent_stops_nothing(self):
        from hypothesisloop_agent.claude import run

        self.assertNotIn("TaskStop", run.DISALLOWED_TOOLS)
        stop = {"tool_name": "TaskStop", "tool_input": {"task_id": "a1"}}
        self.assertEqual(await run._task_stop_hook(stop, "tool-1", {}), {})
        told = (await run._task_stop_hook({**stop, "agent_id": "sub-1"}, "tool-1", {}))["hookSpecificOutput"]
        self.assertEqual((told["permissionDecision"], told["permissionDecisionReason"]), ("deny", run.SUBAGENT_TASK_STOP))

    # A subagent's interim messages each started a turn of its lead over the lead's whole context.
    async def test_the_agent_messages_a_subagent_and_a_subagent_reports_only_at_its_end(self):
        from hypothesisloop_agent.claude import run

        send = {"tool_name": "SendMessage", "tool_input": {"to": "a1", "message": "the best moved"}}
        self.assertEqual(await run._subagent_message_hook(send, "tool-1", {}), {})
        told = (await run._subagent_message_hook({**send, "agent_id": "sub-1"}, "tool-1", {}))["hookSpecificOutput"]
        self.assertEqual((told["permissionDecision"], told["permissionDecisionReason"]), ("deny", run.SUBAGENT_MESSAGE))

    async def test_a_short_pause_and_a_command_that_only_names_sleep_run(self):
        from hypothesisloop_agent.claude import run

        run._forget_job_reads()
        for command in ("sleep 5 && curl -s localhost:8000", "grep -n sleep src/train.py", "hl job status job-1"):
            self.assertEqual(await self.call(command), {}, command)


# Refused sleeps, a baseline agent read its running job's status and metrics every 3-5 s, each
# a model call, in commands that changed between reads.
class RereadTest(unittest.TestCase):
    def setUp(self):
        from hypothesisloop_agent.claude import run

        self.run = run
        run._forget_job_reads()

    def test_a_job_read_again_soon_with_nothing_delivered_between_is_refused_however_the_command_dresses_it(self):
        self.assertIsNone(self.run._repeated_read("hl job status job-1 2>&1; echo ---; hl job metrics job-1 2>&1", 0))
        for command in ("hl job status job-1", "date -u; hl job status job-1 2>&1 | python3 -c 'import json'",
                        "hl job metrics job-1 2>&1; hl job status job-1 2>&1 | tail -3"):
            self.assertIsNotNone(self.run._repeated_read(command, 5), command)
        self.assertEqual(self.run._repeated_read("hl job status job-1", 5), "hl job status job-1")

    def test_a_new_read_an_old_one_or_one_after_an_update_runs(self):
        self.assertIsNone(self.run._repeated_read("hl job status job-1", 0))
        self.assertIsNone(self.run._repeated_read("hl job logs job-1 | tail -40", 5))
        self.assertIsNone(self.run._repeated_read("hl job status job-2", 5))
        self.assertIsNone(self.run._repeated_read("hl job status job-1; hl job status job-3", 6))
        self.assertIsNone(self.run._repeated_read("hl job status job-1", 6 + self.run._REREAD_S))
        self.run._forget_job_reads()
        self.assertIsNone(self.run._repeated_read("hl job status job-1", 7 + self.run._REREAD_S))
        self.assertIsNone(self.run._repeated_read("hl job list --status RUNNING", 8 + self.run._REREAD_S))
        self.assertIsNone(self.run._repeated_read("hl job list --status RUNNING", 9 + self.run._REREAD_S))

    def test_the_refusal_says_how_to_wait(self):
        import asyncio

        call = lambda: asyncio.run(self.run._wait_hook({"tool_name": "Bash", "tool_input": {"command": "hl job status job-1"}}, "t", {}))
        self.assertEqual(call(), {})
        told = call()["hookSpecificOutput"]
        self.assertEqual(told["permissionDecision"], "deny")
        self.assertIn("`hl job status job-1` was read under 2 minutes ago and no update has come since", told["permissionDecisionReason"])
        self.assertIn("To wait, reply with no command in it", told["permissionDecisionReason"])


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

    # A coordinator's only steer was a comment on the researcher's latest hypothesis, which put
    # direction in the lab notebook and reached no researcher without one.
    def test_a_message_wakes_its_agent_and_is_told_as_what_it_says(self):
        sent = {"kind": "agent.message", "subject": "agent-a", "value": "coordinator", "detail": "leave the lr line, try warmup",
                "agent_id": "agent-a", "position": 9, "cursor": 9}
        self.assertEqual(updates.event_line(sent, "agent-a"), "message from the coordinator: leave the lr line, try warmup")
        self.assertEqual(updates.event_line(dict(sent, value="owner"), ""), "message to agent-a from the owner: leave the lr line, try warmup")
        self.assertIn("agent.message", updates.WAKES["agent"])
        self.assertNotIn("agent.message", updates.WAKES["coordinator"])
        # The owner's message, to the coordinator or a researcher, wakes the coordinator; its own do not.
        self.assertIn("agent.message=owner", updates.WAKES["coordinator"])
        self.assertIn("job.duplicate", updates.WAKES["coordinator"])
        self.assertNotIn("job.duplicate", updates.WAKES["agent"])
        to_coordinator = dict(sent, subject="coordinator", agent_id="coordinator", value="owner", detail="model the gap")
        self.assertEqual(updates.event_line(to_coordinator, "coordinator"), "message from the owner: model the gap")

    # A baseline agent told to end its turn while its environment built sat idle until the idle
    # nudge: the build failed 8 minutes in and nothing woke it.
    def test_the_end_of_a_build_wakes_its_agent_and_says_the_outcome(self):
        detail = ("the environment commit cccccccccccc declares in .hypothesisloop/Dockerfile did not build on pytorch: "
                  "`hl env status` at that commit prints the end of its output. Fix the file, commit and build again")
        ended = {"kind": "environment.status", "subject": "90aa96c4" * 8, "value": "failed", "detail": detail,
                 "agent_id": "agent-a", "position": 10, "cursor": 10}
        self.assertEqual(updates.event_line(ended, "agent-a"), f"environment failed: {detail}")
        self.assertIn("environment.status", updates.WAKES["agent"])



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

            async def receive_messages(self):
                await asyncio.Event().wait()
                yield

        async def one_batch(env, stopped, idle_s):
            return [{"kind": "experiment.status", "subject": "exp-1", "value": "COMPLETED", "agent_id": "agent-a"}]

        setup = SimpleNamespace(system_prompt="", workdir=tempfile.mkdtemp(), statedir=tempfile.mkdtemp(),
                                client=SimpleNamespace(get_platform_experiment=lambda _id: {"status": "running"}, get_signup=lambda *_: {}),
                                cfg=SimpleNamespace(agent_id="agent-a", flavor="generalist", role="competitor", api_url="http://x", token=ENV["HL_TOKEN"], platform_experiment_id="pe-1"))
        with mock.patch.object(run, "ClaudeSDKClient", Client), \
                mock.patch.object(updates, "next_batch", one_batch), \
                mock.patch.object(updates, "jobs_in_flight", mock.AsyncMock(return_value=1)), \
                mock.patch.object(updates, "drain", mock.AsyncMock(return_value=[])), \
                redirect_stdout(StringIO()) as out:
            await asyncio.wait_for(run._run_one_session(setup, "m", None, stop_flag), timeout=5)
        self.assertEqual(sent, ["Begin." + S, "Platform updates:\nexperiment.status exp-1 COMPLETED" + S, run._shutdown_prompt(setup) + S])
        self.assertIn("agent-a event experiment.status exp-1 COMPLETED", out.getvalue())
        self.assertEqual(os.listdir(setup.workdir), [], "the harness wrote into the directory the agent clones into")
        self.assertIn(".claude_session_id", os.listdir(setup.statedir))

    async def _turn_ends(self, cfg, in_flight: list, batches: list) -> tuple[list, int]:
        """The prompts a session of cfg is sent while each turn it ends finds the next of
        in_flight jobs of its own, and each wait hands it the next of batches; the last batch
        stops it. Also how many times its jobs were read."""
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

            async def receive_response(self):
                yield ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=False,
                                    num_turns=1, session_id="s")

            async def receive_messages(self):
                await asyncio.Event().wait()
                yield

        async def next_batch(env, stopped, idle_s):
            batch = batches.pop(0)
            stop_flag[0] = not batches
            return batch

        read = mock.AsyncMock(side_effect=in_flight)
        setup = SimpleNamespace(system_prompt="", workdir=tempfile.mkdtemp(), statedir=tempfile.mkdtemp(),
                                client=SimpleNamespace(get_platform_experiment=lambda _id: {"status": "running"}, get_signup=lambda *_: {}),
                                cfg=cfg)
        with mock.patch.object(run, "ClaudeSDKClient", Client), \
                mock.patch.object(updates, "next_batch", next_batch), \
                mock.patch.object(updates, "jobs_in_flight", read), \
                mock.patch.object(updates, "drain", mock.AsyncMock(return_value=[])), \
                redirect_stdout(StringIO()):
            await asyncio.wait_for(run._run_one_session(setup, "m", None, stop_flag), timeout=5)
        return sent[:-1], read.await_count

    # A researcher whose last job ended waited 10 minutes for agent.idle, or 30 for the harness's
    # nudge, for an update nothing of its own would send, and a lead told once rested for hours.
    # It is told at every end of a turn with nothing in flight but the end of the turn that
    # answered the statement: that one waits, and the platform's recurring agent.idle bounds
    # what the statement costs, where saying it again would be a model call after each answer.
    async def test_a_turn_ended_with_no_job_in_flight_is_told_so_unless_it_answered_that(self):
        from types import SimpleNamespace
        cfg = SimpleNamespace(agent_id="agent-a", flavor="generalist", role="competitor", api_url="http://x",
                              token=ENV["HL_TOKEN"], platform_experiment_id="pe-1")
        idle = [{"kind": "agent.idle", "subject": "agent-a", "value": "continues", "agent_id": "agent-a"}]
        sent, reads = await self._turn_ends(cfg, [0, 0], [idle, []])
        nothing = updates.NOTHING_IN_FLIGHT + S
        self.assertEqual(sent, ["Begin." + S, nothing, "Platform updates:\nagent.idle agent-a continues" + S, nothing])
        self.assertEqual(reads, 2, "the end of an answer to the statement reads no jobs")

    async def test_a_job_in_flight_and_a_failed_read_say_nothing(self):
        from types import SimpleNamespace
        cfg = SimpleNamespace(agent_id="agent-a", flavor="generalist", role="baseline", api_url="http://x",
                              token=ENV["HL_TOKEN"], platform_experiment_id="pe-1")
        update = [{"kind": "comment.new", "subject": "hyp-1", "value": "agent", "agent_id": "agent-b"}]
        sent, _ = await self._turn_ends(cfg, [None, 2, 0, 1], [update, update, []])
        self.assertEqual(sent[1:], ["Platform updates:\ncomment.new hyp-1 agent by agent-b" + S,
                                    "Platform updates:\ncomment.new hyp-1 agent by agent-b" + S, updates.NOTHING_IN_FLIGHT + S])

    # A lead whose subagents work in the background gets the turn the CLI starts when one ends,
    # without waiting for a platform update, and is not told it has nothing in flight meanwhile.
    async def test_a_subagent_ending_starts_the_next_turn_before_any_update(self):
        from contextlib import redirect_stdout
        from io import StringIO
        from types import SimpleNamespace
        from unittest import mock

        from claude_agent_sdk import ResultMessage, SystemMessage

        from hypothesisloop_agent.claude import run

        stop_flag = [False]
        sent = []
        result = ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=False, num_turns=1, session_id="s")
        turns = [[SystemMessage("background_tasks_changed", {"tasks": [{"task_id": "sub-1"}]}), result],
                 [SystemMessage("background_tasks_changed", {"tasks": []}), result], [result]]

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
                for message in turns.pop(0):
                    yield message

            async def receive_messages(self):
                if not own_turns:
                    own_turns.append(1)
                    yield SystemMessage("task_notification", {"task_id": "sub-1", "status": "completed"})
                    yield SystemMessage("init", {"session_id": "s"})
                await asyncio.Event().wait()

        waits, own_turns = [], []

        async def next_batch(env, stopped, idle_s):
            waits.append(idle_s)
            if len(waits) == 1:
                while not stopped():
                    await asyncio.sleep(0.01)
                return []
            stop_flag[0] = True
            return []

        setup = SimpleNamespace(system_prompt="", workdir=tempfile.mkdtemp(), statedir=tempfile.mkdtemp(),
                                client=SimpleNamespace(get_platform_experiment=lambda _id: {"status": "running"}, get_signup=lambda *_: {}),
                                cfg=SimpleNamespace(agent_id="agent-a", flavor="generalist", role="competitor", api_url="http://x",
                                                    token=ENV["HL_TOKEN"], platform_experiment_id="pe-1"))
        read = mock.AsyncMock(return_value=1)
        with mock.patch.object(run, "ClaudeSDKClient", Client), \
                mock.patch.object(updates, "next_batch", next_batch), \
                mock.patch.object(updates, "jobs_in_flight", read), \
                mock.patch.object(updates, "drain", mock.AsyncMock(return_value=[])), \
                redirect_stdout(StringIO()):
            await asyncio.wait_for(run._run_one_session(setup, "m", None, stop_flag), timeout=5)
        self.assertEqual((sent[:-1], len(waits), read.await_count), (["Begin." + S], 2, 1))

    # A resumed session whose subagents stopped with the last process answers the opening with an
    # empty turn and then the opening's own, nothing in the background. The second is read as a
    # turn, and the batch the wait took as it came is not handed over: it is read again.
    async def test_a_turn_the_cli_starts_by_itself_is_read_and_the_batch_taken_meanwhile_is_read_again(self):
        from contextlib import redirect_stdout
        from io import StringIO
        from types import SimpleNamespace
        from unittest import mock

        from claude_agent_sdk import ResultMessage, SystemMessage

        from hypothesisloop_agent.claude import run

        stop_flag = [False]
        sent, read_turns, own_turns = [], [], []
        result = ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=False, num_turns=1, session_id="s")

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
                read_turns.append(1)
                yield result

            async def receive_messages(self):
                if not own_turns:
                    own_turns.append(1)
                    yield SystemMessage("init", {"session_id": "s"})
                await asyncio.Event().wait()

        update = {"kind": "comment.new", "subject": "hyp-1", "value": "agent", "agent_id": "agent-b"}
        waits = []

        async def next_batch(env, stopped, idle_s):
            waits.append(env)
            if len(waits) == 1:
                await asyncio.sleep(0.05)
                return [update]
            stop_flag[0] = True
            return []

        setup = SimpleNamespace(system_prompt="", workdir=tempfile.mkdtemp(), statedir=tempfile.mkdtemp(),
                                client=SimpleNamespace(get_platform_experiment=lambda _id: {"status": "running"}, get_signup=lambda *_: {}),
                                cfg=SimpleNamespace(agent_id="agent-a", flavor="generalist", role="competitor", api_url="http://x",
                                                    token=ENV["HL_TOKEN"], platform_experiment_id="pe-1"))
        delivered = mock.Mock()
        with mock.patch.object(run, "ClaudeSDKClient", Client), \
                mock.patch.object(updates, "next_batch", next_batch), \
                mock.patch.object(updates, "delivered", delivered), \
                mock.patch.object(updates, "jobs_in_flight", mock.AsyncMock(return_value=1)), \
                mock.patch.object(updates, "drain", mock.AsyncMock(return_value=[update])), \
                redirect_stdout(StringIO()):
            await asyncio.wait_for(run._run_one_session(setup, "m", "s", stop_flag), timeout=5)
        self.assertEqual(len(read_turns), 3, "the opening's turn, the CLI's own and the last")
        self.assertEqual(sent[1:], [run._shutdown_prompt(setup) + "\n\nPlatform updates:\ncomment.new hyp-1 agent by agent-b" + S])
        self.assertEqual([call.args[0]["HL_STATE_DIR"].rsplit(".", 1)[1] for call in delivered.call_args_list], ["closing"])

    # A turn under way, whose start the wait missed by an instant, drained and handed over what
    # the wait had taken meanwhile: the wait hands it over no second time and reads that turn.
    async def test_a_batch_a_turn_under_way_handed_over_first_is_not_handed_over_again(self):
        from contextlib import redirect_stdout
        from io import StringIO
        from types import SimpleNamespace
        from unittest import mock

        from claude_agent_sdk import ResultMessage

        from hypothesisloop_agent.claude import run

        stop_flag = [False]
        sent, read_turns = [], []
        result = ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=False, num_turns=1, session_id="s")

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
                read_turns.append(1)
                yield result

            async def receive_messages(self):
                await asyncio.Event().wait()
                yield

        setup = SimpleNamespace(system_prompt="", workdir=tempfile.mkdtemp(), statedir=tempfile.mkdtemp(),
                                client=SimpleNamespace(get_platform_experiment=lambda _id: {"status": "running"}, get_signup=lambda *_: {}),
                                cfg=SimpleNamespace(agent_id="agent-a", flavor="generalist", role="competitor", api_url="http://x",
                                                    token=ENV["HL_TOKEN"], platform_experiment_id="pe-1"))
        waits = []

        async def next_batch(env, stopped, idle_s):
            waits.append(env)
            if len(waits) == 1:
                state = os.path.join(setup.statedir, ".hl-state")
                os.makedirs(state)
                with open(os.path.join(state, "watch-pe-1-agent-a.json"), "w") as f:
                    f.write("2")
                return [{"kind": "comment.new", "subject": "hyp-1", "value": "agent", "agent_id": "agent-b"}]
            stop_flag[0] = True
            return []

        with mock.patch.object(run, "ClaudeSDKClient", Client), \
                mock.patch.object(updates, "next_batch", next_batch), \
                mock.patch.object(updates, "jobs_in_flight", mock.AsyncMock(return_value=1)), \
                mock.patch.object(updates, "drain", mock.AsyncMock(return_value=[])), \
                redirect_stdout(StringIO()):
            await asyncio.wait_for(run._run_one_session(setup, "m", None, stop_flag), timeout=5)
        self.assertEqual(len(read_turns), 3, "the opening's turn, the one under way and the last")
        self.assertEqual(sent, ["Begin." + S, run._shutdown_prompt(setup) + S])

    async def test_a_coordinator_has_no_jobs_of_its_own_to_be_told_of(self):
        from types import SimpleNamespace
        from unittest import mock

        from hypothesisloop_agent import config
        cfg = SimpleNamespace(agent_id="", flavor=config.COORDINATOR_FLAVOR, role="", api_url="http://x",
                              token=ENV["HL_TOKEN"], platform_experiment_id="pe-1")
        update = [{"kind": "comment.new", "subject": "hyp-1", "value": "agent", "agent_id": "agent-b"}]
        with mock.patch("hypothesisloop_agent.claude.run._told", mock.AsyncMock(return_value="told")):
            sent, reads = await self._turn_ends(cfg, [], [update, []])
        self.assertEqual((sent[1:], reads), (["told"], 0))

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

            async def receive_messages(self):
                await asyncio.Event().wait()
                yield

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
                                                    token=ENV["HL_TOKEN"], platform_experiment_id="pe-1"))
        with mock.patch.object(run, "ClaudeSDKClient", Client), \
                mock.patch.object(run.seed, "planner", lambda workdir: ""), \
                mock.patch.object(updates, "next_batch", revised), \
                mock.patch.object(updates, "drain", mock.AsyncMock(return_value=[])), \
                redirect_stdout(StringIO()):
            await asyncio.wait_for(run._run_one_session(setup, "m", None, stop_flag), timeout=5)
        self.assertEqual(batches, 3)
        self.assertEqual(sent[0], "Begin.")
        self.assertEqual(sent[1], "The person read your answer and asks for this revision:\n\nMake it fit one accelerator.\n\n"
                                  "Read the draft and the capacity again and answer with a new proposal.")
