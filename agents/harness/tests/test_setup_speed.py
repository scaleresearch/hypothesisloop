import asyncio
import json
import os
import subprocess
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

from conftest import FakePlatform, connect
from hypothesisloop_agent import api_client, core, seed
from hypothesisloop_agent.claude import cli, prompts


def planner():
    return SimpleNamespace(agent_id="ag-planner", role="planner", platform_experiment_id="pe-1", hyperparameters={})


def baseline_agent():
    return SimpleNamespace(agent_id="ag-baseline", role="baseline", platform_experiment_id="pe-1", hyperparameters={})


def git(*args, cwd):
    subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True,
                   env={**os.environ, "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"})


def opening(setup, resume_id) -> str:
    """What opens a connection of setup on a machine it ran on, after no wait for a limit."""
    return asyncio.run(prompts.opening(setup, SimpleNamespace(on_new_machine=False), resume_id, None))


class PlannerOpeningTest(unittest.TestCase):
    # A planner spent its first turns on the same reads every plan makes: the draft, the
    # capacity, the clone, its README and .hypothesisloop/. The harness makes them.
    def test_the_draft_the_capacity_and_the_repository_open_a_fresh_plan(self):
        with tempfile.TemporaryDirectory() as origin, tempfile.TemporaryDirectory() as workdir:
            git("init", "-q", "-b", "main", cwd=origin)
            os.makedirs(os.path.join(origin, ".hypothesisloop"))
            os.makedirs(os.path.join(origin, "src"))
            for path, text in {"README.md": "# Codec\nRun bench.py.\n", ".hypothesisloop/experiment.yaml": "name: codec\n", "src/codec.py": "x = 1\n"}.items():
                with open(os.path.join(origin, path), "w") as f:
                    f.write(text)
            git("add", ".", cwd=origin)
            git("commit", "-qm", "init", cwd=origin)
            draft = json.dumps({"id": "pe-1", "status": "draft", "repository_url": "file://" + origin, "branch": "main"})
            core.clone_workspace("file://" + origin, "main", workdir)
            real, ran = seed._run, []

            def fake(command, cwd=None):
                ran.append(command[:2])
                if command[0] == "hl":
                    return draft if command[1] == "platform-experiments" else "cluster k3s cpu cores_available=8"
                return real(command, cwd)

            with mock.patch.object(seed, "_run", fake):
                opening = seed.planner(workdir)
            self.assertNotIn(["git", "clone"], ran)
            for shown in ("=== hl platform-experiments get\n" + draft, "=== hl capacity\ncluster k3s", "src/codec.py",
                          "=== README.md\n# Codec\nRun bench.py.", "=== .hypothesisloop/experiment.yaml\nname: codec"):
                self.assertIn(shown, opening)
            self.assertNotIn("Not read", opening)

    def test_what_could_not_be_read_is_left_to_the_planner(self):
        def fake(command, cwd=None):
            raise RuntimeError(f"`{command[0]}` exited 1: refused")

        with tempfile.TemporaryDirectory() as workdir, mock.patch.object(seed, "_run", fake):
            opening = seed.planner(workdir)
        self.assertIn("=== Not read, do it yourself\n`hl` exited 1: refused", opening)
        self.assertIn("the repository was not read", opening)

    # A planner read /home/user/<repo>/... and ran `find /` before it used its working
    # directory. The opening says that directory's absolute path first, and the model CLI starts
    # in it.
    def test_the_opening_says_the_absolute_working_directory_the_cli_starts_in(self):
        def fake(command, cwd=None):
            raise RuntimeError(f"`{command[0]}` exited 1: refused")

        with tempfile.TemporaryDirectory() as root:
            os.makedirs(os.path.join(root, "workspace"))
            here = os.getcwd()
            os.chdir(root)
            try:
                with mock.patch.object(seed, "_run", fake):
                    opening = seed.planner("workspace")
            finally:
                os.chdir(here)
            workdir = os.path.join(os.path.realpath(root), "workspace")
            self.assertTrue(opening.startswith(f"Your working directory is {workdir}, the root of the repository's clone"), opening[:200])

            class Started(Exception):
                pass

            def options(**kwargs):
                raise Started(kwargs["cwd"])

            setup = SimpleNamespace(cfg=SimpleNamespace(**vars(planner()), api_url="http://api", token="t"),
                                    client=FakePlatform(), commands="usage: hl", workdir=workdir, statedir=root)
            with mock.patch.object(cli, "ClaudeAgentOptions", options), mock.patch.object(seed, "planner", lambda workdir: ""), \
                    self.assertRaises(Started) as started:
                asyncio.run(connect(setup, None, [False]))
            self.assertEqual(started.exception.args[0], workdir)

    # Only a session that starts fresh is opened with them: a resumed one has read them.
    def test_only_a_fresh_session_is_opened_with_its_roles_reads(self):
        pe = {"status": "draft", "status_reason": {"reason": "planning"}}
        client = FakePlatform(pe)
        with mock.patch.object(seed, "planner", lambda workdir: "READS"):
            self.assertEqual(opening(SimpleNamespace(cfg=planner(), client=client, workdir="/w"), None), "Begin.\n\nREADS")
            self.assertEqual(opening(SimpleNamespace(cfg=planner(), client=client, workdir="/w"), "s-1"), "Continue where you left off.")
        opened = opening(SimpleNamespace(cfg=baseline_agent(), client=client, workdir="/w"), None)
        self.assertEqual(opened, seed.BASELINE_OPENING)
        self.assertEqual(opening(SimpleNamespace(cfg=baseline_agent(), client=client, workdir="/w"), "s-1"), "Continue where you left off.")

    # A fresh baseline session opens with its goal, the slowest part first, and where the plan is
    # while the planner drafts it: it starts beside the planner, before there is a plan.
    def test_a_baseline_agent_opens_with_its_goal(self):
        for said in ("executors, who wait for it", "slowest first", "experiment's image", "the repository's pin files state", "subagents",
                     "`proposal` in `hl platform-experiments get` is the plan so far"):
            self.assertIn(said, seed.BASELINE_OPENING)
        open_pe = FakePlatform({"status": "open", "baseline_due_at": None})
        self.assertEqual(opening(SimpleNamespace(cfg=baseline_agent(), client=open_pe, workdir="/w"), None), seed.BASELINE_OPENING)


