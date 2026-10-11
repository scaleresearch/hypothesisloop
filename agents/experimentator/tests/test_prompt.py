import os
import subprocess
import tempfile
import unittest
from types import SimpleNamespace

from hypothesisloop_agent import core


class GitIdentityTest(unittest.TestCase):
    # Every agent's commits carried the image's one name, so a branch's history did not say who
    # wrote what.
    def test_commits_carry_the_agents_own_id(self):
        agent = SimpleNamespace(agent_id="ag-7", flavor="generalist", platform_experiment_id="pe-1")
        identity = core.git_identity(agent)
        self.assertEqual((identity["GIT_AUTHOR_NAME"], identity["GIT_COMMITTER_NAME"]), ("ag-7", "ag-7"))
        self.assertTrue(identity["GIT_AUTHOR_EMAIL"].startswith("ag-7@"))
        coordinator = SimpleNamespace(agent_id="coordinator", flavor="coordinator", platform_experiment_id="pe-1")
        self.assertEqual(core.git_identity(coordinator)["GIT_AUTHOR_NAME"], "coordinator-pe-1")


class ResearcherPromptTest(unittest.TestCase):
    # A researcher gated every job on a green local suite and an offline check, and submitted one
    # job in an hour with most of its capacity free.
    def test_jobs_are_the_only_evidence_and_a_local_build_is_the_only_check(self):
        cfg = SimpleNamespace(agent_id="ag-1", flavor="generalist", role="researcher", platform_experiment_id="pe-1",
                              hyperparameters={})
        prompt = " ".join(core.system_prompt(cfg, "rules", "commands", "https://git/x", "/app/workspace").split())
        self.assertIn("no offline estimate, local measurement, audit or test suite stands in for one", prompt)
        self.assertIn("a local build that runs is the only check before submitting", prompt)
        self.assertIn("A subagent submits as soon as its code builds", prompt)
        self.assertIn("its own jobs (`hl job submit`", prompt)
        self.assertIn("goes to every subagent it concerns", prompt)
        self.assertIn("keep several independent jobs in flight", prompt)

    # Subagents were 87% of a live run's spend: a lead resumed one with new work until it re-read
    # 457K on every call, and pinged live ones for progress about 25 times.
    def test_subagents_are_short_lived_and_quiet(self):
        cfg = SimpleNamespace(agent_id="ag-1", flavor="generalist", role="researcher", platform_experiment_id="pe-1",
                              hyperparameters={})
        prompt = " ".join(core.system_prompt(cfg, "rules", "commands", "https://git/x", "/app/workspace").split())
        self.assertIn("goes to a fresh subagent with its own brief, never a resumed one", prompt)
        self.assertIn("never a request for its progress", prompt)
        self.assertIn("report once, at its end", prompt)

    # A researcher's job gained 12% on an idea a person posted, tested under its id, and filed no
    # finding and no verdict, so the idea stayed open, and looked for a summary command it could
    # not find.
    def test_a_persons_idea_it_tested_is_its_to_summarize_and_judge(self):
        cfg = SimpleNamespace(agent_id="ag-1", flavor="generalist", role="researcher", platform_experiment_id="pe-1",
                              hyperparameters={})
        prompt = " ".join(core.system_prompt(cfg, "rules", "commands", "https://git/x", "/app/workspace").split())
        self.assertIn("(`hl job summary JOB --text T`)", prompt)
        self.assertIn("its summaries and its verdict are yours to give as on your own; the idea stays the person's", prompt)


class CloneWorkspaceTest(unittest.TestCase):
    # The CLI caches that its directory is no git repository: a researcher that cloned in its first
    # turn had every `isolation: "worktree"` subagent refused with "not in a git repository".
    def test_the_clone_is_there_before_the_cli_starts(self):
        with tempfile.TemporaryDirectory() as root:
            origin, workdir = os.path.join(root, "origin"), os.path.join(root, "workspace")
            os.makedirs(workdir)
            subprocess.run(["git", "init", "-q", origin], check=True)
            subprocess.run(["git", "-C", origin, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q",
                            "--allow-empty", "-m", "start"], check=True)
            core.clone_workspace(origin, workdir)
            self.assertTrue(os.path.isdir(os.path.join(workdir, ".git")))

    def test_a_workdir_in_use_or_a_failed_clone_is_left_as_it_is(self):
        with tempfile.TemporaryDirectory() as workdir:
            core.clone_workspace(os.path.join(workdir, "missing"), workdir)
            self.assertEqual(os.listdir(workdir), [])
            open(os.path.join(workdir, "work"), "w").close()
            core.clone_workspace("https://git/x", workdir)
            self.assertEqual(os.listdir(workdir), ["work"])

    def test_the_briefing_says_the_clone_is_made(self):
        cfg = SimpleNamespace(agent_id="ag-1", flavor="generalist", role="researcher", platform_experiment_id="pe-1",
                              hyperparameters={})
        prompt = " ".join(core.system_prompt(cfg, "rules", "commands", "https://git/x", "/app/workspace").split())
        self.assertIn("/app/workspace, the directory every command starts in, holds the clone of https://git/x", prompt)


if __name__ == "__main__":
    unittest.main()
