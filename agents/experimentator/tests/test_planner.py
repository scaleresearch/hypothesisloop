import os
import tempfile
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
        # A goal that said about 0.1 accelerator-hours was planned with a budget of 0.06.
        self.assertIn("The size: a budget, duration or count the goal states, as stated; else the smallest plan", flat)
        # A draft's max_agents is the most a proposal may ask for, which one read as the number.
        self.assertIn("2 unless plainly more, never above the draft's `max_agents`, a ceiling", flat)
        self.assertNotIn("Every agent competes", flat)
        for word in ("One ranked metric that is the goal itself", "role `constraint`",
                     "With revisions, the last one is what the person wants now"):
            self.assertIn(word, flat)
        self.assertNotIn("{", flat)

    # Planners cloned, wrote seed code, ran a baseline job and waited for it before a person had
    # agreed to anything: 16 to 20 minutes a draft. A planner writes the definition and runs
    # nothing, so its briefing is half of what it was and lists no command that runs something.
    def test_the_planner_defines_the_experiment_and_runs_nothing(self):
        prompt, flat = render(planner())
        self.assertIn("You define the experiment and run nothing: you read, then send one `propose` or `decline`", flat)
        # One measured scikit-learn on its own machine for 17 minutes, through three command
        # timeouts, to learn whether the person's gate could be reached, and declined on that.
        self.assertIn("Whether a gate can be reached is its to measure: state a doubt in `risks` and plan what the person asked", flat)
        self.assertIn("A bound the person stated stays as stated", flat)
        self.assertIn("A gate you doubt is a risk and no ground to decline", flat)
        # Its brief named the image by a word of its own and a `uv pip install` that failed in
        # the job: three baseline jobs went to those two lines. A proposal now states its image
        # and is refused for one no job could be submitted with.
        self.assertIn("the accelerator type, the count or cores per job, and the `image` you propose", flat)
        self.assertNotIn("job_images", flat)
        self.assertIn("prescribe no install command: you have run none", flat)
        for absent in ("hl repo init", "hl job submit", "hl job summary", "$WORKLOAD_SAMPLES", "hl quota"):
            self.assertNotIn(absent, flat)
        self.assertLess(len(prompt.encode()), 4_530)
        self.assertEqual(listed(planner()), [[], ["platform-experiments", "propose"], ["platform-experiments", "decline"]])

    # Told not to clone, a planner invented override flags and a gate for a harness the
    # repository held, and asked for 32 core-hours where its README said a trial takes 90 s.
    def test_the_planner_reads_the_repository_and_sizes_from_what_it_holds(self):
        flat = render(planner())[1]
        self.assertIn("Read the repository too: `git clone --depth 1 https://github.com/acme/model.git .` (`--branch B` when the goal names one), "
                      "its README and `.hypothesisloop/`", flat)
        self.assertIn("A harness it holds for the goal is the experiment: state its files, levers, metric keys, gate, control and job spec as they are, "
                      "and invent none", flat)
        self.assertIn("Budget = the trial length the repository states, else one you assume, x", flat)

    # Nothing has run when a planner proposes, so what it states of the cost is what is not
    # known yet, said as such.
    def test_the_proposal_states_an_estimated_size(self):
        flat = render(planner())[1]
        self.assertIn("`sizing_reason`: nothing has run; the trial length is read or assumed", flat)
        self.assertIn("the budget is a cap", flat)
        self.assertIn("the run does not start if the budget cannot cover one trial for each researcher", flat)
        self.assertIn("`accelerator_type`: a line of `hl capacity` with available > 0", flat)

    # A gate whose bound the control fails disqualifies everyone, and no baseline exists to
    # check a proposed bound against.
    def test_a_gate_whose_bound_nobody_measured_is_a_question(self):
        flat = render(planner())[1]
        self.assertIn("A bound that depends on a number nobody has measured is no gate yet: leave it out and put it in `questions`", flat)

    # A goal the planner could not plan left a draft that closed with nothing said. And work for
    # a CPU has a budget that reads as CPU core-hours.
    def test_the_planner_declines_with_a_reason_and_sizes_cpu_work_in_cores(self):
        flat = render(planner())[1]
        self.assertIn("decline with `hl platform-experiments decline`", flat)
        self.assertIn("The budget of the type whose `kind` is cpu is CPU core-hours", flat)

    # The help of propose and of decline, listed in the briefing, gives the shape of the file,
    # how a free-text value is written, that a refusal names the line and stores nothing, and
    # what a reason says.
    def test_the_briefing_leaves_to_the_help_of_propose_what_it_says(self):
        flat = render(planner())[1]
        for said_there in ("--help", "parser", "`|` block", "A refused propose", "measured: not yet established", "out_of_scope"):
            self.assertNotIn(said_there, flat)