class ToolsTest(unittest.TestCase):
    # The CLI deferred WebSearch and WebFetch behind ToolSearch: planners and executors
    # never searched. Tool search is off, so both are sent to the model whole.
    def test_web_search_and_fetch_are_sent_with_the_other_tools(self):
        class Started(Exception):
            pass

        def options(**kwargs):
            raise Started(kwargs)

        with tempfile.TemporaryDirectory() as root:
            setup = SimpleNamespace(cfg=SimpleNamespace(**vars(planner()), api_url="http://api", token="t"),
                                    client=FakePlatform(), commands="usage: hl", workdir=root, statedir=root)
            with mock.patch.object(cli, "ClaudeAgentOptions", options), mock.patch.object(seed, "planner", lambda workdir: ""), \
                    self.assertRaises(Started) as started:
                asyncio.run(connect(setup, None, [False]))
        given = started.exception.args[0]
        self.assertEqual(given["env"]["ENABLE_TOOL_SEARCH"], "false")
        self.assertFalse({"WebSearch", "WebFetch"} & set(given["disallowed_tools"]))


class TakesNoJobYetTest(unittest.TestCase):
    # A baseline agent starts beside the planner and an executor before the run starts: neither is
    # told it has nothing in flight while the platform takes no job of it yet.
    def test_the_platform_takes_no_job_of_a_baseline_agent_in_a_draft_nor_of_an_executor_before_the_run(self):
        executor = SimpleNamespace(**{**vars(baseline_agent()), "role": "executor"})
        for cfg, status, waits in ((baseline_agent(), "draft", True), (baseline_agent(), "open", False),
                                   (executor, "open", True), (executor, "running", False)):
            setup = SimpleNamespace(cfg=cfg, client=FakePlatform({"status": status}))
            self.assertEqual(asyncio.run(core.takes_no_job_yet(setup)), waits, (cfg.role, status))
        down = SimpleNamespace(cfg=baseline_agent(), client=FakePlatform(api_client.APIError("GET", "/pe", 503, "down")))
        self.assertFalse(asyncio.run(core.takes_no_job_yet(down)))


if __name__ == "__main__":
    unittest.main()
