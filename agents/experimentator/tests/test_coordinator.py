import os
import re
import shutil
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

from hypothesisloop_agent import config, core, updates
from hypothesisloop_agent.claude import run
from test_require_joined import AGENT_TOKEN, FakePlatform, PlatformCase, derived
from test_updates import COMPLETED, fake_hl

COORDINATOR_TOKEN = derived("coordinator:pe-1")
LAUNCH_ENV = {"API_URL": "http://api.example.invalid", "HL_TOKEN": COORDINATOR_TOKEN}


class LaunchEnvironmentTest(unittest.TestCase):
    def test_a_coordinators_token_makes_a_coordinator_whatever_agent_id_is_left_in_the_environment(self):
        with mock.patch.dict(os.environ, {**LAUNCH_ENV, "AGENT_ID": "ag-1"}, clear=True):
            cfg = config.Config()
        self.assertTrue(config.coordinates(cfg))
        self.assertEqual((cfg.agent_id, cfg.platform_experiment_id), ("coordinator", "pe-1"))
        with mock.patch.dict(os.environ, {**LAUNCH_ENV, "HL_TOKEN": AGENT_TOKEN}, clear=True):
            cfg = config.Config()
        self.assertFalse(config.coordinates(cfg))
        self.assertEqual(cfg.agent_id, "agent-a")


class StartupTest(PlatformCase):
    def setUp(self):
        super().setUp()
        self.workdir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.workdir)

    def prepare(self):
        env = {**LAUNCH_ENV, "API_URL": self.url, "AGENT_WORKDIR": self.workdir}
        with mock.patch.dict(os.environ, env, clear=True), \
                mock.patch.object(core, "hl_commands", return_value="usage: hl") as commands:
            setup = core.prepare()
        commands.assert_called_once_with(True)
        return setup

    def test_a_coordinator_starts_on_its_token_alone_and_never_asks_for_a_signup(self):
        # The roster is what only a coordinator's token reads.
        FakePlatform.joined = {"agents"}
        setup = self.prepare()
        self.assertEqual(FakePlatform.calls[0], ("GET", "/platform-experiments/pe-1/agents"))
        self.assertFalse([path for _, path in FakePlatform.calls if "/signups/" in path])
        self.assertNotIn("POST", [method for method, _ in FakePlatform.calls])
        self.assertEqual(FakePlatform.tokens, {"Bearer " + COORDINATOR_TOKEN})
        self.assertEqual((setup.cfg.flavor, setup.cfg.model), ("coordinator", "model-1"))
        self.assertIn("coordinator of one platform experiment", setup.system_prompt)

    def test_a_token_that_does_not_coordinate_fails_startup_without_showing_itself(self):
        FakePlatform.joined = set()
        for status in (401, 403, 404):
            FakePlatform.status_for_strangers = status
            with self.assertRaises(SystemExit) as refused:
                self.prepare()
            self.assertIn("does not coordinate platform experiment pe-1", str(refused.exception))
            self.assertNotIn(COORDINATOR_TOKEN, str(refused.exception))



class BriefingTest(unittest.TestCase):
    def render(self) -> str:
        with mock.patch.dict(os.environ, LAUNCH_ENV, clear=True):
            cfg = config.Config()
        cfg.run_as(config.COORDINATOR_FLAVOR, "model-1")
        return core.system_prompt(cfg, "RULES\n", "COMMANDS", "https://github.com/acme/model.git", "/agent/workspace")

    def test_it_renders_with_the_rules_the_commands_and_its_assignment(self):
        prompt = self.render()
        self.assertIn("\n\nRULES\n\n## hl commands\n\nCOMMANDS\n\n", prompt)
        self.assertIn("clone https://github.com/acme/model.git into /agent/workspace, the directory you start in", " ".join(prompt.split()))
        self.assertIn("(:Objective {id:'pe-1'})", prompt)
        self.assertIsNone(re.search(r"\{[a-z_]+\}", prompt), "a placeholder was left unfilled")

    def test_it_acts_through_hl_and_names_no_host_tool_and_no_http_endpoint(self):
        prompt = self.render()
        self.assertIsNone(re.search(r"podman|kubectl|\bmake\b|docker|(GET|POST|PUT) /|\$API_URL|curl", prompt))
        self.assertLess(len(prompt.encode()), 9_000)

    def test_its_findings_go_to_the_platform_and_it_pushes_nothing(self):
        prompt = self.render()
        self.assertIn("hl platform-experiments summary --file", prompt)
        self.assertIn("read-only", prompt)
        self.assertIsNone(re.search(r"git push|findings\.md|hl/pe-1/coordinator", prompt, re.IGNORECASE))

    def test_the_commands_listed_for_a_coordinator_are_the_ones_that_change_roster_and_brief(self):
        asked = []

        def fake_run(command, **how):
            # One command's help is on stderr: read from stdout alone, a planner was never
            # shown the proposal file and a coordinator never how to add an agent.
            self.assertEqual(how["stderr"], core.subprocess.STDOUT)
            asked.append(command[1:-1])
            return mock.Mock(stdout="usage\n")

        with mock.patch.object(core.subprocess, "run", fake_run):
            core.hl_commands(True)
            self.assertIn(["platform-experiments", "agents", "add"], asked)
            self.assertIn(["platform-experiments", "update"], asked)
            self.assertIn(["platform-experiments", "summary"], asked)
            self.assertIn(["platform-experiments", "close"], asked)
            asked.clear()
            core.hl_commands()
        self.assertEqual(asked, [[], ["job"], ["hypothesis"]])

    def test_the_prompt_file_ships_beside_the_agents_briefing(self):
        self.assertTrue((Path(core.__file__).parent / "prompts" / "coordinator.md").is_file())


