import os
import tempfile
import unittest
from unittest import mock
from types import SimpleNamespace

from hypothesisloop_agent import core


def planner():
    return SimpleNamespace(agent_id="ag-planner", flavor="planner", role="planner", platform_experiment_id="pe-1", hyperparameters={})


def baseline_agent():
    return SimpleNamespace(agent_id="ag-baseline", flavor="generalist", role="baseline", platform_experiment_id="pe-1", hyperparameters={})


def listed(cfg) -> list:
    asked = []
    with mock.patch.object(core.subprocess, "run", lambda command, **_: asked.append(command[1:-1]) or mock.Mock(stdout="usage\n")):
        core.hl_commands(cfg)
    return asked


class CommandsListedTest(unittest.TestCase):
    # hl's own listing named `quota`, which answers a planner 404, and no `repo init`, whose
    # files one guessed at.
    def test_a_planner_and_a_baseline_agent_are_shown_the_commands_of_their_role(self):
        self.assertEqual(listed(planner()), [["platform-experiments", "propose"], ["platform-experiments", "decline"], ["repo", "init"]])
        self.assertEqual(listed(baseline_agent()), [[], ["job", "submit"], ["env", "run"], ["job", "summary"], ["platform-experiments", "agents", "finish"]])


class RunEndedTest(unittest.TestCase):
    def reason(self, cfg, signup, platform_experiment=None):
        client = SimpleNamespace(get_platform_experiment=lambda _id: platform_experiment, get_signup=lambda *_: signup)
        return core.stop_reason(SimpleNamespace(cfg=cfg, client=client), [False])

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

    # A baseline agent read baseline_due_at once, ten minutes ahead, and never again: it wrote a
    # careful harness and the experiment closed with no job submitted.
    def test_a_baseline_agent_is_told_its_clock_with_every_prompt(self):
        import asyncio
        from datetime import datetime, timedelta, timezone
        from hypothesisloop_agent.claude import run
        due = (datetime.now(timezone.utc) + timedelta(minutes=9, seconds=30)).strftime("%Y-%m-%dT%H:%M:%SZ")

        def setup(cfg, platform_experiment):
            return SimpleNamespace(cfg=cfg, client=SimpleNamespace(get_platform_experiment=lambda _id: platform_experiment))

        clock = f"baseline_due_at is {due}, 9 minutes from now."
        measuring = setup(baseline_agent(), {"status": "open", "baseline_due_at": due})
        self.assertEqual(run._first_prompt(measuring, None, None), "Begin.\n\n" + clock)
        job = [{"kind": "experiment.status", "subject": "job-1", "value": "FAILED"}]
        self.assertTrue(asyncio.run(run._told(measuring, job)).endswith("\n\n" + clock))
        self.assertEqual(asyncio.run(run._told(measuring, [])), "")
        self.assertEqual(asyncio.run(run._told(setup(baseline_agent(), {"status": "running"}), job)), asyncio.run(run._told(setup(planner(), {}), job)))
        self.assertEqual(run._first_prompt(setup(planner(), {"status": "draft", "baseline_due_at": due}), None, None), "Begin.")

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
        researcher = SimpleNamespace(agent_id="ag-1", flavor="generalist", platform_experiment_id="pe-1")
        self.assertIsNone(self.reason(researcher, {}))
        self.assertEqual(self.reason(researcher, {"ended": "finished"}), "the platform ended the run: finished")

    def test_a_coordinator_stops_once_its_platform_experiment_closed(self):
        coordinator = SimpleNamespace(agent_id="", flavor="coordinator", platform_experiment_id="pe-1")
        self.assertIsNone(self.reason(coordinator, None, {"status": "running"}))
        self.assertEqual(self.reason(coordinator, None, {"status": "closed"}), "the platform ended the run: closed")


if __name__ == "__main__":
    unittest.main()
