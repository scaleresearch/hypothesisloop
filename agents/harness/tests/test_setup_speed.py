import asyncio
import json
import os
import subprocess
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

from hypothesisloop_agent import api_client, core, seed
from hypothesisloop_agent.claude import run


def planner():
    return SimpleNamespace(agent_id="ag-planner", role="planner", platform_experiment_id="pe-1", hyperparameters={})


def baseline_agent():
    return SimpleNamespace(agent_id="ag-baseline", role="baseline", platform_experiment_id="pe-1", hyperparameters={})


def git(*args, cwd):
    subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True,
                   env={**os.environ, "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"})


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
            real = seed._run

            def fake(command, cwd=None):
                if command[0] == "hl":
                    return draft if command[1] == "platform-experiments" else "cluster k3s cpu cores_available=8"
                return real(command, cwd)

            with mock.patch.object(seed, "_run", fake):
                opening = seed.planner(workdir)
            self.assertTrue(os.path.exists(os.path.join(workdir, "src", "codec.py")), "the repository was not cloned into the working directory")
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
                                    client=None, commands="usage: hl", workdir=workdir, statedir=root)
            with mock.patch.object(run, "ClaudeAgentOptions", options), self.assertRaises(Started) as started:
                asyncio.run(run._run_one_session(setup, None, [False]))
            self.assertEqual(started.exception.args[0], workdir)

    # Only a session that starts fresh is opened with them: a resumed one has read them.
    def test_only_a_fresh_session_is_opened_with_its_roles_reads(self):
        pe = {"status": "draft", "status_reason": {"reason": "planning"}, "proposal": {"repo_map": "bench.py: the harness"}}
        client = SimpleNamespace(get_platform_experiment=lambda _id: pe)
        with mock.patch.object(seed, "planner", lambda workdir: "READS"):
            self.assertEqual(run._opening(SimpleNamespace(cfg=planner(), client=client, workdir="/w"), None, None), "Begin.\n\nREADS")
            self.assertEqual(run._opening(SimpleNamespace(cfg=planner(), client=client, workdir="/w"), "s-1", None), "Continue where you left off.")
        opened = run._opening(SimpleNamespace(cfg=baseline_agent(), client=client, workdir="/w"), None, None)
        self.assertTrue(opened.startswith("Your goal is the control") and opened.endswith("\n\n=== The planner's map of the repository\nbench.py: the harness"), opened)
        self.assertEqual(run._opening(SimpleNamespace(cfg=baseline_agent(), client=client, workdir="/w"), "s-1", None), "Continue where you left off.")

    # Baseline agents built the environment 9.5 to 14.5 min in. A fresh baseline
    # session opens with its goal, the slowest part first, and the planner's pins.
    def test_a_baseline_agent_opens_with_its_goal(self):
        opened = seed.baseline("src/: the code. rust-toolchain.toml pins 1.95.0.")
        first, _, rest = opened.partition("\n\n")
        for said in ("researchers, who wait for it", "slowest first", "environment build", "the planner's map below names", "subagents"):
            self.assertIn(said, first)
        self.assertTrue(rest.endswith("rust-toolchain.toml pins 1.95.0."), rest)
        bare = seed.baseline("")
        self.assertNotIn("map", bare)
        self.assertIn("the repository's pin files state", bare)
        open_pe = SimpleNamespace(get_platform_experiment=lambda _id: {"status": "open", "baseline_due_at": None})
        self.assertEqual(run._opening(SimpleNamespace(cfg=baseline_agent(), client=open_pe, workdir="/w"), None, None), bare)


class ToolsTest(unittest.TestCase):
    # The CLI deferred WebSearch and WebFetch behind ToolSearch: planners and researchers
    # never searched. Tool search is off, so both are sent to the model whole.
    def test_web_search_and_fetch_are_sent_with_the_other_tools(self):
        class Started(Exception):
            pass

        def options(**kwargs):
            raise Started(kwargs)

        with tempfile.TemporaryDirectory() as root:
            setup = SimpleNamespace(cfg=SimpleNamespace(**vars(planner()), api_url="http://api", token="t"),
                                    client=None, commands="usage: hl", workdir=root, statedir=root)
            with mock.patch.object(run, "ClaudeAgentOptions", options), self.assertRaises(Started) as started:
                asyncio.run(run._run_one_session(setup, None, [False]))
        given = started.exception.args[0]
        self.assertEqual(given["env"]["ENABLE_TOOL_SEARCH"], "false")
        self.assertFalse({"WebSearch", "WebFetch"} & set(given["disallowed_tools"]))


class UntilAcceptedTest(unittest.TestCase):
    # The baseline agent is signed up with the proposal and its machine starts while the draft
    # counts down: it starts its session once the draft is accepted, and not before.
    def test_a_baseline_agent_starts_once_its_draft_is_accepted(self):
        statuses = iter([{"status": "draft"}, api_client.APIError("GET", "/pe", 503, "down"), {"status": "draft"}, {"status": "open"}])
        read = []

        def get(_id):
            read.append(_id)
            answer = next(statuses)
            if isinstance(answer, Exception):
                raise answer
            return answer

        said = []
        setup = SimpleNamespace(cfg=baseline_agent(), client=SimpleNamespace(get_platform_experiment=get))
        asyncio.run(core.until_accepted(setup, [False], poll_s=0, said=lambda: said.append(1)))
        self.assertEqual(len(read), 4)
        self.assertEqual(said, [1])

    def test_no_other_agent_waits_and_a_stop_ends_the_wait(self):
        never = SimpleNamespace(get_platform_experiment=lambda _id: self.fail("read"))
        asyncio.run(core.until_accepted(SimpleNamespace(cfg=planner(), client=never), [False], poll_s=0))
        asyncio.run(core.until_accepted(SimpleNamespace(cfg=baseline_agent(), client=never), [True], poll_s=0))
        stop = [False]

        def get(_id):
            stop[0] = True
            return {"status": "draft"}

        asyncio.run(core.until_accepted(SimpleNamespace(cfg=baseline_agent(), client=SimpleNamespace(get_platform_experiment=get)), stop, poll_s=0))


class StagingTest(unittest.TestCase):
    # Agents installed a job's toolchain by hand, 4 to 10 minutes per machine: an agent with jobs
    # has the image they run in staged from the start, without waiting for it.
    def test_an_agent_with_jobs_stages_in_the_background_and_a_planner_does_not(self):
        with tempfile.TemporaryDirectory() as statedir, mock.patch("subprocess.Popen") as popen, mock.patch("subprocess.run"):
            core.start_staging(SimpleNamespace(cfg=planner(), statedir=statedir))
            popen.assert_not_called()
            core.start_staging(SimpleNamespace(cfg=baseline_agent(), statedir=statedir))
            popen.assert_called_once()
            self.assertEqual(popen.call_args.args[0][:3], ["hl", "env", "stage"])
            self.assertTrue(os.path.exists(os.path.join(statedir, "env-stage.log")))


if __name__ == "__main__":
    unittest.main()
