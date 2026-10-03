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
            # One agent cloned into a subdirectory and one into the workspace, so their paths
            # differed, and one set up a credential helper the image already has.
            self.assertIn("`git push` needs no setup, git has your credential", flat)
            # Every job of two runs ran the baseline commit and no researcher pushed a branch:
            # the branch was the agent's, so git followed no idea.
            self.assertIn("A branch is your implementation of one hypothesis, named by hl: before working on an idea, and after a "
                          "restart, `hl hypothesis checkout ID` checks yours out, created from its parent's last "
                          "completed job, from the baseline under `root`, or `--from JOB`, refused once yours exists", flat)
            self.assertIn("`hl job submit` pushes it", flat)
            self.assertIn("Jobs report metrics with `post_metric` of the root `metrics.py`", flat)
            self.assertNotIn("$WORKLOAD_SAMPLES", flat)
            # --from is refused on a branch that exists, so a second way on from a job is a new idea.
            self.assertIn("A different claim, or a second way to improve your job, is a child hypothesis; another "
                          "implementation of a claim, yours or another agent's, is your branch of its hypothesis", flat)
            self.assertNotIn("git checkout -b", flat)
            self.assertNotIn("hl/pe-1/agent-a", flat)
            self.assertLess(len(prompt.encode()), 11_000)

    # An agent's `pip install` went to a site its python3 never read, and `image: python` once
    # ran Docker Hub's python.
    def test_one_way_to_install_on_the_machine_and_in_a_job_and_how_an_image_is_named(self):
        prompt = " ".join(self.render("generalist").split())
        self.assertIn("On this machine `pip install X` and `uv pip install X` install where `python3` reads", prompt)
        self.assertIn("the rest in the `command` with `uv pip install X`", prompt)
        self.assertIn("a name your type's line in `hl capacity` lists in `job_images`, spelled as there: "
                      "any other word but a full registry address is refused, and every job names one", prompt)

    def test_why_its_jobs_wait_for_the_start_is_read_from_status_reason(self):
        self.assertIn("with its `status_reason`, `metrics`, `stages` and `current_stage`", " ".join(self.render("generalist").split()))

    # A job that asked for more cores than any node had waited to the run's end, its agent idle.
    def test_a_held_job_is_cancelled_and_resubmitted_smaller_or_waited_for(self):
        self.assertIn("A `job.held` update says why a job still waits: cancel and resubmit it smaller if no "
                      "cluster can fit what it asks, else wait.", " ".join(self.render("generalist").split()))

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
    # times. The harness wakes an agent that ended its turn once its polls are answered again.
    def test_a_platform_that_fails_is_waited_for_with_the_turn_ended(self):
        self.assertIn("\"the platform could not be reached\", from `hl` or in a failed `git push`, is the platform failing: "
                      "end your turn, and an update wakes you to retry once it is back.", " ".join(self.render("generalist").split()))

    # A job that left its storage out waited for a share of the disk the node never had free,
    # and a chip job that left out cpu and memory held a whole node, so nothing ran beside it.
    def test_every_job_states_its_cpu_memory_and_storage_and_takes_no_whole_node(self):
        prompt = " ".join(self.render("generalist").split())
        self.assertIn("CPU work: `cpu: N` in its place. Every job states `cpu`, `memory` and `storage` for what it needs, "
                      "small enough that other jobs fit beside it, never a whole node: one left out is refused", prompt)

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
        self.assertIn("inconclusive (noisy, or dropped: comment why)", prompt)
        # A finish with a hypothesis left open is refused now.
        self.assertIn("no job in flight or owing its summary and every hypothesis you created settled, run "
                      "`hl platform-experiments agents finish --reason \"...\"`: it is refused with `hypotheses_open` while one is open", prompt)
        self.assertIn("on a match, test under its id so every trial of a claim stays on one thread", prompt)

    # All four researchers of two runs opened with `hl hypothesis query 'MATCH (n) RETURN n'`,
    # which is refused: the briefing sent them to the query to read the graph.
    def test_the_graph_is_read_with_list_and_get_and_queried_only_after_the_querys_help(self):
        prompt = " ".join(self.render("generalist").split())
        self.assertIn("Read the graph with `hl hypothesis list`; `hl hypothesis get ID` adds findings, comments and jobs", prompt)
        self.assertIn("`hl hypothesis query` takes read-only Cypher: read its --help first", prompt)

    # A researcher answered a completed job's update with a line and no summary, and its next
    # job waited four minutes behind it.
    def test_a_completed_jobs_summary_is_filed_at_once_because_the_next_job_waits_for_it(self):
        self.assertIn("An ended job's update carries its machine and, completed, its metrics. File a COMPLETED job's summary at once "
                      "with `hl job summary JOB --text T`: your next job waits while one is owed.", " ".join(self.render("generalist").split()))

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
