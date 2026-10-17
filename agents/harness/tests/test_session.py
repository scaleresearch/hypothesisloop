import os
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

from hypothesisloop_agent import api_client, session
from hypothesisloop_agent.claude import store as store_module, transcripts
from hypothesisloop_agent.claude.store import Store


class Platform:
    """Holds what was last put under each path, as the platform does."""

    def __init__(self):
        self.stored: dict[str, bytes] = {}
        self.puts = 0
        self.fail: api_client.APIError | None = None

    async def get_bytes(self, path):
        return self.stored.get(path)

    async def put_bytes(self, path, content):
        if self.fail:
            raise self.fail
        self.puts += 1
        self.stored[path] = content


def _cfg(role="researcher"):
    return SimpleNamespace(platform_experiment_id="pe-1", agent_id="ag-1" if role != "coordinator" else "coordinator", role=role)


class Machine:
    """One sandbox: a working directory and the model CLI's home, both empty at first."""

    def __init__(self, platform, cfg=None):
        self.platform, self.cfg = platform, cfg or _cfg()
        self.workdir, self.home = Path(tempfile.mkdtemp()), Path(tempfile.mkdtemp())
        self.sessions = session.Sessions(platform, self.cfg, str(self.workdir), str(self.home))

    def write(self, root: Path, relative: str, text: str) -> None:
        (root / relative).parent.mkdir(parents=True, exist_ok=True)
        (root / relative).write_text(text)

    def on(self):
        """The model CLI's home is this machine's while it is in effect."""
        return mock.patch.dict(os.environ, {"CLAUDE_CONFIG_DIR": str(self.home)})

    def store(self) -> Store:
        """A harness process started on this machine, which keeps its session as the Claude harness does."""
        with self.on():
            return Store(SimpleNamespace(client=self.platform, cfg=self.cfg, statedir=str(self.workdir)))


