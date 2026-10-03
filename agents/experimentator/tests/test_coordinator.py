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
        env = {**LAUNCH_ENV, "API_URL": self.url, "AGENT_WORKDIR": self.workdir, "AGENT_STATE_DIR": self.workdir,
               "AGENT_VENV": "/agent/.venv", "PATH": "/usr/bin"}
        with mock.patch.dict(os.environ, env, clear=True), \
                mock.patch.object(core, "hl_commands", return_value="usage: hl") as commands:
            setup = core.prepare()
        commands.assert_called_once_with(setup.cfg)
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
        self.assertIn("run `git clone https://github.com/acme/model.git .` in /agent/workspace, the directory you start in", " ".join(prompt.split()))
        self.assertIn('hl hypothesis query "MATCH (d:Hypothesis)-[:BUILDS_ON]->(:Objective) '
                      'OPTIONAL MATCH (h:Hypothesis)-[:BUILDS_ON*0..]->(d:Hypothesis) RETURN d.id, d.text, h.status, count(h)"',
                      " ".join(prompt.split()))
        self.assertIsNone(re.search(r"\{[a-z_]+\}", prompt), "a placeholder was left unfilled")

    def test_it_acts_through_hl_and_names_no_host_tool_and_no_http_endpoint(self):
        prompt = self.render()
        self.assertIsNone(re.search(r"podman|kubectl|\bmake\b|docker|(GET|POST|PUT) /|\$API_URL|curl", prompt))
        self.assertLess(len(prompt.encode()), 7_760)

    # A coordinator of an accepted draft read "not started" and waited 36 minutes for a start
    # that only its own baseline agent could bring. The `status_reason` of an open experiment
    # now names the baseline agent to add and the declaration, each with its command.
    def test_its_next_step_on_an_open_experiment_is_read_from_status_reason(self):
        flat = " ".join(self.render().split())
        self.assertIn("`status_reason` (while `open`, your next step and its command)", flat)
        self.assertIn("The run's duration counts from the start the platform gives it once a valid baseline is declared, "
                      "and an experiment still `open` at its `baseline_due_at` is closed", flat)
        # One wrote "proceeding" of a declared baseline whose run had no researcher to start for.
        self.assertIn("Once declared, `status_reason` reads `starting` or what the start waits for: a researcher it lacks you add", flat)
        self.assertNotIn("starts_at", flat)

    # One removed the owner's researcher to "match max_agents" and cancelled the baseline
    # agent's queued job itself. Both are refused now, each with whose it is.
    def test_it_changes_nothing_of_the_owners_and_nothing_of_an_agents(self):
        flat = " ".join(self.render().split())
        self.assertIn("What the run may spend, how long it lasts and its gate are the owner's", flat)
        self.assertIn("An agent's jobs, hypotheses and branches are the agent's: what is wrong with a job you say in the brief", flat)
        for said_there in ("agents remove", "--help", "refuses", "--role baseline", "baseline_breaks_constraint"):
            self.assertNotIn(said_there, flat)

    # Three baseline jobs failed for what the brief said of the image before a coordinator
    # looked at one.
    def test_a_failed_baseline_job_is_the_agents_to_fix_and_the_briefs_cause_the_coordinators(self):
        flat = " ".join(self.render().split())
        self.assertIn("and while it is `open` when a job ends", flat)
        self.assertIn("A failed baseline job is its agent's to fix and run again. Read `hl job logs ID`: "
                      "if the cause is in the brief (an image, a type, a config that cannot run), fix the brief", flat)
        self.assertIn("State in it only what you read on the platform", flat)

    # A run closed under a summary that said it was "waiting for the competitor agent", one
    # still open was written up as "proceeding". Told to end its turn by replying in one line
    # with no tool call, a coordinator wrote what it saw, ran `echo ok` and then wrote the one
    # line, six times in a run: the briefing asked for a closing reply of a set form, and only
    # a command separates two replies. What it writes last is the end, in whatever words.
    def test_the_last_summary_is_written_before_the_close_and_a_turn_ends_with_the_last_reply(self):
        flat = " ".join(self.render().split())
        self.assertIn("Rewrite it as your last act before `hl platform-experiments close`: what was found, in the past tense, "
                      "with nothing in it still waiting or under way", flat)
        self.assertIn("It opens with the `status` that `get` shows now, and of an `open` experiment its `status_reason`: no run is under way", flat)
        self.assertIn("Once nothing needs you, say what you saw and stop: a reply with no command in it is the end of your turn", flat)
        for form in ("one line", "call no tool", "echo"):
            self.assertNotIn(form, flat)
        self.assertEqual(updates.message([{"kind": "comment.new", "subject": "hyp-1"}], ""), "Platform updates:\ncomment.new hyp-1")

    # Every graph of the audited runs was three root hypotheses with nothing below them, two
    # of them varying the same knobs, and no summary said how the best result was reached.
    def test_it_checks_the_shape_of_the_graph_and_its_last_summary_names_the_path_to_the_best_finding(self):
        flat = " ".join(self.render().split())
        self.assertIn("The objective's children are distinct approaches, each one claim, a refinement or a combination "
                      "sits under what it refines, and the best result is a node", flat)
        self.assertIn("Roots that overlap, or stay flat while one holds a whole sweep, you comment on: "
                      "`hl hypothesis comment ID --text T`", flat)
        self.assertIn("and the path from the objective to the best finding, hypothesis by hypothesis", flat)

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
            core.hl_commands(SimpleNamespace(flavor="coordinator", role=""))
            self.assertIn(["platform-experiments", "agents", "add"], asked)
            self.assertIn(["platform-experiments", "update"], asked)
            self.assertIn(["platform-experiments", "summary"], asked)
            self.assertIn(["platform-experiments", "close"], asked)
            asked.clear()
            core.hl_commands(SimpleNamespace(flavor="generalist", role="competitor"))
        self.assertEqual(asked, [[], ["job"], ["hypothesis"]])

    def test_the_prompt_file_ships_beside_the_agents_briefing(self):
        self.assertTrue((Path(core.__file__).parent / "prompts" / "coordinator.md").is_file())


