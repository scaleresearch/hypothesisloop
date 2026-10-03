import os
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

from hypothesisloop_agent import api_client, session
from hypothesisloop_agent.claude import run


class Platform:
    """Holds what was last put under each path, as the platform does."""

    def __init__(self):
        self.stored: dict[str, bytes] = {}
        self.puts = 0
        self.fail: api_client.APIError | None = None

    def get_bytes(self, path):
        return self.stored.get(path)

    def put_bytes(self, path, content):
        if self.fail:
            raise self.fail
        self.puts += 1
        self.stored[path] = content


def _cfg(flavor="generalist"):
    return SimpleNamespace(platform_experiment_id="pe-1", agent_id="ag-1" if flavor != "coordinator" else "coordinator", flavor=flavor)


class Machine:
    """One sandbox: a working directory and the model CLI's home, both empty at first."""

    def __init__(self, platform, cfg=None):
        self.workdir, self.home = Path(tempfile.mkdtemp()), Path(tempfile.mkdtemp())
        self.sessions = session.Sessions(platform, cfg or _cfg(), str(self.workdir), str(self.home))

    def write(self, root: Path, relative: str, text: str) -> None:
        (root / relative).parent.mkdir(parents=True, exist_ok=True)
        (root / relative).write_text(text)


class SessionsTest(unittest.TestCase):
    def test_a_session_stored_on_one_machine_is_restored_on_the_next_file_for_file(self):
        platform = Platform()
        old = Machine(platform)
        old.write(old.workdir, ".claude_session_id", "s-1")
        old.write(old.home, "projects/-app-workspace-ag-1/s-1.jsonl", '{"turn":1}\n{"turn":2}\n')
        old.write(old.home, "projects/-app-workspace-ag-1/s-1/subagents/a.jsonl", "{}\n")
        old.write(old.workdir, "repo/unpushed.py", "not part of a session")
        files = [".claude_session_id", ".claude_compaction_handoff"]
        transcript = ["projects/-app-workspace-ag-1/s-1.jsonl", "projects/-app-workspace-ag-1/s-1/subagents/a.jsonl"]
        self.assertTrue(old.sessions.save(files, transcript))
        self.assertEqual(list(platform.stored), ["/platform-experiments/pe-1/agents/ag-1/session"])

        new = Machine(platform)
        self.assertTrue(new.sessions.restore())
        self.assertEqual((new.workdir / ".claude_session_id").read_text(), "s-1")
        self.assertEqual((new.home / transcript[0]).read_text(), '{"turn":1}\n{"turn":2}\n')
        self.assertEqual((new.home / transcript[1]).read_text(), "{}\n")
        self.assertFalse((new.workdir / "repo").exists(), "the working tree travelled with the session")
        self.assertFalse((new.workdir / ".claude_compaction_handoff").exists())
        # What was restored is what the platform holds, so storing it again sends nothing.
        self.assertFalse(new.sessions.save(files, transcript))
        self.assertEqual(platform.puts, 1)

    def test_a_coordinator_keeps_its_session_under_the_coordinators_path(self):
        platform = Platform()
        machine = Machine(platform, _cfg("coordinator"))
        machine.write(machine.workdir, ".claude_session_id", "s-1")
        machine.sessions.save([".claude_session_id"], [])
        self.assertEqual(list(platform.stored), ["/platform-experiments/pe-1/coordinator/session"])

    def test_nothing_is_sent_while_the_session_is_what_the_platform_holds(self):
        platform = Platform()
        machine = Machine(platform)
        machine.write(machine.workdir, ".claude_session_id", "s-1")
        machine.write(machine.home, "projects/p/s-1.jsonl", "one\n")
        files, transcript = [".claude_session_id"], ["projects/p/s-1.jsonl"]
        self.assertTrue(machine.sessions.save(files, transcript))
        for _ in range(3):
            # An idle wait that ends in a nudge leaves the files as they were, written again or not.
            os.utime(machine.home / "projects/p/s-1.jsonl", (1, 1))
            self.assertFalse(machine.sessions.save(files, transcript))
        self.assertEqual(platform.puts, 1)
        machine.write(machine.home, "projects/p/s-1.jsonl", "one\ntwo\n")
        self.assertTrue(machine.sessions.save(files, transcript))
        self.assertEqual(platform.puts, 2)

    def test_a_store_that_fails_is_tried_again_after_the_next_turn(self):
        platform = Platform()
        machine = Machine(platform)
        machine.write(machine.workdir, ".claude_session_id", "s-1")
        platform.fail = api_client.APIError("PUT", "http://x", 503, "away")
        with self.assertLogs("llm_agent", "WARNING"):
            self.assertFalse(machine.sessions.save([".claude_session_id"], []))
        platform.fail = None
        self.assertTrue(machine.sessions.save([".claude_session_id"], []))

    def test_a_session_above_what_the_platform_stores_is_not_sent(self):
        platform = Platform()
        machine = Machine(platform)
        (machine.workdir / ".claude_session_id").write_bytes(os.urandom(4096))
        with mock.patch.object(session, "MAX_BYTES", 1024), self.assertLogs("llm_agent", "ERROR"):
            self.assertFalse(machine.sessions.save([".claude_session_id"], []))
        self.assertEqual(platform.puts, 0)

    def test_a_machine_with_no_stored_session_starts_fresh(self):
        machine = Machine(Platform())
        self.assertFalse(machine.sessions.restore())
        self.assertEqual(list(machine.workdir.iterdir()), [])

    def test_a_platform_that_does_not_answer_is_asked_again_and_a_refusal_stops_the_agent(self):
        platform = Platform()
        old = Machine(platform)
        old.write(old.workdir, ".claude_session_id", "s-1")
        old.sessions.save([".claude_session_id"], [])
        answers = [api_client.APIError("GET", "http://x", 0, "timed out"), api_client.APIError("GET", "http://x", 502, "bad gateway")]
        real = platform.get_bytes

        def flaky(path):
            if answers:
                raise answers.pop(0)
            return real(path)

        new = Machine(platform)
        with mock.patch.object(platform, "get_bytes", flaky), mock.patch.object(session, "_READ_PAUSE_S", 0), \
                self.assertLogs("llm_agent", "WARNING"):
            self.assertTrue(new.sessions.restore())
        self.assertEqual((new.workdir / ".claude_session_id").read_text(), "s-1")

        def refused(path):
            raise api_client.APIError("GET", "http://x", 403, "not yours")

        with mock.patch.object(platform, "get_bytes", refused), self.assertRaises(SystemExit):
            Machine(platform).sessions.restore()

    def test_a_stored_session_that_names_a_path_outside_its_roots_stops_the_agent(self):
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
                Machine(platform).sessions.restore()


