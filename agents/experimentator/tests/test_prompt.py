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
            self.assertIn("clone https://github.com/acme/model.git and work on branch `hl/pe-1/agent-a`", prompt)

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

    def test_the_briefing_hands_out_no_git_token(self):
        self.assertNotIn("GIT_TOKEN", self.render("generalist"))

    def test_the_briefing_names_hl_and_no_http_endpoint(self):
        prompt = self.render("generalist")
        self.assertIsNone(re.search(r"(GET|POST|PUT) /|/explore|openapi|\$API_URL", prompt))
        self.assertLess(len(prompt.encode()), 11_000)


if __name__ == "__main__":
    unittest.main()
