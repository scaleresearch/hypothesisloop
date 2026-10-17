import os
import tempfile
import unittest
from unittest import mock
from types import SimpleNamespace

from conftest import SITUATION
from hypothesisloop_agent import core, prompt, updates


def planner():
    return SimpleNamespace(agent_id="ag-planner", role="planner", platform_experiment_id="pe-1", hyperparameters={})


def baseline_agent():
    return SimpleNamespace(agent_id="ag-baseline", role="baseline", platform_experiment_id="pe-1", hyperparameters={})


def listed(cfg) -> list:
    asked = []
    with mock.patch.object(prompt.subprocess, "run", lambda command, **_: asked.append(command[1:-1]) or mock.Mock(stdout="usage\n")):
        prompt.hl_commands(cfg)
    return asked


class CommandsListedTest(unittest.TestCase):
    # hl's own listing named `quota`, which answers a planner 404.
    def test_a_planner_and_a_baseline_agent_are_shown_the_commands_of_their_role(self):
        self.assertEqual(listed(planner()), [["platform-experiments", "propose"], ["platform-experiments", "decline"]])
        self.assertEqual(listed(baseline_agent()), [[], ["job", "submit"], ["job", "summary"], ["platform-experiments", "agents", "finish"]])


class RunEndedTest(unittest.TestCase):
    def reason(self, cfg, signup, platform_experiment=None):
        client = SimpleNamespace(get_platform_experiment=lambda _id: platform_experiment, get_signup=lambda *_: signup)
        return core.stop_reason(SimpleNamespace(cfg=cfg, client=client), [False])

    # The planner's machine is rented while it plans and no longer: it stops by itself once the
    # platform says its answer is stored, and a change of the goal, which takes that back, lets it
    # go on.
    def test_a_planner_stops_once_the_platform_has_its_answer(self):
        self.assertIsNone(self.reason(planner(), {}))
        self.assertEqual(self.reason(planner(), {"ended": "planned"}), "the platform ended the run: planned")
        self.assertEqual(self.reason(planner(), {"ended": "closed"}), "the platform ended the run: closed")

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
        self.assertTrue(asyncio.run(run._told(measuring, {}, job)).endswith("\n\n" + clock + "\n\n" + SITUATION))
        self.assertEqual(asyncio.run(run._told(measuring, {}, [])), "")
        told = "Platform updates:\nexperiment.status job-1 FAILED"
        self.assertEqual(asyncio.run(run._told(setup(baseline_agent(), {"status": "running"}), {}, job)), told + "\n\n" + SITUATION)
        self.assertEqual(asyncio.run(run._told(setup(planner(), {}), {}, job)), told, "a planner has no run to read the situation of")
        self.assertEqual(run._first_prompt(setup(planner(), {"status": "draft", "baseline_due_at": due}), None, None), "Begin.")

    # Researchers once finished in twenty minutes and their machines idled to the end time.
    def test_a_researcher_stops_once_it_finished_and_not_before(self):
        researcher = SimpleNamespace(agent_id="ag-1", role="researcher", platform_experiment_id="pe-1")
        self.assertIsNone(self.reason(researcher, {}))
        self.assertEqual(self.reason(researcher, {"ended": "finished"}), "the platform ended the run: finished")

    def test_a_coordinator_stops_once_its_platform_experiment_closed(self):
        coordinator = SimpleNamespace(agent_id="coordinator", role="coordinator", platform_experiment_id="pe-1")
        self.assertIsNone(self.reason(coordinator, None, {"status": "running"}))
        self.assertEqual(self.reason(coordinator, None, {"status": "closed"}), "the platform ended the run: closed")


class GoalTest(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.statedir = tempfile.mkdtemp()
        self.draft = {"status": "draft", "goal": "Make it fit one accelerator.", "goal_version": 2, "status_reason": {"reason": "revising"}}

    def setup(self, cfg):
        async def get(_id):
            return self.draft
        return SimpleNamespace(cfg=cfg, statedir=self.statedir, workdir=self.statedir, client=SimpleNamespace(get_platform_experiment=get))

    async def told(self, cfg, events):
        from hypothesisloop_agent.claude import prompts
        return await prompts.told(self.setup(cfg), SimpleNamespace(on_new_machine=False), SimpleNamespace(not_own=lambda e: e), events)

    # A planner kept on its machine after its answer is told a changed goal by an event that only
    # points at the draft. It is handed the goal itself and the version to propose for, and the
    # same event replayed once the planner has answered is handed as nothing. A coordinator reads
    # it as a line of its batch.
    async def test_a_changed_goal_reaches_a_planner_as_the_goal_and_an_answered_one_as_nothing(self):
        goal = {"kind": updates.GOAL, "subject": "pe-1", "value": "2", "agent_id": "ag-planner"}
        comment = {"kind": "comment.new", "subject": "hyp-1", "value": "human"}
        told = await self.told(planner(), [goal, comment])
        self.assertTrue(told.startswith("The owner changed the goal to:\n\nMake it fit one accelerator.\n\n"
                                        "Plan from it, keep what it does not change, and propose with goal_version: 2."), told)
        self.assertTrue(told.endswith("Platform updates:\ncomment.new hyp-1 human"), told)
        self.assertNotIn(updates.GOAL, told)
        self.draft["status_reason"] = {"reason": "proposed"}
        self.assertEqual(await self.told(planner(), [goal]), "")
        coordinator = SimpleNamespace(agent_id="", role="coordinator", platform_experiment_id="pe-1")
        self.assertEqual(await self.told(coordinator, [dict(goal, agent_id="coordinator")]),
                         "Platform updates:\nthe owner changed the goal (version 2): read it with `hl platform-experiments get`")

    # A planner whose machine was released is started again with its session, and found its
    # proposal's file gone with the machine: the stored proposal is put back for the goal it has.
    async def test_a_resumed_planner_is_told_the_goal_with_its_proposal_put_back(self):
        from hypothesisloop_agent.claude import prompts
        store = SimpleNamespace(on_new_machine=True)
        prompt = await prompts.opening(self.setup(planner()), store, "s-1", None)
        self.assertTrue(prompt.endswith("Your machine was released with your answer: a file you wrote on it is gone."), prompt)
        path = os.path.join(os.path.abspath(self.statedir), "proposal.yaml")
        self.assertFalse(os.path.exists(path))
        self.draft["proposal"] = {"goal_version": 1, "experiment": {"name": "throughput"}, "duration_hours": 4}
        prompt = await prompts.opening(self.setup(planner()), store, "s-1", None)
        self.assertTrue(prompt.endswith(f"is in {path}: edit that file and propose it."), prompt)
        with open(path) as f:
            self.assertEqual(f.read(), "goal_version: 2\nexperiment:\n  name: throughput\nduration_hours: 4\n")
        self.draft["status_reason"] = {"reason": "proposed"}
        self.assertTrue((await prompts.opening(self.setup(planner()), store, "s-1", None)).endswith("Continue where you left off."))

    # The planner's stream is its own, so the goal wakes it and no researcher; the coordinator is
    # woken by the goal and by a changed plan.
    def test_the_goal_wakes_a_planner_and_the_coordinator_and_the_plan_the_coordinator(self):
        self.assertIn(updates.GOAL, updates.WAKES["agent"])
        self.assertIn(updates.GOAL, updates.WAKES["coordinator"])
        self.assertIn("platform_experiment.plan", updates.WAKES["coordinator"])


if __name__ == "__main__":
    unittest.main()