class ClaudeSessionFilesTest(unittest.TestCase):
    def test_the_harness_takes_the_platforms_session_only_on_a_machine_that_has_none(self):
        platform = Platform()
        old = Machine(platform)
        old.write(old.workdir, ".claude_session_id", "s-1")
        old.write(old.home, "projects/p/s-1.jsonl", "one\n")
        old.write(old.home, "projects/p/s-other.jsonl", "another agent's or an older session\n")
        with mock.patch.dict(os.environ, {"CLAUDE_CONFIG_DIR": str(old.home)}):
            self.assertEqual(run._transcript_files("s-1"), ["projects/p/s-1.jsonl"])
            self.assertEqual(run._transcript_files(None), [])
        old.sessions.save([".claude_session_id"], ["projects/p/s-1.jsonl"])

        new = Machine(platform)
        setup = SimpleNamespace(statedir=str(new.workdir))
        with mock.patch.object(run, "_sessions", new.sessions), mock.patch.object(run, "_on_new_machine", False):
            run._restore_session(setup)
            self.assertEqual(run._restart_state(setup), ("s-1", None))
            self.assertTrue(run._on_new_machine, "a session restored from the platform is not told its machine is new")
            run._settled_on_this_machine()
            self.assertFalse(run._on_new_machine)

        # A harness restarted on the machine it ran on keeps what is on its disk.
        kept = Machine(platform)
        kept.write(kept.workdir, ".claude_session_id", "s-local")
        setup = SimpleNamespace(statedir=str(kept.workdir))
        with mock.patch.object(run, "_sessions", kept.sessions), mock.patch.object(run, "_on_new_machine", False):
            run._restore_session(setup)
            self.assertFalse(run._on_new_machine)
        self.assertEqual((kept.workdir / ".claude_session_id").read_text(), "s-local")

    def test_a_handoff_written_at_rotation_travels_as_the_session(self):
        platform = Platform()
        old = Machine(platform)
        old.write(old.workdir, ".claude_compaction_handoff", "where things stand")
        old.sessions.save([".claude_session_id", ".claude_compaction_handoff"], [])
        new = Machine(platform)
        setup = SimpleNamespace(statedir=str(new.workdir))
        with mock.patch.object(run, "_sessions", new.sessions), mock.patch.object(run, "_on_new_machine", False):
            run._restore_session(setup)
        resume_id, prompt = run._restart_state(setup)
        self.assertIsNone(resume_id)
        self.assertIn("where things stand", prompt)