class WatchTest(unittest.IsolatedAsyncioTestCase):
    def test_the_token_says_whether_the_watch_is_a_coordinators_of_the_whole_experiment(self):
        env = updates.watch_env("http://api", COORDINATOR_TOKEN, "/tmp/w")
        self.assertEqual(updates.watch_command(env), ["hl", "watch", "--all", "--decisions", "--settle", "120"])
        own = updates.watch_env("http://api", AGENT_TOKEN, "/tmp/w")
        self.assertEqual((own["HL_TOKEN"], updates.watch_command(own)), (AGENT_TOKEN, ["hl", "watch"]))

    async def test_an_ended_job_reaches_a_coordinator_as_it_is_with_no_read_as_an_agent(self):
        command, _ = fake_hl(f"echo '{COMPLETED}'\n", status="{}", stats="{}", quota="{}")
        events = await updates.drain({**os.environ, "HL_TOKEN": COORDINATOR_TOKEN}, command)
        self.assertEqual([e["kind"] for e in events], ["experiment.status"])
        self.assertFalse(os.path.exists(command[0] + ".reads"))

    def test_the_last_turn_of_a_coordinator_writes_the_summary_and_pushes_nothing(self):
        self.assertNotIn("job", run._COORDINATOR_SHUTDOWN_PROMPT)
        self.assertNotIn("push", run._COORDINATOR_SHUTDOWN_PROMPT)
        self.assertIn("hl platform-experiments summary", run._COORDINATOR_SHUTDOWN_PROMPT)

    # A coordinator wrote "closed by owner at 02:48" for a run that had reached its end time:
    # nothing told it why it was being stopped.
    def test_the_last_turn_opens_with_why_the_run_stops_as_the_platform_has_it(self):
        ended = {"status": "closed", "ends_at": "2026-01-01T02:48:00Z", "closed_at": "2026-01-01T02:48:05Z"}
        person = {"kind": "user", "id": "u-1", "name": "Ada"}
        for experiment, signup, flavor, want in (
            ({**ended, "close_reason": "end_time_reached"}, {}, "coordinator", "reached its end time, 2026-01-01T02:48:00Z, and the platform closed it"),
            ({**ended, "close_reason": "closed_by_request", "closed_by": person}, {}, "coordinator", "a person closed it (Ada) at 2026-01-01T02:48:05Z"),
            ({**ended, "close_reason": "closed_by_request", "closed_by": {"kind": "coordinator"}}, {}, "coordinator", "you closed it"),
            ({**ended, "close_reason": "closed_by_request", "closed_by": {"kind": "coordinator"}}, {"ended": "closed"}, "generalist", "its coordinator closed it"),
            (ended, {}, "coordinator", "The platform recorded no reason"),
            ({**ended, "close_reason": "planning_limit_reached"}, {"ended": "closed"}, "planner", "the planning limit, 2026-01-01T02:48:00Z, passed with nothing proposed"),
            ({"status": "running"}, {}, "coordinator", "Only this machine is being stopped, and the platform does not say why"),
            ({"status": "running"}, {"ended": "finished"}, "generalist", "you said you have nothing left to do"),
            ({"status": "running"}, {"ended": "cut"}, "generalist", "you were cut from the platform experiment at a stage boundary"),
        ):
            setup = SimpleNamespace(cfg=SimpleNamespace(agent_id="ag-1", flavor=flavor, platform_experiment_id="pe-1"),
                                    client=SimpleNamespace(get_platform_experiment=lambda _id, e=experiment: e, get_signup=lambda *_, s=signup: s))
            prompt = run._shutdown_prompt(setup)
            self.assertTrue(prompt.startswith("You are being stopped"), prompt)
            self.assertIn(want, prompt)
            self.assertNotIn("owner", prompt)

    # A planner whose proposal was just stored was told its machine was being replaced and to
    # file the summaries it owed.
    def test_a_planner_whose_answer_is_stored_is_told_its_planning_is_over_and_owes_nothing(self):
        setup = SimpleNamespace(cfg=SimpleNamespace(agent_id="ag-1", flavor="planner", platform_experiment_id="pe-1"),
                                client=SimpleNamespace(get_signup=lambda *_: {"ended": "planned"}))
        self.assertEqual(run._shutdown_prompt(setup), "You are being stopped because your planning is over: the platform has your answer "
                         "to the draft. This is your last turn. Start nothing new. You owe nothing more: end your turn.")


if __name__ == "__main__":
    unittest.main()
