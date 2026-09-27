import time
import unittest
from unittest import mock
from types import SimpleNamespace

from hypothesisloop_agent import core


def planner():
    return SimpleNamespace(agent_id="ag-planner", flavor="planner", role="planner", platform_experiment_id="pe-1", hyperparameters={}, max_wall_hours=0)


def baseline_agent():
    return SimpleNamespace(agent_id="ag-baseline", flavor="generalist", role="baseline", platform_experiment_id="pe-1", hyperparameters={}, max_wall_hours=0)


def render(cfg) -> tuple[str, str]:
    prompt = core.system_prompt(cfg, "RULES\n", "COMMANDS", "https://github.com/acme/model.git", "/agent/workspace")
    return prompt, " ".join(prompt.split())


def listed(cfg) -> list:
    asked = []
    with mock.patch.object(core.subprocess, "run", lambda command, **_: asked.append(command[1:-1]) or mock.Mock(stdout="usage\n")):
        core.hl_commands(cfg)
    return asked


class PlannerPromptTest(unittest.TestCase):
    # The planner has its own briefing: the researchers' one tells an agent to compete, and a
    # planner that competed would optimize a metric nobody had chosen yet.
    def test_the_planner_is_briefed_to_propose_and_not_to_compete(self):
        prompt, flat = render(planner())
        self.assertIn("\n\nRULES\n\n## hl commands\n\nCOMMANDS\n\n", prompt)
        self.assertIn("agent_id: ag-planner", flat)
        self.assertIn("`hl platform-experiments propose`", flat)
        # Every proposal of two runs asked for 3 researchers over 3 hours for goals met in 40 minutes.
        self.assertIn("the smallest plan that can reach the goal", flat)
        self.assertNotIn("Every agent competes", flat)
        for word in ("One ranked metric that is the goal itself", "role `constraint`", "BASELINE block",
                     "With revisions, the last one is what the person wants now"):
            self.assertIn(word, flat)
        self.assertNotIn("{", flat)

    # Planners cloned, wrote seed code, ran a baseline job and waited for it before a person had
    # agreed to anything: 16 to 20 minutes a draft. A planner writes the definition and runs
    # nothing, so its briefing is half of what it was and lists no command that runs something.
    def test_the_planner_defines_the_experiment_and_runs_nothing(self):
        prompt, flat = render(planner())
        self.assertIn("You run nothing: no clone, no code, no job, no hypothesis", flat)
        for absent in ("git clone", "hl repo init", "hl job submit", "hl job summary", "$WORKLOAD_SAMPLES", "hl quota"):
            self.assertNotIn(absent, flat)
        self.assertLess(len(prompt.encode()), 5_500)
        self.assertEqual(listed(planner()), [[], ["platform-experiments", "propose"], ["platform-experiments", "decline"]])

    # Nothing has run when a planner proposes, so what it states of the baseline and of the cost
    # is what is not known yet, said as such.
    def test_the_proposal_states_an_unmeasured_baseline_and_an_estimated_size(self):
        flat = render(planner())[1]
        self.assertIn("`config:` the exact control configuration, and `measured: not yet established`", flat)
        self.assertIn("`sizing_reason`, worded as the estimate it is: nothing has run; the trial length is assumed", flat)
        self.assertIn("the budget is a cap", flat)
        self.assertIn("the run does not start if the budget cannot cover one trial for each researcher", flat)
        self.assertIn("`accelerator_type`: one line of `hl capacity` with available > 0, copied verbatim", flat)

    # A gate whose bound the control fails disqualifies everyone, and no baseline exists to
    # check a proposed bound against.
    def test_a_gate_holds_for_the_control_by_construction_or_is_a_question(self):
        flat = render(planner())[1]
        self.assertIn("Its `bound` must hold for the unchanged control by construction", flat)
        self.assertIn("A bound that depends on a number nobody has measured is no gate yet: leave it out and put it in `questions`", flat)

    # A planner whose proposal was not valid YAML believed a refused propose was costly, tried
    # to validate the file with tools its machine does not have, and sent it blind. The next one
    # spent six commands installing a parser for a file hl had already refused.
    def test_a_refused_propose_is_fixed_and_sent_again(self):
        flat = render(planner())[1]
        self.assertIn("`hl` checks the YAML and names the line: install no parser", flat)
        self.assertIn("A refused propose stores nothing: fix what it names and send it again", flat)
        self.assertIn("a colon in unquoted text is not valid YAML", flat)

    # A goal the planner could not plan left a draft that closed with nothing said. And work for
    # a CPU has a budget that reads as CPU core-hours.
    def test_the_planner_declines_with_a_reason_and_sizes_cpu_work_in_cores(self):
        flat = render(planner())[1]
        self.assertIn('Decline with `hl platform-experiments decline --reason "..."`', flat)
        self.assertIn("takes the type whose `kind` is cpu; its budget is CPU core-hours", flat)