class MidTurnTest(unittest.IsolatedAsyncioTestCase):
    async def test_a_machine_lost_inside_its_first_turn_is_resumed_in_that_turn_with_its_update_cursor(self):
        platform = Platform()
        old = Machine(platform)
        setup = SimpleNamespace(statedir=str(old.workdir))
        old.write(old.home, "projects/p/s-live.jsonl", "the turn so far\n")
        old.write(old.workdir, ".hl-state/watch/cursor", "108202")
        with mock.patch.dict(os.environ, {"CLAUDE_CONFIG_DIR": str(old.home)}), \
                mock.patch.object(run, "_sessions", old.sessions), mock.patch.object(run, "_live_session_id", None):
            # No turn has ended, so no session id was saved: the first message names the session.
            run._seen_session(setup, SimpleNamespace(subtype="init", data={"session_id": "s-live"}))
            await run._store_session(setup)
            self.assertEqual(platform.puts, 1)
            await run._store_session(setup)
            self.assertEqual(platform.puts, 1, "an unchanged turn was sent again")
            old.write(old.home, "projects/p/s-live.jsonl", "the turn so far\nand a tool call\n")
            await run._store_session(setup)
            self.assertEqual(platform.puts, 2)

        new = Machine(platform)
        setup = SimpleNamespace(statedir=str(new.workdir))
        with mock.patch.object(run, "_sessions", new.sessions), mock.patch.object(run, "_on_new_machine", False):
            run._restore_session(setup)
            self.assertEqual(run._restart_state(setup), ("s-live", None))
        self.assertEqual((new.home / "projects/p/s-live.jsonl").read_text(), "the turn so far\nand a tool call\n")
        self.assertEqual((new.workdir / ".hl-state/watch/cursor").read_text(), "108202")

    async def test_a_session_rotated_out_is_not_the_one_resumed(self):
        platform = Platform()
        old = Machine(platform)
        setup = SimpleNamespace(statedir=str(old.workdir))
        with mock.patch.object(run, "_sessions", old.sessions), mock.patch.object(run, "_live_session_id", None):
            run._seen_session(setup, SimpleNamespace(session_id="s-old"))
            old.write(old.workdir, ".claude_compaction_handoff", "where things stand")
            run._forget_live_session(setup)
            await run._store_session(setup)
        new = Machine(platform)
        setup = SimpleNamespace(statedir=str(new.workdir))
        with mock.patch.object(run, "_sessions", new.sessions), mock.patch.object(run, "_on_new_machine", False):
            run._restore_session(setup)
            resume_id, prompt = run._restart_state(setup)
        self.assertIsNone(resume_id)
        self.assertIn("where things stand", prompt)


if __name__ == "__main__":
    unittest.main()


class NewMachineNoteTest(unittest.TestCase):
    # A restored coordinator was told to check out its branch, and it has none. A researcher's
    # branch is its hypothesis's, which only hl names.
    def test_a_researcher_is_sent_to_its_hypothesis_a_baseline_agent_to_its_branch_and_a_coordinator_to_neither(self):
        from types import SimpleNamespace
        researcher = run._new_machine_note(SimpleNamespace(flavor="generalist", role="competitor"))
        baseline = run._new_machine_note(SimpleNamespace(flavor="generalist", role="baseline"))
        coordinator = run._new_machine_note(SimpleNamespace(flavor="coordinator", role="coordinator"))
        for note in (researcher, baseline, coordinator):
            self.assertIn("The machine you ran on was replaced", note)
        self.assertIn("run `hl hypothesis checkout ID` for the hypothesis you work on", researcher)
        self.assertIn("check out your branch", baseline)
        self.assertNotIn("hypothes", baseline)
        self.assertNotIn("branch", coordinator)
        self.assertIn("summary", coordinator)

    # A planner whose machine died was restarted with the researcher's note, and has no hypothesis
    # to check out and no job to look at.
    def test_a_planner_is_sent_back_to_its_draft_and_its_proposal(self):
        from types import SimpleNamespace
        note = run._new_machine_note(SimpleNamespace(flavor="planner", role="planner"))
        self.assertIn("The machine you ran on was replaced", note)
        self.assertIn("write your proposal's file again", note)
        self.assertNotIn("hypothes", note)
        self.assertNotIn("jobs", note)
