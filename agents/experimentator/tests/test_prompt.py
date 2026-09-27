import re
import unittest
from pathlib import Path
from types import SimpleNamespace

from hypothesisloop_agent import core

FLAVORS = sorted(p.stem for p in (Path(core.__file__).parent / "prompts" / "flavors").glob("*.md"))


class SystemPromptTest(unittest.TestCase):
    def render(self, flavor: str) -> str:
        cfg = SimpleNamespace(agent_id="agent-a", flavor=flavor, platform_experiment_id="pe-1", hyperparameters={"k": 1})
        return core.system_prompt(cfg, "RULES\n", "COMMANDS", "https://github.com/acme/model.git")

    def test_every_flavor_renders_with_the_rules_and_the_hl_commands(self):
        for flavor in FLAVORS:
            prompt = self.render(flavor)
            self.assertIn("\n\nRULES\n\n## hl commands\n\nCOMMANDS\n\n", prompt)
            flat = " ".join(prompt.split())
            # Five of six hosted agents cloned into /tmp and then fought the working directory
            # every command starts in.
            self.assertIn("clone https://github.com/acme/model.git into the directory you start in", flat)
            self.assertIn("work on branch `hl/pe-1/agent-a`", flat)
            # Told to branch off the default branch, a researcher never met the planner's seed,
            # and the `hl-clone` a brief sent it to leaves a directory with no origin to push to.
            self.assertIn("create it off the commit after `@` in the description's BASELINE `code_ref` "
                          "(`git checkout -b <branch> <sha>`), or off the default branch when it names none", flat)
            self.assertLess(len(prompt.encode()), 11_000)

    # Six hosted agents were told by a brief to block in `hl watch --until` and all did, for the
    # whole job: the turn stayed in flight, so a frozen agent had no stored session to resume.
    def test_waiting_for_a_job_is_ending_the_turn_whatever_a_brief_says(self):
        prompt = " ".join(self.render("generalist").split())
        self.assertIn("A brief that tells you to wait for a job means exactly that, and never a blocking `hl watch`", prompt)
        self.assertIn("end your turn", prompt)

    # Four of six hosted agents wrote a job's result twice, as its summary and again as a comment
    # headed "Finding:", because the briefing used both words for it.
    def test_a_jobs_summary_is_its_finding_and_a_comment_is_for_what_has_no_job(self):
        prompt = " ".join(self.render("generalist").split())
        self.assertIn("a job's *summary*, which is its finding", prompt)
        self.assertIn("Never repeat it as a comment", prompt)
        self.assertIn("a *comment* on a hypothesis: only what has no job behind it", prompt)

    # Every agent's commits carried the image's one name, so a branch's history did not say who
    # wrote what.
    def test_commits_carry_the_agents_own_id(self):
        agent = SimpleNamespace(agent_id="ag-7", flavor="generalist", platform_experiment_id="pe-1")
        identity = core.git_identity(agent)
        self.assertEqual((identity["GIT_AUTHOR_NAME"], identity["GIT_COMMITTER_NAME"]), ("ag-7", "ag-7"))
        self.assertTrue(identity["GIT_AUTHOR_EMAIL"].startswith("ag-7@"))
        coordinator = SimpleNamespace(agent_id="coordinator", flavor="coordinator", platform_experiment_id="pe-1")
        self.assertEqual(core.git_identity(coordinator)["GIT_AUTHOR_NAME"], "coordinator-pe-1")

    def test_the_briefing_names_hl_and_no_http_endpoint(self):
        prompt = self.render("generalist")
        self.assertIsNone(re.search(r"(GET|POST|PUT) /|/explore|openapi|\$API_URL", prompt))

    # The variant for a platform experiment with no repository replaces the paragraphs about the
    # repository and adds none, so it fits the same limit.
    def test_with_no_repository_the_briefing_is_no_longer_and_names_no_remote(self):
        for flavor in FLAVORS:
            cfg = SimpleNamespace(agent_id="agent-a", flavor=flavor, platform_experiment_id="pe-1", hyperparameters={"k": 1})
            prompt = core.system_prompt(cfg, "RULES\n", "COMMANDS", "")
            self.assertLessEqual(len(prompt.encode()), len(self.render(flavor).encode()))
            flat = " ".join(prompt.split())
            self.assertIn("submit from there with `code_ref` left empty: hl pins the HEAD you pushed", flat)
            self.assertIn("`hl-clone <code_ref of a job> <dir>`", flat)
            for absent in ("git push", "branch `hl/", "{repository", "===="):
                self.assertNotIn(absent, flat)


if __name__ == "__main__":
    unittest.main()
