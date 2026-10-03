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
            self.assertLess(len(prompt.encode()), 7_970)

    def test_why_its_jobs_wait_for_the_start_is_read_from_status_reason(self):
        self.assertIn("with its `status_reason`, `metrics`, `stages` and `current_stage`", " ".join(self.render("generalist").split()))

    def test_a_trial_leaves_its_estimate_to_the_one_measured_on_the_baseline(self):
        self.assertIn("Leave `--estimated-duration-hours` out: the platform applies the one it measured on the baseline. "
                      "State one only for a trial clearly longer or shorter.", " ".join(self.render("generalist").split()))

    # One config differed up to 2.3x between rented hosts, and researchers who compared across
    # them found fp16 slower than fp32.
    def test_a_trial_is_compared_with_a_control_on_the_same_host(self):
        prompt = " ".join(self.render("generalist").split())
        self.assertIn("Compare a trial only with a control that ran on the same host (`on host N` in its update)", prompt)
        self.assertIn("With none there, rerun the control alongside. A finding across hosts says so", prompt)

    # In a platform outage one researcher ran `hl quota` in sleep loops and another pushed five
    # times. The harness wakes an agent that ended its turn once its polls are answered again,
    # and neither hl's refusal nor git's says so.
    def test_a_platform_that_fails_is_waited_for_with_the_turn_ended(self):
        self.assertIn("\"the platform could not be reached\", from `hl` or in a failed `git push`, is the platform failing: "
                      "stop there, and an update wakes you to retry once it is back.", " ".join(self.render("generalist").split()))

    # Six hosted agents were told by a brief to block in `hl watch --until` and all did, for the
    # whole job: the turn stayed in flight, so a frozen agent had no stored session to resume.
    def test_waiting_for_a_job_is_ending_the_turn_whatever_a_brief_says(self):
        prompt = " ".join(self.render("generalist").split())
        self.assertIn("Never poll, sleep or block in `hl watch`, whatever a brief says of waiting", prompt)
        self.assertIn("A reply with no command in it ends your turn, and the next update starts another.", prompt)

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
        self.assertIn("with `--related` for a combination's second parent", prompt)
        self.assertIn("a sweep is one hypothesis only while it tests one claim, and its winning configuration, once more work "
                      "builds on it, is registered as a child, so the best result is a node", prompt)
        # Both researchers of one run registered the winner as a child with no job of its own:
        # one confirmed it with --despite, which the refused verdict offers, and the other left
        # it open at its finish, which is taken.
        self.assertIn("Only its own jobs confirm that child: run that configuration twice with `--hypothesis CHILD`, "
                      "or settle it `inconclusive` with the reason", prompt)
        self.assertIn("inconclusive (noisy, or dropped: comment why)", prompt)
        self.assertIn("settle every hypothesis you created (confirmed, refuted, or inconclusive with a reason) and run "
                      "`hl platform-experiments agents finish --reason \"...\"`", prompt)

    # What the platform says when it matters is said there alone: a submit names the field it
    # lacks, the image it does not know, the branch to check out, the hypotheses yours reads
    # close to and the largest estimate that fits, a held job its reason and the command that
    # clears it, a refused verdict the jobs it needs, a refused finish the work left, and each
    # update that ends a job its quota with what a later stage raises it to.
    def test_the_briefing_leaves_to_the_platform_what_the_platform_says_when_it_matters(self):
        prompt = " ".join(self.render("generalist").split())
        for said_there in ("--help", "is refused", "`memory`", "job.held", "402", "next_stage", "hl job summary",
                           "--against", "--contradicts", "--distinct-from", "git push` needs", "pip install X` and",
                           "hl hypothesis query", "experiment_closed", "agent_held"):
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
