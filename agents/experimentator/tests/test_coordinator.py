"""The harness started as a platform experiment's coordinator: what it is launched with, what it
checks before its first turn, what it is briefed with and what it watches."""
import os
import re
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from hypothesisloop_agent import config, core, updates
from hypothesisloop_agent.claude import run
from test_require_joined import FakePlatform, PlatformCase
from test_updates import COMPLETED, fake_hl

LAUNCH_ENV = {
    "API_URL": "http://api.example.invalid", "HL_TOKEN": "hls_coordinator", "PLATFORM_EXPERIMENT_ID": "pe-1",
    "AGENT_FLAVOR": "coordinator", "AGENT_MODEL": "model-1",
}


class LaunchEnvironmentTest(unittest.TestCase):
    def test_what_the_launcher_hands_a_coordinator_is_the_whole_config_and_holds_no_agent_id(self):
        with mock.patch.dict(os.environ, LAUNCH_ENV, clear=True):
            cfg = config.Config()
        self.assertTrue(config.coordinates(cfg))
        self.assertEqual((cfg.agent_id, config.watched_agent_id(cfg), cfg.model), ("coordinator", "", "model-1"))

    def test_an_agent_id_beside_the_coordinator_flavor_is_refused(self):
        with mock.patch.dict(os.environ, {**LAUNCH_ENV, "AGENT_ID": "ag-1"}, clear=True), self.assertRaises(SystemExit) as refused:
            config.Config()
        self.assertIn("AGENT_ID", str(refused.exception))

    def test_an_agent_still_needs_its_id_and_does_not_coordinate(self):
        env = {**LAUNCH_ENV, "AGENT_FLAVOR": "generalist"}
        with mock.patch.dict(os.environ, env, clear=True), self.assertRaises(SystemExit):
            config.Config()
        with mock.patch.dict(os.environ, {**env, "AGENT_ID": "ag-1"}, clear=True):
            cfg = config.Config()
        self.assertFalse(config.coordinates(cfg))
        self.assertEqual(config.watched_agent_id(cfg), "ag-1")


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
        self.assertEqual(FakePlatform.tokens, {"Bearer hls_coordinator"})
        self.assertIn("coordinator of one platform experiment", setup.system_prompt)

    def test_a_token_that_does_not_coordinate_fails_startup_without_showing_itself(self):
        FakePlatform.joined = set()
        for status in (401, 403, 404):
            FakePlatform.status_for_strangers = status
            with self.assertRaises(SystemExit) as refused:
                self.prepare()
            self.assertIn("does not coordinate platform experiment pe-1", str(refused.exception))
            self.assertNotIn("hls_coordinator", str(refused.exception))

    def test_a_project_without_a_repository_fails_startup(self):
        FakePlatform.joined, FakePlatform.code_repo_url = {"agents"}, ""
        with self.assertRaises(SystemExit) as refused:
            self.prepare()
        self.assertIn("no repository", str(refused.exception))


class BriefingTest(unittest.TestCase):
    def render(self) -> str:
        with mock.patch.dict(os.environ, LAUNCH_ENV, clear=True):
            cfg = config.Config()
        return core.system_prompt(cfg, "RULES\n", "COMMANDS", "https://github.com/acme/model.git")

    def test_it_renders_with_the_rules_the_commands_and_its_assignment(self):
        prompt = self.render()
        self.assertIn("\n\nRULES\n\n## hl commands\n\nCOMMANDS\n\n", prompt)
        self.assertIn("clone https://github.com/acme/model.git and work on branch `hl/pe-1/coordinator`", prompt)
        self.assertIn("(:Objective {id:'pe-1'})", prompt)
        self.assertIsNone(re.search(r"\{[a-z_]+\}", prompt), "a placeholder was left unfilled")

    def test_it_acts_through_hl_and_names_no_host_tool_and_no_http_endpoint(self):
        prompt = self.render()
        self.assertIsNone(re.search(r"podman|kubectl|\bmake\b|docker|(GET|POST|PUT) /|\$API_URL|curl", prompt))
        self.assertLess(len(prompt.encode()), 9_000)

    def test_the_commands_listed_for_a_coordinator_are_the_ones_that_change_roster_and_brief(self):
        asked = []

        def fake_run(command, **_):
            asked.append(command[1:-1])
            return mock.Mock(stdout="usage\n")

        with mock.patch.object(core.subprocess, "run", fake_run):
            core.hl_commands(True)
            self.assertIn(["platform-experiments", "agents", "add"], asked)
            self.assertIn(["platform-experiments", "update"], asked)
            asked.clear()
            core.hl_commands()
        self.assertEqual(asked, [[], ["job"], ["hypothesis"]])

    def test_the_prompt_file_ships_beside_the_agents_briefing(self):
        self.assertTrue((Path(core.__file__).parent / "prompts" / "coordinator.md").is_file())


class WatchTest(unittest.IsolatedAsyncioTestCase):
    def test_a_coordinator_watches_the_whole_experiment_and_is_handed_no_agent_id(self):
        with mock.patch.dict(os.environ, {"AGENT_ID": "left-over"}):
            env = updates.watch_env("http://api", "", "pe-1", "/tmp/w")
        self.assertNotIn("AGENT_ID", env)
        self.assertEqual(updates.watch_command(env), ["hl", "watch", "--all"])
        own = updates.watch_env("http://api", "ag-1", "pe-1", "/tmp/w")
        self.assertEqual((own["AGENT_ID"], updates.watch_command(own)), ("ag-1", ["hl", "watch"]))

    async def test_an_ended_job_reaches_a_coordinator_as_it_is_with_no_read_as_an_agent(self):
        command, _ = fake_hl(f"echo '{COMPLETED}'\n", status="{}", stats="{}", quota="{}")
        env = {k: v for k, v in os.environ.items() if k != "AGENT_ID"}
        events = await updates.drain(env, command)
        self.assertEqual([e["kind"] for e in events], ["experiment.status"])
        self.assertFalse(os.path.exists(command[0] + ".reads"))

    def test_the_last_turn_of_a_coordinator_asks_for_no_job_summary(self):
        self.assertNotIn("summar", run._COORDINATOR_SHUTDOWN_PROMPT)
        self.assertIn("push", run._COORDINATOR_SHUTDOWN_PROMPT)


if __name__ == "__main__":
    unittest.main()