class BaselinePromptTest(unittest.TestCase):
    # The agent that measures the baseline is of role baseline, as a planner is in the roster:
    # the flavor tells the two apart. It builds what the planner once built, after a person
    # accepted, and competes no more than a planner does.
    def test_the_baseline_agent_is_briefed_to_measure_the_control_and_finish(self):
        prompt, flat = render(baseline_agent())
        self.assertIn("\n\nRULES\n\n## hl commands\n\nCOMMANDS\n\n", prompt)
        self.assertIn("You are the baseline agent", flat)
        self.assertIn("agent_id: ag-baseline", flat)
        self.assertIn("You do not compete and you try no idea", flat)
        self.assertIn('`hl platform-experiments agents finish --reason "baseline measured by job ID"`', flat)
        self.assertNotIn("Every agent competes", flat)
        self.assertNotIn("hl platform-experiments propose", flat)
        self.assertLess(len(prompt.encode()), 5_500)
        self.assertEqual(listed(baseline_agent()), [[], ["job"], ["job", "submit"], ["hypothesis"], ["repo"]])

    # It reads where its code goes in the words the researchers' briefing uses.
    def test_its_code_goes_to_the_experiments_repository(self):
        flat = render(baseline_agent())[1]
        self.assertIn("Where it is kept: run `git clone https://github.com/acme/model.git .` in /agent/workspace, the directory you start in", flat)
        self.assertIn("work on branch `hl/pe-1/ag-baseline`: create it off the default branch", flat)
        self.assertIn("submit from your clone with `code_ref` left empty: hl pins the pushed HEAD", flat)
        # What a researcher is told of the others' branches is not true of the agent that
        # starts the code.
        for absent in ("{", "====", "Others push", "BASELINE `code_ref`"):
            self.assertNotIn(absent, flat)

    # A planner cloned three times looking for its directory, read six sample directories to
    # learn the shape of a job spec, wrote one of its own beside the repository's, and named no
    # image in it. The repository's layout is one rule: hl repo init, fill in, hl repo check.
    # A repository that is ready and fits runs as it is.
    def test_it_makes_the_repository_ready_from_the_template_or_runs_one_that_is(self):
        flat = render(baseline_agent())[1]
        self.assertIn("never commit to the default branch", flat)
        self.assertIn("Run `hl repo init` in the clone before you write anything", flat)
        self.assertIn("keeps every file it holds", flat)
        self.assertIn("The job.yaml is the shape of a job spec and the one you fill in and submit: write no other, and read no sample", flat)
        self.assertIn("submit only when `hl repo check` passes", flat)
        self.assertIn("If `hl repo check` passes and the repository's harness already measures these metrics on this config, change nothing and go to 3", flat)
        self.assertIn('`hl job submit .hypothesisloop/job.yaml --hypothesis ID --theory "..."`', flat)
        self.assertEqual(flat.count("$WORKLOAD_SAMPLES"), 1)
        self.assertIn("`$WORKLOAD_SAMPLES/template/seed/metrics.py` does: copy that one file, and read nothing else there", flat)

    # One planner tried confirmed against the baseline itself and was refused, another forced it
    # with --despite: a baseline is the control and has no verdict.
    def test_the_baselines_hypothesis_takes_no_verdict(self):
        self.assertIn("Its hypothesis states the control, which claims no effect: set no status on it", render(baseline_agent())[1])

    # Told to hold the baseline job to its own estimate, both planners of a run ran the baseline
    # a second time only to carry a corrected one: 2 to 4 of their 16 to 18 minutes.
    def test_the_baseline_runs_once_on_a_guessed_estimate(self):
        flat = render(baseline_agent())[1]
        self.assertIn("`--estimated-duration-hours` your guess, the committed file unedited", flat)
        self.assertIn("The file states no estimate", flat)
        self.assertIn("Never run a good baseline a second time", flat)

    # Six agents once blocked in `hl watch` for a whole job, and none had a session to resume.
    def test_waiting_for_the_baseline_is_ending_the_turn(self):
        self.assertIn("end your turn: the result arrives as a message. Never poll, sleep or block in `hl watch`", render(baseline_agent())[1])

    # A baseline that cannot be measured must not hold the experiment open until its limit.
    def test_a_baseline_that_cannot_be_measured_is_given_up_with_a_reason(self):
        flat = render(baseline_agent())[1]
        self.assertIn("If three jobs failed, or one stays queued with `capacity_unavailable` or `no_cluster_for_image`", flat)
        self.assertIn("finish with the reason", flat)


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

    # A revision reached a planner as "The machine you ran on was replaced ... Continue where you
    # left off", and it had to find in the draft what the person wanted.
    def test_a_revision_is_what_a_resumed_planner_is_told(self):
        from hypothesisloop_agent.claude import run
        asked = [{"message": "use two cores"}, {"message": "Keep the bar as I wrote it."}]
        draft = {"status": "draft", "revisions": asked}

        def setup(cfg, platform_experiment):
            return SimpleNamespace(cfg=cfg, client=SimpleNamespace(get_platform_experiment=lambda _id: platform_experiment))

        with mock.patch.object(run, "_on_new_machine", True):
            prompt = run._first_prompt(setup(planner(), draft), "s-1", None)
            self.assertIn("asks for this revision:\n\nKeep the bar as I wrote it.\n\n", prompt)
            self.assertIn("a file you wrote on it is gone", prompt)
            for absent in ("use two cores", "was replaced", "Continue where you left off", "branch"):
                self.assertNotIn(absent, prompt)
            self.assertEqual(run._first_prompt(setup(planner(), draft), None, None), run._new_machine_note(planner()) + "\n\nBegin.")
            self.assertTrue(run._first_prompt(setup(planner(), {"status": "draft"}), "s-1", None).endswith("Continue where you left off."))
            self.assertTrue(run._first_prompt(setup(baseline_agent(), draft), "s-1", None).endswith("Continue where you left off."))
        self.assertNotIn("gone", run._first_prompt(setup(planner(), draft), "s-1", None))

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