class WatchTest(unittest.IsolatedAsyncioTestCase):
    def test_the_token_says_whether_the_watch_is_a_coordinators_of_the_whole_experiment(self):
        env = updates.watch_env("http://api", COORDINATOR_TOKEN, "/tmp/w")
        self.assertEqual(updates.watch_command(env), ["hl", "watch", "--all", "--decisions"])
        own = updates.watch_env("http://api", AGENT_TOKEN, "/tmp/w")
        self.assertEqual((own["HL_TOKEN"], updates.watch_command(own)), (AGENT_TOKEN, ["hl", "watch"]))

    async def test_an_ended_job_reaches_a_coordinator_with_its_machine_and_no_read_as_an_agent(self):
        command, _ = fake_hl(f"echo '{COMPLETED}'\n", status='{"id":"exp-1","ran_on":{"node":"n1"}}', stats="{}", quota="{}")
        events = await updates.drain({**os.environ, "HL_TOKEN": COORDINATOR_TOKEN}, command)
        self.assertEqual(updates.message(events, ""), "Platform updates:\nexperiment.status exp-1 COMPLETED on node n1 by agent-a")
        with open(command[0] + ".reads") as f:
            self.assertEqual(f.read(), "job status exp-1\n")

    def test_the_last_turn_of_a_coordinator_writes_the_summary_and_pushes_nothing(self):
        self.assertNotIn("job", run._COORDINATOR_SHUTDOWN_PROMPT)
        self.assertNotIn("push", run._COORDINATOR_SHUTDOWN_PROMPT)
        self.assertIn("hl platform-experiments summary", run._COORDINATOR_SHUTDOWN_PROMPT)

    # A coordinator wrote "closed by owner at 02:48" for a run that had reached its end time:
    # nothing told it why it was being stopped. The moment named is closed_at, which every closed
    # platform experiment carries: one closed before it ran has no ends_at.
    def test_the_last_turn_opens_with_why_the_run_stops_as_the_platform_has_it(self):
        ended = {"status": "closed", "closed_at": "2026-01-01T02:48:05Z"}
        person = {"kind": "user", "id": "u-1", "name": "Ada"}
        for experiment, signup, flavor, want in (
            ({**ended, "close_reason": "end_time_reached"}, {}, "coordinator", "reached its end time and the platform closed it at 2026-01-01T02:48:05Z"),
            ({**ended, "close_reason": "closed_by_request", "closed_by": person}, {}, "coordinator", "a person closed it (Ada) at 2026-01-01T02:48:05Z"),
            ({**ended, "close_reason": "closed_by_request", "closed_by": {"kind": "coordinator"}}, {"ended": "closed"}, "generalist", "its coordinator closed it"),
            (ended, {}, "coordinator", "The platform recorded no reason"),
            ({**ended, "close_reason": "planning_limit_reached"}, {"ended": "closed"}, "planner", "the planning limit passed with nothing proposed, and the platform closed the draft at 2026-01-01T02:48:05Z"),
            ({**ended, "close_reason": "baseline_limit_reached"}, {"ended": "closed"}, "generalist", "was not running by its baseline limit, and the platform closed it at 2026-01-01T02:48:05Z before it started"),
            ({"status": "running"}, {}, "coordinator", "Only this machine is being stopped, and the platform does not say why"),
            ({"status": "running"}, {"ended": "cut"}, "generalist", "you were cut from the platform experiment at a stage boundary"),
        ):
            setup = SimpleNamespace(cfg=SimpleNamespace(agent_id="ag-1", flavor=flavor, platform_experiment_id="pe-1"),
                                    client=SimpleNamespace(get_platform_experiment=lambda _id, e=experiment: e, get_signup=lambda *_, s=signup: s))
            prompt = run._shutdown_prompt(setup)
            self.assertTrue(prompt.startswith("You are being stopped"), prompt)
            self.assertIn(want, prompt)
            self.assertNotIn("owner", prompt)

    # A researcher that finished, a planner whose proposal was stored and a coordinator that
    # closed its own run were each interrupted in their closing words and given a turn to file
    # what they did not owe.
    def test_an_agent_whose_own_action_ended_its_run_gets_no_last_turn(self):
        closed = {"status": "closed", "close_reason": "closed_by_request", "closed_by": {"kind": "coordinator"}}
        for experiment, signup, flavor in (
            ({"status": "running"}, {"ended": "finished"}, "generalist"),
            ({"status": "draft"}, {"ended": "planned"}, "planner"),
            (closed, {}, "coordinator"),
        ):
            setup = SimpleNamespace(cfg=SimpleNamespace(agent_id="ag-1", flavor=flavor, platform_experiment_id="pe-1"),
                                    client=SimpleNamespace(get_platform_experiment=lambda _id, e=experiment: e, get_signup=lambda *_, s=signup: s))
            self.assertIsNone(run._shutdown_prompt(setup), flavor)


if __name__ == "__main__":
    unittest.main()