class SessionsTest(unittest.IsolatedAsyncioTestCase):
    async def test_a_session_stored_on_one_machine_is_restored_on_the_next_file_for_file(self):
        platform = Platform()
        old = Machine(platform)
        old.write(old.workdir, ".claude_session_id", "s-1")
        old.write(old.home, "projects/-app-workspace-ag-1/s-1.jsonl", '{"turn":1}\n{"turn":2}\n')
        old.write(old.home, "projects/-app-workspace-ag-1/s-1/subagents/a.jsonl", "{}\n")
        old.write(old.workdir, "repo/unpushed.py", "not part of a session")
        files = [".claude_session_id", ".claude_compaction_handoff"]
        transcript = ["projects/-app-workspace-ag-1/s-1.jsonl", "projects/-app-workspace-ag-1/s-1/subagents/a.jsonl"]
        self.assertTrue(await old.sessions.save(files, transcript))
        self.assertEqual(list(platform.stored), ["/platform-experiments/pe-1/agents/ag-1/session"])

        new = Machine(platform)
        self.assertTrue(await new.sessions.restore())
        self.assertEqual((new.workdir / ".claude_session_id").read_text(), "s-1")
        self.assertEqual((new.home / transcript[0]).read_text(), '{"turn":1}\n{"turn":2}\n')
        self.assertEqual((new.home / transcript[1]).read_text(), "{}\n")
        self.assertFalse((new.workdir / "repo").exists(), "the working tree travelled with the session")
        self.assertFalse((new.workdir / ".claude_compaction_handoff").exists())
        # What was restored is what the platform holds, so storing it again sends nothing.
        self.assertFalse(await new.sessions.save(files, transcript))
        self.assertEqual(platform.puts, 1)

    async def test_a_coordinator_keeps_its_session_under_the_coordinators_path(self):
        platform = Platform()
        machine = Machine(platform, _cfg("coordinator"))
        machine.write(machine.workdir, ".claude_session_id", "s-1")
        await machine.sessions.save([".claude_session_id"], [])
        self.assertEqual(list(platform.stored), ["/platform-experiments/pe-1/coordinator/session"])

    async def test_nothing_is_sent_while_the_session_is_what_the_platform_holds(self):
        platform = Platform()
        machine = Machine(platform)
        machine.write(machine.workdir, ".claude_session_id", "s-1")
        machine.write(machine.home, "projects/p/s-1.jsonl", "one\n")
        files, transcript = [".claude_session_id"], ["projects/p/s-1.jsonl"]
        self.assertTrue(await machine.sessions.save(files, transcript))
        for _ in range(3):
            # An idle wait that ends in a nudge leaves the files as they were, written again or not.
            os.utime(machine.home / "projects/p/s-1.jsonl", (1, 1))
            self.assertFalse(await machine.sessions.save(files, transcript))
        self.assertEqual(platform.puts, 1)
        machine.write(machine.home, "projects/p/s-1.jsonl", "one\ntwo\n")
        self.assertTrue(await machine.sessions.save(files, transcript))
        self.assertEqual(platform.puts, 2)

    async def test_a_store_that_fails_says_why_and_the_next_one_sends_the_session(self):
        platform = Platform()
        machine = Machine(platform)
        machine.write(machine.workdir, ".claude_session_id", "s-1")
        for failure, why in ((api_client.APIError("PUT", "http://x", 503, "away"), "the platform answered HTTP 503"),
                             (api_client.APIError("PUT", "http://x", 0, "timed out"), "the platform could not be reached")):
            platform.fail = failure
            with self.assertLogs("llm_agent", "WARNING"), self.assertRaises(session.NotStored) as not_stored:
                await machine.sessions.save([".claude_session_id"], [])
            self.assertEqual(str(not_stored.exception), why)
        platform.fail = None
        self.assertTrue(await machine.sessions.save([".claude_session_id"], []))

    async def test_a_session_above_what_the_platform_stores_is_not_sent(self):
        platform = Platform()
        machine = Machine(platform)
        (machine.workdir / ".claude_session_id").write_bytes(os.urandom(4 << 20))
        with mock.patch.object(session, "MAX_BYTES", 1 << 20), self.assertRaises(session.NotStored) as not_stored:
            await machine.sessions.save([".claude_session_id"], [])
        self.assertEqual(str(not_stored.exception), "it is 4.0 MiB packed and the platform stores 1 MiB")
        self.assertEqual(platform.puts, 0)

    async def test_a_machine_with_no_stored_session_starts_fresh(self):
        machine = Machine(Platform())
        self.assertFalse(await machine.sessions.restore())
        self.assertEqual(list(machine.workdir.iterdir()), [])

    async def test_a_session_that_cannot_be_read_stops_the_agent_with_why(self):
        platform = Platform()

        async def refused(path):
            raise api_client.APIError("GET", "http://x", 403, "not yours")

        with mock.patch.object(platform, "get_bytes", refused), self.assertRaises(SystemExit) as stopped:
            await Machine(platform).sessions.restore()
        self.assertIn("not yours", str(stopped.exception))

    async def test_a_stored_session_that_names_a_path_outside_its_roots_stops_the_agent(self):
        import io
        import tarfile
        for name in ("workdir/../../etc/passwd", "elsewhere/file", "workdir"):
            packed = io.BytesIO()
            with tarfile.open(fileobj=packed, mode="w:gz") as archive:
                info = tarfile.TarInfo(name)
                info.size = 1
                archive.addfile(info, io.BytesIO(b"x"))
            platform = Platform()
            platform.stored["/platform-experiments/pe-1/agents/ag-1/session"] = packed.getvalue()
            with self.assertRaises(SystemExit, msg=name):
                await Machine(platform).sessions.restore()


class ClaudeSessionFilesTest(unittest.IsolatedAsyncioTestCase):
    async def test_the_harness_takes_the_platforms_session_only_on_a_machine_that_has_none(self):
        platform = Platform()
        old = Machine(platform)
        old.write(old.workdir, ".claude_session_id", "s-1")
        old.write(old.home, "projects/p/s-1.jsonl", "one\n")
        old.write(old.home, "projects/p/s-other.jsonl", "another agent's or an older session\n")
        with old.on():
            self.assertEqual(transcripts.files("s-1"), ["projects/p/s-1.jsonl"])
            self.assertEqual(transcripts.files(None), [])
        await old.sessions.save([".claude_session_id"], ["projects/p/s-1.jsonl"])

        new = Machine(platform)
        store = new.store()
        with new.on():
            await store.restore()
            self.assertEqual(store.restart_state(), ("s-1", None))
        self.assertTrue(store.on_new_machine, "a session restored from the platform is not told its machine is new")
        store.settled()
        self.assertFalse(store.on_new_machine)

        # A harness restarted on the machine it ran on keeps what is on its disk.
        kept = Machine(platform)
        kept.write(kept.workdir, ".claude_session_id", "s-local")
        store = kept.store()
        with kept.on():
            await store.restore()
        self.assertFalse(store.on_new_machine)
        self.assertEqual((kept.workdir / ".claude_session_id").read_text(), "s-local")

    async def test_a_handoff_written_at_rotation_travels_as_the_session(self):
        platform = Platform()
        old = Machine(platform)
        old.write(old.workdir, ".claude_compaction_handoff", "where things stand")
        await old.sessions.save([".claude_session_id", ".claude_compaction_handoff"], [])
        new = Machine(platform)
        store = new.store()
        with new.on():
            await store.restore()
            resume_id, prompt = store.restart_state()
        self.assertIsNone(resume_id)
        self.assertIn("where things stand", prompt)


