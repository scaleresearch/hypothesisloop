import time
import unittest
from types import SimpleNamespace

from hypothesisloop_agent import core


def planner():
    return SimpleNamespace(agent_id="ag-planner", flavor="planner", platform_experiment_id="pe-1", hyperparameters={}, max_wall_hours=0)


class PlannerPromptTest(unittest.TestCase):
    def render(self) -> str:
        return core.system_prompt(planner(), "RULES\n", "COMMANDS", "https://github.com/acme/model.git")

    # The planner has its own briefing: the researchers' one tells an agent to compete, and a
    # planner that competed would optimize a metric nobody had chosen yet.
    def test_the_planner_is_briefed_to_propose_and_not_to_compete(self):
        prompt = self.render()
        flat = " ".join(prompt.split())
        self.assertIn("\n\nRULES\n\n## hl commands\n\nCOMMANDS\n\n", prompt)
        self.assertIn("agent_id: ag-planner", flat)
        self.assertIn("`hl platform-experiments propose`", flat)
        # Every proposal of two runs asked for 3 researchers over 3 hours for goals met in 40 minutes.
        self.assertIn("the smallest plan that can reach the goal", flat)
        self.assertIn("Write the arithmetic in `sizing_reason`", flat)
        self.assertIn("`hl job cost` shows what the platform billed", flat)
        self.assertNotIn("Every agent competes", flat)
        for word in ("One ranked metric that is the goal itself", "role `constraint`", "BASELINE block", "`hl job summary`",
                     "With revisions, the last one is what the person wants now"):
            self.assertIn(word, flat)
        self.assertLess(len(prompt.encode()), 11_000)

    # It reads where its code goes in the words the researchers' briefing uses.
    def test_the_planners_code_goes_to_the_drafts_repository(self):
        prompt = self.render()
        flat = " ".join(prompt.split())
        self.assertIn("Where it is kept: clone https://github.com/acme/model.git into the directory you start in", flat)
        self.assertIn("work on branch `hl/pe-1/ag-planner`: create it off the default branch", flat)
        self.assertIn("submit from your clone with `code_ref` left empty: hl pins the pushed HEAD", flat)
        # What a researcher is told of the others' branches and of a base job spec is not true
        # of a planner, whose repository holds one commit and whose draft has no other agent.
        for absent in ("{", "====", "Others push", "job.yaml", "BASELINE `code_ref`"):
            self.assertNotIn(absent, flat)
        self.assertLess(len(prompt.encode()), 11_000)

    # Six agents once blocked in `hl watch` for a whole job, and none had a session to resume.
    def test_waiting_for_the_baseline_is_ending_the_turn(self):
        self.assertIn("end your turn: its result arrives as a message. Never poll, sleep or block in `hl watch`", " ".join(self.render().split()))

    # A goal the planner could not plan left a draft that closed with nothing said. And work for
    # a CPU states only its cores, with a budget that reads as CPU core-hours.
    def test_the_planner_declines_with_a_reason_and_sizes_cpu_work_in_cores(self):
        flat = " ".join(self.render().split())
        self.assertIn('Decline with `hl platform-experiments decline --reason "..."`', flat)
        self.assertIn("its job states only `cpu: N`, whole cores", flat)
        self.assertIn("the budget are then CPU core-hours: 4 cores for an hour cost 4", flat)
        self.assertNotIn("none for work a CPU does", flat)

    def test_the_planner_is_listed_the_commands_it_alone_uses(self):
        self.assertIn(["platform-experiments", "decline"], core._HL_HELP_PLANNER)
        self.assertIn(["platform-experiments", "propose"], core._HL_HELP_PLANNER)
        self.assertIn(["job", "submit"], core._HL_HELP_PLANNER)


class PlannerStopTest(unittest.TestCase):
    signup: dict = {}

    def reason(self, cfg, platform_experiment):
        client = SimpleNamespace(get_platform_experiment=lambda _id: platform_experiment, get_signup=lambda *_: self.signup)
        return core.stop_reason(SimpleNamespace(cfg=cfg, client=client), time.time(), [False])

    # The planner's machine is rented while it plans and no longer: it stops by itself once the
    # proposal is stored, and a revision, which clears proposed_at, lets it go on.
    def test_a_planner_stops_once_its_draft_is_proposed_or_accepted(self):
        self.assertIsNone(self.reason(planner(), {"status": "draft"}))
        self.assertIsNone(self.reason(planner(), {"status": "draft", "proposed_at": None, "proposal": {"x": 1}}))
        self.assertEqual(self.reason(planner(), {"status": "draft", "proposed_at": "2026-01-01T00:00:00Z"}), "the draft is answered")
        self.assertEqual(self.reason(planner(), {"status": "draft", "proposed_at": "2026-01-01T00:00:00Z", "declined_reason": "nothing to measure"}), "the draft is answered")
        self.assertEqual(self.reason(planner(), {"status": "open"}), "the draft is answered")
        self.assertEqual(self.reason(planner(), {"status": "closed"}), "platform experiment closed")

    # Researchers once finished in twenty minutes and their machines idled to the end time.
    def test_a_researcher_stops_once_it_finished_and_not_before(self):
        researcher = SimpleNamespace(agent_id="ag-1", flavor="generalist", platform_experiment_id="pe-1", max_wall_hours=0)
        self.assertIsNone(self.reason(researcher, {"status": "open"}))
        self.assertIsNone(self.reason(researcher, {"status": "running"}))
        self.signup = {"finished_at": "2026-01-01T00:20:00Z"}
        self.assertEqual(self.reason(researcher, {"status": "running"}), "the agent finished")


if __name__ == "__main__":
    unittest.main()