class BaselinePromptTest(unittest.TestCase):
    # The agent that measures the baseline is of role baseline. It builds what the planner once
    # built, after a person accepted, and competes no more than a planner does.
    def test_the_baseline_agent_is_briefed_to_measure_the_control_and_finish(self):
        prompt, flat = render(baseline_agent())
        self.assertIn("\n\nRULES\n\n## hl commands\n\nCOMMANDS\n\n", prompt)
        self.assertIn("You are the baseline agent", flat)
        self.assertIn("agent_id: ag-baseline", flat)
        self.assertIn("You do not compete and you try no idea", flat)
        self.assertIn('`hl platform-experiments agents finish --reason "baseline measured by job ID"`', flat)
        self.assertNotIn("Every agent competes", flat)
        self.assertNotIn("hl platform-experiments propose", flat)
        self.assertLess(len(prompt.encode()), 3_700)
        self.assertEqual(listed(baseline_agent()), [[], ["job"], ["job", "submit"], ["repo"]])

    # It reads where its code goes in the words the researchers' briefing uses.
    def test_its_code_goes_to_the_experiments_repository(self):
        flat = render(baseline_agent())[1]
        self.assertIn("Where it is kept: run `git clone https://github.com/acme/model.git .` in /agent/workspace, the directory you start in", flat)
        self.assertIn("Work on branch `hl/pe-1/baseline` (`git checkout -b` it once): `hl job submit` pushes it", flat)
        self.assertNotIn("push and submit", flat)
        # What a researcher is told of the others' branches is not true of the agent that
        # starts the code.
        for absent in ("{", "====", "hl hypothesis checkout", "ag-baseline`"):
            self.assertNotIn(absent, flat)

    # A planner cloned three times looking for its directory, read six sample directories to
    # learn the shape of a job spec, wrote one of its own beside the repository's, and named no
    # image in it. The repository's layout is one rule: hl repo init, fill in, hl repo check.
    # A repository that is ready and fits runs as it is.
    def test_it_makes_the_repository_ready_from_the_template_or_runs_one_that_is(self):
        flat = render(baseline_agent())[1]
        self.assertIn("Run `hl repo init` in the clone before you write anything", flat)
        self.assertIn("keeps every file it holds", flat)
        self.assertIn("The job.yaml it gives is the one you fill in and submit, once `hl repo check` passes", flat)
        self.assertIn("Only a harness that already measures these metrics on this config runs unchanged: go to 3", flat)
        # `hl repo check` passed a repository whose files described a compression benchmark,
        # straight after `hl repo init` had kept all three. It now holds them to the platform
        # experiment, so the briefing asks only that it pass.
        self.assertIn("once `hl repo check` passes: it names what a file still lacks and where one does not fit this experiment", flat)
        self.assertNotIn("earlier experiment", flat)
        self.assertIn("`hl job submit .hypothesisloop/job.yaml --estimated-duration-hours H`, H a generous guess", flat)
        # The repository template supplies the helper, so no briefing sends an agent to the samples.
        self.assertNotIn("$WORKLOAD_SAMPLES", flat)
        self.assertIn("once more at the end with the `post_metric` of the repository's `metrics.py`", flat)

    # The control was a root hypothesis that stayed open to the end, and in one run the parent
    # of every other: the baseline is the objective's own, and a submit that names one is
    # refused with that.
    def test_the_baseline_takes_no_hypothesis(self):
        flat = render(baseline_agent())[1]
        self.assertNotIn("--hypothesis", flat)
        self.assertNotIn("--theory", flat)

    # Told to hold the baseline job to its own estimate, both planners of a run ran the baseline
    # a second time only to carry a corrected one: 2 to 4 of their 16 to 18 minutes.
    def test_the_baseline_runs_once_on_a_guessed_estimate(self):
        flat = render(baseline_agent())[1]
        # The baseline left "--estimated-duration-hours H" in the job spec's header, so both
        # researchers stated estimates of two and six times the measured one, which never applied.
        # Told that the file states none, one committed `estimated_duration_hours: 0.06`, and
        # every researcher's job carried it in place of the measured one. `hl repo check` fails
        # a job spec that states one, so the briefing says nothing of it.
        self.assertNotIn("estimate into the file", flat)
        self.assertIn("(the job.yaml submitted unedited and the `--env` that vary it)", flat)

    def test_waiting_for_the_baseline_is_ending_the_turn(self):
        flat = render(baseline_agent())[1]
        # Agents of one run ended seven turns with `true` or an `echo`, each between what they
        # had written and the one-line reply the briefing asked a turn to end with.
        self.assertIn("A reply with no command in it ends your turn, and the next update starts another. "
                      "A job that failed", flat)
        self.assertNotIn("hl watch", flat)
        self.assertIn("A reply with no command in it ends your turn, and a revision starts another.", render(planner())[1])

    # A baseline that cannot be measured must not hold the experiment open until its limit.
    # Told to give up after three failed jobs, one finished with the fix for the third already
    # pushed, and its successor took 4.5 minutes to run that fix.
    def test_a_failed_job_with_a_known_fix_is_run_again_and_one_failure_three_times_over_ends_it(self):
        flat = render(baseline_agent())[1]
        self.assertIn("has its cause in `hl job logs ID`: fix it, commit and submit again, as often as you know a fix", flat)
        self.assertIn("once you can say why no baseline is possible: one failure came back three times through your fixes, "
                      "or a job stays queued with `capacity_unavailable` or `no_cluster_for_image`", flat)
        self.assertIn("the coordinator is sent it and acts on it", flat)
        self.assertNotIn("three jobs failed", flat)

    # The help of hl job submit, listed in its briefing, and the job.yaml hl repo init adds
    # both give the command that runs the submitted commit and how a job installs a package.
    def test_how_a_job_gets_its_code_is_left_to_the_help_of_submit_and_the_job_spec(self):
        flat = render(baseline_agent())[1]
        for said_there in ("hl-clone", "pip install"):
            self.assertNotIn(said_there, flat)

    # One spent twelve minutes proving a GPU harness on its own machine: a three-minute timeout,
    # four slow reruns on CPU and two failed installs of torch.
    def test_the_harness_is_tested_by_the_baseline_job_and_not_on_the_agents_machine(self):
        self.assertIn("This machine has no accelerator and no framework: check syntax and a tiny dry path only. "
                      "The baseline job is the test", render(baseline_agent())[1])

    # A baseline agent stopped at the close answered its last turn with a finish, which the
    # platform refuses: the close had ended its run.
    def test_a_refusal_for_a_closed_experiment_ends_the_last_turn(self):
        from hypothesisloop_agent.claude import run
        self.assertIn("A refusal with reason `experiment_closed` ends it too: your run is over, so retry nothing", run._SHUTDOWN_PROMPT)

    # hl once said an agent's next step on stderr beside the record. The record's
    # `status_reason` says it now, and nothing else does.
    def test_its_next_step_is_read_from_status_reason(self):
        self.assertIn("its `metrics` and `status_reason`, your next step", render(baseline_agent())[1])
        for cfg in (planner(), baseline_agent()):
            self.assertNotIn("2>&1", render(cfg)[0])

    # A chip job that left out cpu and memory held a whole node. A submit without one of the
    # three, or with a share of the node for one, is refused with the field: how much to state
    # is the agent's to judge.
    def test_the_job_spec_is_sized_to_what_the_job_uses(self):
        flat = render(baseline_agent())[1]
        self.assertIn("`cpu`, `memory` and `storage` sized to what the job uses, so that other jobs fit beside it", flat)
        for said_there in ("--help", "is refused", "experiment_closed", "pip install X` and", "default branch"):
            self.assertNotIn(said_there, flat)


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
        draft = {"status": "draft", "revisions": asked, "status_reason": {"reason": "revising"}}

        def setup(cfg, platform_experiment):
            return SimpleNamespace(cfg=cfg, client=SimpleNamespace(get_platform_experiment=lambda _id: platform_experiment))

        with mock.patch.object(run, "_on_new_machine", True):
            prompt = run._first_prompt(setup(planner(), draft), "s-1", None)
            self.assertIn("asks for this revision:\n\nKeep the bar as I wrote it.\n\n", prompt)
            self.assertIn("a file you wrote on it is gone", prompt)
            for absent in ("use two cores", "was replaced", "Continue where you left off", "branch"):
                self.assertNotIn(absent, prompt)
            self.assertEqual(run._first_prompt(setup(planner(), draft), None, None), run._new_machine_note(planner()) + "\n\nBegin.")
            self.assertTrue(run._first_prompt(setup(planner(), {"status": "draft", "status_reason": {"reason": "planning"}}), "s-1", None).endswith("Continue where you left off."))
            # A machine replaced after the planner answered the revision was handed it again.
            for answered in ("proposed", "declined"):
                again = {"status": "draft", "revisions": asked, "status_reason": {"reason": answered}}
                self.assertTrue(run._first_prompt(setup(planner(), again), "s-1", None).endswith("Continue where you left off."))
            self.assertTrue(run._first_prompt(setup(baseline_agent(), draft), "s-1", None).endswith("Continue where you left off."))
        self.assertNotIn("gone", run._first_prompt(setup(planner(), draft), "s-1", None))

    # A planner woken for a revision found its proposal's file gone with its machine and wrote
    # the whole proposal again.
    def test_a_revision_puts_the_stored_proposal_back_as_a_file(self):
        from hypothesisloop_agent.claude import run
        stored = {"risks": ["A shared GPU: the repeats may differ.", "two\nlines\n"],
                  "agents": [{"flavor": "hyperparameter-search", "hyperparameters": {"direction": "Sweep batch_size.\n"}}, {}],
                  "experiment": {"name": "ResNet-18: throughput", "metrics": [{"key": "top1_agreement", "role": "constraint", "bound": 0.99}],
                                 "description": "GOAL\n\n  config: fp32\n"},
                  "duration_hours": 4, "accelerator_type": "nvidia.com/gpu.product=RTX-4090", "sizing_reason": " kept as sent"}
        draft = {"status": "draft", "revisions": [{"message": "Keep the bar."}], "status_reason": {"reason": "revising"}, "proposal": stored}
        with tempfile.TemporaryDirectory() as statedir:
            def setup(platform_experiment):
                return SimpleNamespace(cfg=planner(), statedir=statedir, client=SimpleNamespace(get_platform_experiment=lambda _id: platform_experiment))

            path = os.path.join(os.path.realpath(statedir), "proposal.yaml")
            with mock.patch.object(run, "_on_new_machine", True):
                prompt = run._first_prompt(setup(dict(draft, proposal=None)), "s-1", None)
                self.assertTrue(prompt.endswith("a file you wrote on it is gone."))
                self.assertFalse(os.path.exists(path))
                prompt = run._first_prompt(setup(draft), "s-1", None)
            self.assertTrue(prompt.endswith(f"is gone. Your last proposal, as the platform stores it, is in {os.path.abspath(statedir)}/proposal.yaml: edit that file and propose it."))
            with open(path) as f:
                self.assertEqual(f.read(), """\
risks:
  - |-
    A shared GPU: the repeats may differ.
  - |
    two
    lines
agents:
  -
    flavor: hyperparameter-search
    hyperparameters:
      direction: |
        Sweep batch_size.
  - {}
experiment:
  name: |-
    ResNet-18: throughput
  metrics:
    -
      key: top1_agreement
      role: constraint
      bound: 0.99
  description: |
    GOAL

      config: fp32
duration_hours: 4
accelerator_type: nvidia.com/gpu.product=RTX-4090
sizing_reason: " kept as sent"
""")

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
