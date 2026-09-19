import re
import unittest
from pathlib import Path
from types import SimpleNamespace

from hypothesisloop_agent import core

FLAVORS = sorted(p.stem for p in (Path(core.__file__).parent / "prompts" / "flavors").glob("*.md"))


class SystemPromptTest(unittest.TestCase):
    def render(self, flavor: str) -> str:
        cfg = SimpleNamespace(agent_id="agent-a", flavor=flavor, platform_experiment_id="pe-1",
                              code_repo_url="git://repo", hyperparameters={"k": 1})
        return core.system_prompt(cfg, "RULES\n", "COMMANDS")

    def test_every_flavor_renders_with_the_rules_and_the_hl_commands(self):
        for flavor in FLAVORS:
            prompt = self.render(flavor)
            self.assertIn("\n\nRULES\n\n## hl commands\n\nCOMMANDS\n\n", prompt)
            self.assertIn("branch `agent-a-pe-1`", prompt)

    def test_the_briefing_names_hl_and_no_http_endpoint(self):
        prompt = self.render("generalist")
        self.assertIsNone(re.search(r"(GET|POST|PUT) /|/explore|openapi|\$API_URL", prompt))
        self.assertLess(len(prompt.encode()), 11_000)


if __name__ == "__main__":
    unittest.main()
