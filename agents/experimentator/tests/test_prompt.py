import re
import unittest
from pathlib import Path
from types import SimpleNamespace

from hypothesisloop_agent import core

FLAVORS = sorted(p.stem for p in (Path(core.__file__).parent / "prompts" / "flavors").glob("*.md"))


class SystemPromptTest(unittest.TestCase):
    def render(self, flavor: str) -> str:
        cfg = SimpleNamespace(agent_id="agent-a", flavor=flavor, role="competitor", platform_experiment_id="pe-1", hyperparameters={"k": 1})
        return core.system_prompt(cfg, "RULES\n", "COMMANDS", "https://github.com/acme/model.git", "/agent/workspace")

    def test_every_flavor_renders_with_the_rules_and_the_hl_commands(self):
        for flavor in FLAVORS:
            prompt = self.render(flavor)
            self.assertIn("\n\nRULES\n\n## hl commands\n\nCOMMANDS\n\n", prompt)
            flat = " ".join(prompt.split())
            # Five of six hosted agents cloned into /tmp and then fought the working directory
            # every command starts in.
            self.assertIn("run `git clone https://github.com/acme/model.git .` in /agent/workspace, the directory you start in", flat)
            # Every job of two runs ran the baseline commit and no researcher pushed a branch:
            # the branch was the agent's, so git followed no idea.
            self.assertIn("A branch is your implementation of one hypothesis, named by hl: before working on an idea, and after a "
                          "restart, `hl hypothesis checkout ID` puts the clone on yours", flat)
            self.assertIn("`hl job submit` pushes it", flat)
            self.assertIn("Jobs report metrics with `post_metric` of the root `metrics.py`", flat)
            self.assertNotIn("$WORKLOAD_SAMPLES", flat)
            # A branch that exists takes no --from, so a second way on from a job is a new idea.
            self.assertIn("A different claim, or a second way to improve your job, is a child hypothesis; another "
                          "implementation of a claim, yours or another agent's, is your branch of its hypothesis", flat)
            self.assertNotIn("git checkout -b", flat)
            self.assertNotIn("hl/pe-1/agent-a", flat)
            self.assertLess(len(prompt.encode()), 6_820)

    def test_why_its_jobs_wait_for_the_start_is_read_from_status_reason(self):
        self.assertIn("with its `status_reason`, `metrics`, `stages` and `current_stage`", " ".join(self.render("generalist").split()))

    # The help of hl job submit says how the command gets the commit and a package, what a secret
    # is, which estimate a job holds and what a submit answers, and the base job spec carries the
    # command. The briefing points there once.
    def test_what_a_job_spec_means_is_left_to_the_help_of_submit(self):
        prompt = " ".join(self.render("generalist").split())
        self.assertIn("`hl job submit --help` has the rest of a spec, to read before you change one", prompt)
        self.assertIn("Jobs run in parallel while their estimates fit your quota.", prompt)
        for said_there in ("--estimated-duration-hours", "hl-clone", "pip install", "--secret-env", "hl capacity", "measured on the baseline"):
            self.assertNotIn(said_there, prompt)

    # A planner is told a direction is a mechanism, the hyperparameter-search brief called the
    # same keys starting values, and this briefing left their meaning to the agent.
    def test_hyperparameters_are_the_direction_the_agent_was_given(self):
        for flavor in FLAVORS:
            prompt = " ".join(self.render(flavor).split())
            self.assertIn("Your `hyperparameters` are the direction whoever added you gave you: the approach you explore, "
                          "each claim under it a hypothesis of yours.", prompt)
            self.assertEqual(prompt.count("`hyperparameters`"), 1, flavor)

    # One config differed up to 2.3x between rented hosts, and researchers who compared across
    # them found fp16 slower than fp32. Told `on host N`, a researcher on a cluster of one node,
    # whose updates read `on node box-b`, spent a job on a control it had and wrote
    # "different host" in two findings: the rule names what every update shows.
    def test_a_trial_is_compared_with_a_control_on_the_same_machine(self):
        prompt = " ".join(self.render("generalist").split())
        self.assertIn("Compare a trial only with a control whose update ends in the same `on ...`, its machine", prompt)
        self.assertIn("With none there, rerun the control alongside. A finding across machines says so", prompt)
        for worded_for_rented in ("on host N", "rented"):
            self.assertNotIn(worded_for_rented, prompt)

    # Agents waited for a job in sleep loops around `hl job status`, and in a platform outage one
    # ran `hl quota` in sleep loops and another pushed five times. Ending the turn is the one way
    # to wait, said once to every role by the harness, and hl's own line says what a platform
    # that could not be reached asks of the agent.
    def test_every_role_is_told_once_that_ending_the_turn_is_how_it_waits(self):
        coordinator = SimpleNamespace(agent_id="coordinator", flavor="coordinator", role="", platform_experiment_id="pe-1")
        planner = SimpleNamespace(agent_id="ag-p", flavor="planner", role="planner", platform_experiment_id="pe-1")
        baseline = SimpleNamespace(agent_id="ag-b", flavor="generalist", role="baseline", platform_experiment_id="pe-1", hyperparameters={})
        prompts = [self.render("generalist"), *(core.system_prompt(cfg, "RULES\n", "COMMANDS", "https://github.com/acme/model.git", "/agent/workspace")
                                                for cfg in (coordinator, planner, baseline))]
        for prompt in prompts:
            self.assertTrue(prompt.endswith("\n\nTo wait, reply with no command in it: your turn ends, and the next update starts another. "
                                            "An error from `hl` is signal: do what it says."), prompt[-300:])
            for said_once in ("your turn", "An error from `hl`"):
                self.assertEqual(prompt.count(said_once), 1)
            for forbidden in ("Never poll", "sleep", "hl watch", "could not be reached"):
                self.assertNotIn(forbidden, prompt)

    # Four of six hosted agents wrote a job's result twice, as its summary and again as a comment
    # headed "Finding:", because the briefing used both words for it.
    def test_a_jobs_summary_is_its_finding_and_a_comment_is_for_what_has_no_job(self):
        prompt = " ".join(self.render("generalist").split())
        self.assertIn("a job's *summary*, which is its finding", prompt)
        self.assertIn("a *comment*: only what has no job behind it", prompt)

    # Every graph of the audited runs was flat: three root hypotheses, two of them varying the
    # same knobs, one holding a sweep of seven jobs whose winner was only a finding, and some
    # left open at the close.
    def test_the_graph_reads_from_distinct_approaches_down_to_the_best_result(self):
        prompt = " ".join(self.render("generalist").split())
        self.assertIn("Every *hypothesis* is one falsifiable claim", prompt)
        self.assertIn("the root's children are the distinct approaches to the objective, each a different mechanism", prompt)
        self.assertIn("Before adding a root child, read the existing ones: take a mechanism none of them has, or go under one", prompt)
        self.assertIn("A refinement `--builds-on` the most specific hypothesis it refines.", prompt)
        self.assertIn("a sweep is one hypothesis only while it tests one claim, and its winning configuration, once more work "
                      "builds on it, is registered as a child, so the best result is a node", prompt)
        # Both researchers of one run registered the winner as a child with no job of its own:
        # one confirmed it with --despite, which the refused verdict offers, and the other left
        # it open at its finish, which is taken.
        self.assertIn("Only its own jobs confirm that child: run that configuration twice with `--hypothesis CHILD`, "
                      "or settle it `inconclusive` with the reason", prompt)
        self.assertIn("inconclusive (noisy, or dropped: comment why)", prompt)
        # The finish prints the hypotheses it leaves open, so the briefing asks for no settling.
        self.assertIn("With your quota spent or nothing worth a job left, run "
                      "`hl platform-experiments agents finish --reason \"...\"`", prompt)
        self.assertNotIn("settle every hypothesis", prompt)

    # What the platform says when it matters is said there alone: a submit names the field it
    # lacks, the image it does not know, the branch to check out, the hypotheses yours reads
    # close to and the largest estimate that fits, a held job its reason and the command that
    # clears it, a refused verdict the jobs it needs, a refused finish the work left, and each
    # update that ends a job its quota with what a later stage raises it to.
    def test_the_briefing_leaves_to_the_platform_what_the_platform_says_when_it_matters(self):
        prompt = " ".join(self.render("generalist").split())
        for said_there in ("is refused", "`memory`", "job.held", "402", "next_stage", "hl job summary",
                           "--against", "--contradicts", "--distinct-from", "--related", "git push` needs",
                           "hl hypothesis query", "experiment_closed", "agent_held", "are cut"):
            self.assertNotIn(said_there, prompt)

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


if __name__ == "__main__":
    unittest.main()
