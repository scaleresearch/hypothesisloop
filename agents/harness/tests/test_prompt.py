import os
import pathlib
import subprocess
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

import hypothesisloop_agent
from hypothesisloop_agent import core, prompt


class GitIdentityTest(unittest.TestCase):
    # Every agent's commits carried the image's one name, so a branch's history did not say who
    # wrote what.
    def test_commits_carry_the_agents_own_id(self):
        agent = SimpleNamespace(agent_id="ag-7", role="researcher", platform_experiment_id="pe-1")
        identity = core.git_identity(agent)
        self.assertEqual((identity["GIT_AUTHOR_NAME"], identity["GIT_COMMITTER_NAME"]), ("ag-7", "ag-7"))
        self.assertTrue(identity["GIT_AUTHOR_EMAIL"].startswith("ag-7@"))
        coordinator = SimpleNamespace(agent_id="coordinator", role="coordinator", platform_experiment_id="pe-1")
        self.assertEqual(core.git_identity(coordinator)["GIT_AUTHOR_NAME"], "coordinator-pe-1")


class FillTest(unittest.TestCase):
    VALUES = {"hl_commands": "usage: hl {{not a placeholder}} $1 \\1", "workdir": "/agent/workspace"}

    def test_the_harness_placeholders_are_filled_in_one_pass_and_no_value_is_expanded_again(self):
        served = "Work in {{workdir}}.\n{{hl_commands}}\nAgain {{workdir}}, braces {single} stay."
        self.assertEqual(prompt.fill(served, ["hl_commands", "workdir"], self.VALUES),
                         "Work in /agent/workspace.\nusage: hl {{not a placeholder}} $1 \\1\nAgain /agent/workspace, braces {single} stay.")

    def test_a_placeholder_left_in_the_prompt_ends_the_harness(self):
        for served, said in (
            ("{{workdir}} for {{agent_id}}", "holds '{{agent_id}}'"),
            ("{{hl_commands}} and {{ workdir }}", "holds '{{ workdir }}'"),
            ("{{hl_commands}} {{", "holds '{{'"),
            # One the platform did not list for the harness is not the harness's to fill.
            ("{{hl_commands}} in {{workdir}}", "holds '{{workdir}}'"),
        ):
            placeholders = ["hl_commands"] if said == "holds '{{workdir}}'" else ["hl_commands", "workdir"]
            with self.assertRaises(SystemExit) as ctx:
                prompt.fill(served, placeholders, self.VALUES)
            self.assertIn(said, str(ctx.exception))

    def test_a_placeholder_the_harness_has_no_value_for_ends_the_harness(self):
        with self.assertRaises(SystemExit) as ctx:
            prompt.fill("{{hl_commands}} {{gpu}}", ["hl_commands", "gpu"], self.VALUES)
        self.assertIn("leaves gpu to the harness", str(ctx.exception))


class NoPromptFilesTest(unittest.TestCase):
    # The prompts live with the platform (controlplane/domain/agenttypes/builtin), which serves
    # every agent the version it is pinned to: a copy in the image would be one that drifts.
    def test_the_harness_ships_no_prompt_and_reads_none(self):
        package = pathlib.Path(hypothesisloop_agent.__file__).parent
        self.assertEqual(sorted(str(p.relative_to(package)) for p in package.rglob("*.md")), [])
        self.assertFalse((package / "prompts").exists())
        for source in package.rglob("*.py"):
            self.assertNotIn('.md"', source.read_text(), source)


class CloneWorkspaceTest(unittest.TestCase):
    # The CLI caches that its directory is no git repository: a researcher that cloned in its first
    # turn had every `isolation: "worktree"` subagent refused with "not in a git repository".
    def test_the_clone_is_there_before_the_cli_starts(self):
        with tempfile.TemporaryDirectory() as root:
            origin, workdir = os.path.join(root, "origin"), os.path.join(root, "workspace")
            os.makedirs(workdir)
            subprocess.run(["git", "init", "-q", "-b", "main", origin], check=True)
            subprocess.run(["git", "-C", origin, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q",
                            "--allow-empty", "-m", "start"], check=True)
            core.clone_workspace(origin, "main", workdir)
            self.assertTrue(os.path.isdir(os.path.join(workdir, ".git")))

    def test_a_workdir_in_use_is_left_as_it_is(self):
        with tempfile.TemporaryDirectory() as workdir:
            open(os.path.join(workdir, "work"), "w").close()
            core.clone_workspace("https://git/x", "main", workdir)
            self.assertEqual(os.listdir(workdir), ["work"])

    # A failed clone once left the workdir empty for the agent to clone, which it did without the
    # worktree subagents the CLI then refused.
    def test_a_failed_clone_fails_the_run(self):
        with tempfile.TemporaryDirectory() as workdir:
            missing = os.path.join(workdir, "missing")
            with self.assertRaises(SystemExit) as failed:
                core.clone_workspace(missing, "main", workdir)
            self.assertIn(f"cloning {missing} at main into {workdir} exited 128", str(failed.exception))
            with mock.patch.object(core.subprocess, "run", side_effect=subprocess.TimeoutExpired("git", 600)), \
                    self.assertRaises(SystemExit) as timed_out:
                core.clone_workspace(missing, "main", workdir)
            self.assertIn("failed: Command 'git' timed out", str(timed_out.exception))

    # The planner and every researcher clone with this one function, and nothing else in the
    # harness runs a clone of its own.
    def test_the_harness_has_one_clone(self):
        package = pathlib.Path(hypothesisloop_agent.__file__).parent
        cloning = sorted(str(p.relative_to(package)) for p in package.rglob("*.py") if '"clone"' in p.read_text())
        self.assertEqual(cloning, ["core.py"])

if __name__ == "__main__":
    unittest.main()
