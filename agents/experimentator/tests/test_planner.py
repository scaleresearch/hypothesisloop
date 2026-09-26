import time
import unittest
from types import SimpleNamespace

from hypothesisloop_agent import core


def planner():
    return SimpleNamespace(agent_id="ag-planner", flavor="planner", platform_experiment_id="pe-1", hyperparameters={}, max_wall_hours=0)


class PlannerPromptTest(unittest.TestCase):
    def render(self) -> str:
        return core.system_prompt(planner(), "RULES\n", "COMMANDS", "")

    # The planner has its own briefing: the researchers' one tells an agent to compete, and a
    # planner that competed would optimize a metric nobody had chosen yet.
    def test_the_planner_is_briefed_to_propose_and_not_to_compete(self):
        prompt = self.render()
        flat = " ".join(prompt.split())
        self.assertIn("\n\nRULES\n\n## hl commands\n\nCOMMANDS\n\n", prompt)
        self.assertIn("agent_id: ag-planner", flat)
        self.assertIn("`hl platform-experiments propose`", flat)
        self.assertNotIn("Every agent competes", flat)
        for word in ("One ranked metric that is the goal itself", "role `constraint`", "BASELINE block", "`hl job summary`",
                     "With revisions, the last one is what the person wants now"):
            self.assertIn(word, flat)
        self.assertLess(len(prompt.encode()), 11_000)

    # A draft names no repository, so the planner's code goes to the platform like any agent's
    # there, in the words the researchers' briefing uses.
    def test_the_planners_code_is_kept_by_the_platform(self):
        flat = " ".join(self.render().split())
        self.assertIn("`hl code push` stores your committed HEAD with the platform", flat)
        self.assertIn("submit from there with `code_ref` left empty: hl pins the HEAD you pushed", flat)
        for absent in ("git push", "branch `hl/", "{", "===="):
            self.assertNotIn(absent, flat)

    # Six agents once blocked in `hl watch` for a whole job, and none had a session to resume.
    def test_waiting_for_the_baseline_is_ending_the_turn(self):
        self.assertIn("end your turn: its result arrives as a message. Never poll, sleep or block in `hl watch`", " ".join(self.render().split()))

    def test_the_planner_is_listed_the_commands_it_alone_uses(self):
        self.assertIn(["platform-experiments", "propose"], core._HL_HELP_PLANNER)
        self.assertIn(["code"], core._HL_HELP_PLANNER)


class PlannerStopTest(unittest.TestCase):
    def reason(self, cfg, platform_experiment):
        client = SimpleNamespace(get_platform_experiment=lambda _id: platform_experiment)
        return core.stop_reason(SimpleNamespace(cfg=cfg, client=client), time.time(), [False])

    # The planner's machine is rented while it plans and no longer: it stops by itself once the
    # proposal is stored, and a revision, which clears proposed_at, lets it go on.
    def test_a_planner_stops_once_its_draft_is_proposed_or_accepted(self):
        self.assertIsNone(self.reason(planner(), {"status": "draft"}))
        self.assertIsNone(self.reason(planner(), {"status": "draft", "proposed_at": None, "proposal": {"x": 1}}))
        self.assertEqual(self.reason(planner(), {"status": "draft", "proposed_at": "2026-01-01T00:00:00Z"}), "the draft is proposed")
        self.assertEqual(self.reason(planner(), {"status": "open"}), "the draft is proposed")
        self.assertEqual(self.reason(planner(), {"status": "closed"}), "platform experiment closed")

    def test_a_researcher_does_not_stop_in_an_open_experiment(self):
        researcher = SimpleNamespace(agent_id="ag-1", flavor="generalist", platform_experiment_id="pe-1", max_wall_hours=0)
        self.assertIsNone(self.reason(researcher, {"status": "open"}))
        self.assertIsNone(self.reason(researcher, {"status": "running"}))


if __name__ == "__main__":
    unittest.main()
