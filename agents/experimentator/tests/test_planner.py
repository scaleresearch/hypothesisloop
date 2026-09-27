import time
import unittest
from types import SimpleNamespace

from hypothesisloop_agent import core


def planner():
    return SimpleNamespace(agent_id="ag-planner", flavor="planner", platform_experiment_id="pe-1", hyperparameters={}, max_wall_hours=0)


class PlannerPromptTest(unittest.TestCase):
    def render(self) -> str:
        return core.system_prompt(planner(), "RULES\n", "COMMANDS", "https://github.com/acme/model.git", "/agent/workspace")

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
        self.assertIn("Where it is kept: run `git clone https://github.com/acme/model.git .` in /agent/workspace, the directory you start in", flat)
        self.assertIn("work on branch `hl/pe-1/ag-planner`: create it off the default branch", flat)
        self.assertIn("submit from your clone with `code_ref` left empty: hl pins the pushed HEAD", flat)
        # What a researcher is told of the others' branches is not true of a planner, whose
        # draft has no other agent.
        for absent in ("{", "====", "Others push", "BASELINE `code_ref`"):
            self.assertNotIn(absent, flat)
        self.assertLess(len(prompt.encode()), 11_000)

    # A planner cloned three times looking for its directory, read six sample directories to
    # learn the shape of a job spec, wrote one of its own beside the repository's, and named no
    # image in it. The repository's layout is one rule: hl repo init, fill in, hl repo check.
    def test_the_planner_makes_the_repository_ready_from_the_template(self):
        flat = " ".join(self.render().split())
        self.assertIn("never commit to the default branch", flat)
        self.assertIn("Run `hl repo init` in the clone before you write anything", flat)
        self.assertIn("keeps every file it holds", flat)
        self.assertIn("The job.yaml is the shape of a job spec and the one you fill in and submit: write no other, and read no sample", flat)
        self.assertIn("propose only when `hl repo check` passes", flat)
        self.assertIn('`hl job submit .hypothesisloop/job.yaml --hypothesis ID --theory "..."`', flat)
        self.assertEqual(flat.count("$WORKLOAD_SAMPLES"), 1)
        self.assertIn("`$WORKLOAD_SAMPLES/template/seed/metrics.py` does: copy that one file, and read nothing else there", flat)
        self.assertIn(["repo"], core._HL_HELP_PLANNER)

    # A planner whose proposal was not valid YAML believed a refused propose was costly, tried
    # to validate the file with tools its machine does not have, and sent it blind. The next one
    # spent six commands installing a parser for a file hl had already refused.
    def test_a_refused_propose_is_fixed_and_sent_again(self):
        flat = " ".join(self.render().split())
        self.assertIn("`hl` checks the YAML of every file it reads and names the line: install no parser", flat)
        self.assertIn("A refused propose stores nothing: fix what it names and send it again", flat)
        self.assertIn("a colon in unquoted text is not valid YAML", flat)

    # One planner tried confirmed against the baseline itself and was refused, another forced it
    # with --despite: a baseline is the control and has no verdict.
    def test_the_baselines_hypothesis_takes_no_verdict(self):
        self.assertIn("Its hypothesis states the control, which claims no effect: set no status on it", " ".join(self.render().split()))

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


class RunEndedTest(unittest.TestCase):
    def reason(self, cfg, signup, platform_experiment=None):
        client = SimpleNamespace(get_platform_experiment=lambda _id: platform_experiment, get_signup=lambda *_: signup)
        return core.stop_reason(SimpleNamespace(cfg=cfg, client=client), time.time(), [False])

    # The planner's machine is rented while it plans and no longer: it stops by itself once the
    # platform says its answer is stored, and a revision, which takes that back, lets it go on.
    def test_a_planner_stops_once_the_platform_has_its_answer(self):
        self.assertIsNone(self.reason(planner(), {}))
        self.assertEqual(self.reason(planner(), {"ended": "planned"}), "the platform ended the run: planned")
        self.assertEqual(self.reason(planner(), {"ended": "closed"}), "the platform ended the run: closed")

    # Researchers once finished in twenty minutes and their machines idled to the end time.
    def test_a_researcher_stops_once_it_finished_and_not_before(self):
        researcher = SimpleNamespace(agent_id="ag-1", flavor="generalist", platform_experiment_id="pe-1", max_wall_hours=0)
        self.assertIsNone(self.reason(researcher, {}))
        self.assertEqual(self.reason(researcher, {"ended": "finished"}), "the platform ended the run: finished")

    def test_a_coordinator_stops_once_its_platform_experiment_closed(self):
        coordinator = SimpleNamespace(agent_id="", flavor="coordinator", platform_experiment_id="pe-1", max_wall_hours=0)
        self.assertIsNone(self.reason(coordinator, None, {"status": "running"}))
        self.assertEqual(self.reason(coordinator, None, {"status": "closed"}), "the platform ended the run: closed")


if __name__ == "__main__":
    unittest.main()