class NotStoredTest(unittest.IsolatedAsyncioTestCase):
    # A session over the limit, or one the platform would not take, was logged and skipped at
    # every turn: the agent worked on, and a replacement machine resumed from an old snapshot
    # with nothing having said so.
    async def test_a_store_that_keeps_failing_is_said_in_the_activity_line_until_one_succeeds(self):
        from hypothesisloop_agent import progress
        from hypothesisloop_agent.claude import stream

        platform = Platform()
        machine = Machine(platform)
        machine.write(machine.workdir, ".claude_session_id", "s-1")
        store = machine.store()
        said = []
        beating = progress.Progress(quiet_limit_s=60, emit=lambda line: line.startswith(progress.STATE_PREFIX) or said.append(line))
        beating.doing("thinking")
        platform.fail = api_client.APIError("PUT", "http://x", 413, "too large")
        with machine.on(), mock.patch.object(stream, "progress", beating):
            with self.assertLogs("llm_agent", "WARNING") as logged:
                await store.save()
            self.assertEqual(logged.records[-1].levelname, "WARNING")
            beating.beat()
            self.assertEqual(said[-1], "hl-activity thinking")
            with mock.patch.object(store_module, "_NOT_STORED_SAID_S", 0), self.assertLogs("llm_agent", "ERROR") as logged:
                await store.save()
            self.assertIn("the platform answered HTTP 413. A replacement machine would resume from the last one stored", logged.output[-1])
            beating.beat()
            self.assertEqual(said[-1], "hl-activity session not stored: the platform answered HTTP 413. thinking")
            platform.fail = None
            await store.save()
            self.assertEqual(platform.puts, 1)
            self.assertIsNone(store._not_stored_since)
            beating.beat()
            self.assertEqual(said[-1], "hl-activity thinking")


class MidTurnTest(unittest.IsolatedAsyncioTestCase):
    async def test_a_machine_lost_inside_its_first_turn_is_resumed_in_that_turn_with_its_update_cursor(self):
        platform = Platform()
        old = Machine(platform)
        old.write(old.home, "projects/p/s-live.jsonl", "the turn so far\n")
        old.write(old.workdir, ".hl-state/watch/cursor", "108202")
        store = old.store()
        with old.on():
            # No turn has ended, so no session id was saved: the first message names the session.
            store.seen(SimpleNamespace(subtype="init", data={"session_id": "s-live"}))
            await store.save()
            self.assertEqual(platform.puts, 1)
            await store.save()
            self.assertEqual(platform.puts, 1, "an unchanged turn was sent again")
            old.write(old.home, "projects/p/s-live.jsonl", "the turn so far\nand a tool call\n")
            await store.save()
            self.assertEqual(platform.puts, 2)

        new = Machine(platform)
        store = new.store()
        with new.on():
            await store.restore()
            self.assertEqual(store.restart_state(), ("s-live", None))
        self.assertEqual((new.home / "projects/p/s-live.jsonl").read_text(), "the turn so far\nand a tool call\n")
        self.assertEqual((new.workdir / ".hl-state/watch/cursor").read_text(), "108202")

    async def test_a_session_rotated_out_is_not_the_one_resumed(self):
        platform = Platform()
        old = Machine(platform)
        store = old.store()
        with old.on():
            store.seen(SimpleNamespace(session_id="s-old"))
            await store.rotate("where things stand", "s-old")
        new = Machine(platform)
        store = new.store()
        with new.on():
            await store.restore()
            resume_id, prompt = store.restart_state()
        self.assertIsNone(resume_id)
        self.assertIn("where things stand", prompt)


if __name__ == "__main__":
    unittest.main()
