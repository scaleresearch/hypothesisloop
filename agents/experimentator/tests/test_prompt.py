import unittest
from types import SimpleNamespace

from hypothesisloop_agent import core


class GitIdentityTest(unittest.TestCase):
    # Every agent's commits carried the image's one name, so a branch's history did not say who
    # wrote what.
    def test_commits_carry_the_agents_own_id(self):
        agent = SimpleNamespace(agent_id="ag-7", flavor="generalist", platform_experiment_id="pe-1")
        identity = core.git_identity(agent)
        self.assertEqual((identity["GIT_AUTHOR_NAME"], identity["GIT_COMMITTER_NAME"]), ("ag-7", "ag-7"))
        self.assertTrue(identity["GIT_AUTHOR_EMAIL"].startswith("ag-7@"))
        coordinator = SimpleNamespace(agent_id="coordinator", flavor="coordinator", platform_experiment_id="pe-1")
        self.assertEqual(core.git_identity(coordinator)["GIT_AUTHOR_NAME"], "coordinator-pe-1")


class ResearcherPromptTest(unittest.TestCase):
    # A researcher gated every job on a green local suite and an offline check, and submitted one
    # job in an hour with most of its capacity free.
    def test_the_job_is_the_measurement_and_local_checks_gate_nothing(self):
        cfg = SimpleNamespace(agent_id="ag-1", flavor="generalist", role="researcher", platform_experiment_id="pe-1",
                              hyperparameters={})
        prompt = " ".join(core.system_prompt(cfg, "rules", "commands", "https://git/x", "/app/workspace").split())
        self.assertIn("A local build or test run is a quick sanity check before it, never a gate", prompt)
        self.assertIn("noted in the summary and the job submitted", prompt)
        self.assertIn("its own jobs (`hl job submit`", prompt)
        self.assertIn("goes to every subagent it concerns", prompt)
        self.assertIn("keep several independent jobs in flight", prompt)

    # A researcher's job gained 12% on an idea a person posted, tested under its id, and filed no
    # finding and no verdict, so the idea stayed open, and looked for a summary command it could
    # not find.
    def test_a_persons_idea_it_tested_is_its_to_summarize_and_judge(self):
        cfg = SimpleNamespace(agent_id="ag-1", flavor="generalist", role="researcher", platform_experiment_id="pe-1",
                              hyperparameters={})
        prompt = " ".join(core.system_prompt(cfg, "rules", "commands", "https://git/x", "/app/workspace").split())
        self.assertIn("(`hl job summary JOB --text T`)", prompt)
        self.assertIn("its summaries and its verdict are yours to give as on your own; the idea stays the person's", prompt)


if __name__ == "__main__":
    unittest.main()
