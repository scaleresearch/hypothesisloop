import base64
import json
import os
import shutil
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest import mock

from hypothesisloop_agent import api_client, config, core, instructions, prompt


def derived(scope: str) -> str:
    """A token shaped like the ones the platform derives for scope."""
    return "hls_" + base64.urlsafe_b64encode(scope.encode()).decode().rstrip("=") + ".mac"


AGENT_TOKEN = derived("agent:pe-1/agent-a")


class FakePlatform(BaseHTTPRequestHandler):
    """Answers like the platform: an agent that was added reads its own signup, and the token of
    one that was not is refused before any handler runs."""

    joined: set = set()
    signup: dict = {}
    calls: list = []
    tokens: set = set()
    status_for_strangers = 401
    repository_url = "https://github.com/acme/model.git"
    git_url = "https://api.example.invalid/git/pe-1.git"
    branch = "main"
    instructions: dict = {}

    def _reply(self, status, body):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(body).encode())

    def do_GET(self):
        FakePlatform.calls.append(("GET", self.path))
        FakePlatform.tokens.add(self.headers["Authorization"])
        if self.path.endswith("/instructions"):
            role = "coordinator" if self.path.endswith("/coordinator/instructions") else FakePlatform.instructions["role"]
            return self._reply(200, {**FakePlatform.instructions, "role": role})
        if self.path == "/platform-experiments/pe-1":
            body = {"id": "pe-1", "status": "running", "coordinator": {"model": "model-1"}}
            if FakePlatform.repository_url:
                body["repository_url"] = FakePlatform.repository_url
            if FakePlatform.branch:
                body["branch"] = FakePlatform.branch
            if FakePlatform.git_url:
                body["git_url"] = FakePlatform.git_url
            return self._reply(200, body)
        agent_id = self.path.rsplit("/", 1)[-1]
        if agent_id in FakePlatform.joined:
            return self._reply(200, {"agent_id": agent_id, "role": "competitor", **FakePlatform.signup})
        self._reply(FakePlatform.status_for_strangers, {"message": "unauthenticated: agent is not in platform experiment"})

    def do_POST(self):
        FakePlatform.calls.append(("POST", self.path))
        self._reply(500, {"message": "the harness must not write at startup"})

    def log_message(self, *args):
        pass


def served(**changes) -> dict:
    """Instructions as the platform answers them."""
    return {"agent_type": "generalist", "agent_type_version": 1, "role": "researcher",
            "prompt": "Work in {{workdir}} with:\n{{hl_commands}}", "placeholders": ["hl_commands", "workdir"],
            "settings": {"model": "model-1", "max_subagents": 4, "effort": "high",
                         "hyperparameters": {"batch_size": 64}},
            "rules_version": 1, "digest": "digest-1", **changes}


class PlatformCase(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        FakePlatform.joined, FakePlatform.calls, FakePlatform.tokens = {"agent-a"}, [], set()
        FakePlatform.signup = {"agent_type": "generalist", "model": "model-1", "hyperparameters": {"batch_size": 64}}
        FakePlatform.status_for_strangers = 401
        FakePlatform.repository_url = "https://github.com/acme/model.git"
        FakePlatform.git_url = "https://api.example.invalid/git/pe-1.git"
        FakePlatform.branch = "main"
        FakePlatform.instructions = served()
        self.server = HTTPServer(("127.0.0.1", 0), FakePlatform)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        self.client = api_client.PlatformClient(self.url, "agent-token")


class RequireJoinedTest(PlatformCase):
    async def test_an_added_agent_starts_after_one_read_and_no_write(self):
        await self.client.require_joined("agent-a", "pe-1")
        self.assertEqual(FakePlatform.calls, [("GET", "/platform-experiments/pe-1/signups/agent-a")])

    async def test_every_call_carries_the_token(self):
        await self.client.require_joined("agent-a", "pe-1")
        self.assertEqual(FakePlatform.tokens, {"Bearer agent-token"})

    async def test_an_agent_nobody_added_fails_startup_and_is_told_how_to_join(self):
        for status in (401, 403, 404):
            FakePlatform.status_for_strangers = status
            with self.assertRaises(SystemExit) as ctx:
                await self.client.require_joined("agent-b", "pe-1")
            message = str(ctx.exception)
            self.assertIn("agent-b is not in platform experiment pe-1", message)
            self.assertIn("hl platform-experiments agents add --platform-experiment pe-1", message)
            self.assertNotIn("agent-token", message)
        self.assertNotIn("POST", [method for method, _ in FakePlatform.calls])

    async def test_a_platform_failure_is_not_read_as_not_joined(self):
        FakePlatform.status_for_strangers = 500
        with self.assertRaises(api_client.APIError) as ctx:
            await self.client.require_joined("agent-b", "pe-1")
        self.assertEqual(ctx.exception.status, 500)

    async def test_an_unreachable_platform_is_not_read_as_not_joined(self):
        client = api_client.PlatformClient("http://127.0.0.1:1", "agent-token")
        with self.assertRaises(api_client.APIError) as ctx:
            await client.require_joined("agent-a", "pe-1")
        self.assertEqual(ctx.exception.status, 0)


class StartupTest(PlatformCase):
    def environment(self, **overrides):
        env = {"API_URL": self.url, "HL_TOKEN": AGENT_TOKEN, "AGENT_WORKDIR": self.workdir, "AGENT_STATE_DIR": self.workdir,
               "AGENT_VENV": "/agent/.venv", "PATH": "/usr/bin"}
        env.update(overrides)
        return {k: v for k, v in env.items() if v is not None}

    def setUp(self):
        super().setUp()
        self.workdir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.workdir)

    async def prepare(self, **overrides):
        with mock.patch.dict(os.environ, self.environment(**overrides), clear=True), \
                mock.patch.object(prompt, "hl_commands", return_value="usage: hl"), \
                mock.patch.object(core, "clone_workspace") as self.clone:
            return await core.prepare()

    async def test_prepare_reads_who_it_is_from_the_token_and_what_it_runs_as_from_its_instructions(self):
        # What a stale environment says of who this is counts for nothing.
        FakePlatform.instructions = served(role="baseline")
        setup = await self.prepare(AGENT_ID="left-over", PLATFORM_EXPERIMENT_ID="pe-stale", AGENT_FLAVOR="coordinator", AGENT_MODEL="other")
        cfg = setup.cfg
        self.assertEqual((cfg.agent_id, cfg.platform_experiment_id, cfg.role), ("agent-a", "pe-1", "baseline"))
        self.assertEqual(setup.commands, "usage: hl")
        self.assertEqual(FakePlatform.calls, [
            ("GET", "/platform-experiments/pe-1/signups/agent-a"),
            ("GET", "/platform-experiments/pe-1"),
            ("GET", "/platform-experiments/pe-1/agents/agent-a/instructions"),
        ])

    # An agent's `pip install` went to a user site the python3 on its PATH, the harness's, never
    # read, and three more tries passed before it had a place to install into.
    async def test_prepare_puts_the_agents_own_virtualenv_first_for_everything_it_starts(self):
        with mock.patch.dict(os.environ, self.environment(VIRTUAL_ENV="/opt/hypothesisloop-agent/.venv"), clear=True), \
                mock.patch.object(prompt, "hl_commands", return_value="usage: hl"), mock.patch.object(core, "clone_workspace"):
            await core.prepare()
            self.assertEqual((os.environ["VIRTUAL_ENV"], os.environ["PATH"]), ("/agent/.venv", "/agent/.venv/bin:/usr/bin"))
        with self.assertRaises(SystemExit) as ctx:
            await self.prepare(AGENT_VENV=None)
        self.assertIn("AGENT_VENV", str(ctx.exception))

    # Left out, both once fell to a path under whatever directory the harness was started in.
    async def test_prepare_requires_where_the_agent_works_and_where_the_harness_keeps_its_state(self):
        for missing in ("AGENT_WORKDIR", "AGENT_STATE_DIR"):
            with self.assertRaises(SystemExit) as ctx:
                await self.prepare(**{missing: None})
            self.assertIn(f"missing required env var: {missing}", str(ctx.exception))

    # An experiment states its branch, and every agent clones that one: a clone of the default
    # branch worked on code the experiment does not run.
    async def test_prepare_clones_the_branch_the_platform_experiment_names_and_refuses_one_that_names_none(self):
        setup = await self.prepare()
        self.clone.assert_called_once_with("https://github.com/acme/model.git", "main", os.path.abspath(self.workdir))
        self.assertEqual(setup.workdir, os.path.abspath(self.workdir))
        FakePlatform.branch = ""
        with self.assertRaises(SystemExit) as refused:
            await self.prepare()
        self.assertIn("platform experiment pe-1 names no branch", str(refused.exception))
        self.clone.assert_not_called()

    def test_the_workspace_is_cloned_at_the_branch_into_an_empty_directory_only(self):
        workdir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, workdir)
        with mock.patch.object(core.subprocess, "run", return_value=mock.Mock(returncode=0)) as run:
            core.clone_workspace("https://github.com/acme/model.git", "exp/throughput", workdir)
        self.assertEqual(run.call_args.args[0], ["git", "clone", "--branch", "exp/throughput", "https://github.com/acme/model.git", "."])
        self.assertEqual(run.call_args.kwargs["cwd"], workdir)
        open(os.path.join(workdir, "kept"), "w").close()
        with mock.patch.object(core.subprocess, "run") as run:
            core.clone_workspace("https://github.com/acme/model.git", "exp/throughput", workdir)
        run.assert_not_called()

    async def test_prepare_stops_an_agent_whose_platform_experiment_names_no_repository(self):
        FakePlatform.repository_url = ""
        with self.assertRaises(SystemExit) as refused:
            await self.prepare()
        self.assertIn("platform experiment pe-1 names no repository", str(refused.exception))

    # The agent's git reaches the repository through the platform's git proxy with the agent's own
    # token, and no credential of the git host is handed to it.
    async def test_prepare_sends_git_for_the_repository_to_the_platforms_git_proxy(self):
        with mock.patch.dict(os.environ, self.environment(GIT_CONFIG_COUNT="1", GIT_CONFIG_KEY_0="core.pager", GIT_CONFIG_VALUE_0="cat"), clear=True), \
                mock.patch.object(prompt, "hl_commands", return_value="usage: hl"), mock.patch.object(core, "clone_workspace"):
            await core.prepare()
            self.assertEqual(os.environ["GIT_CONFIG_COUNT"], "2")
            self.assertEqual((os.environ["GIT_CONFIG_KEY_0"], os.environ["GIT_CONFIG_VALUE_0"]), ("core.pager", "cat"))
            self.assertEqual(os.environ["GIT_CONFIG_KEY_1"], "url.https://api.example.invalid/git/pe-1.git.insteadOf")
            self.assertEqual(os.environ["GIT_CONFIG_VALUE_1"], "https://github.com/acme/model.git")
            self.assertEqual(os.environ["HL_GIT_URL"], "https://api.example.invalid/git/pe-1.git")
        FakePlatform.git_url = ""
        with self.assertRaises(SystemExit) as refused:
            await self.prepare()
        self.assertIn("names no git_url", str(refused.exception))

    async def test_prepare_stops_an_agent_nobody_added_before_it_reads_its_instructions(self):
        with self.assertRaises(SystemExit):
            await self.prepare(HL_TOKEN=derived("agent:pe-1/agent-b"))
        self.assertEqual(FakePlatform.calls, [("GET", "/platform-experiments/pe-1/signups/agent-b")])

    async def test_prepare_requires_the_address_and_a_token_that_says_who_it_is(self):
        for missing in ("HL_TOKEN", "API_URL"):
            FakePlatform.calls = []
            with self.assertRaises(SystemExit) as ctx:
                await self.prepare(**{missing: None})
            self.assertIn(missing, str(ctx.exception))
            self.assertEqual(FakePlatform.calls, [], f"the platform was called without {missing}")
            with self.assertRaises(SystemExit):
                await self.prepare(**{missing: ""})
        for token in ("hlk_an-api-key", "hls_not-base64.mac", derived("launcher:main"), derived("agent:pe-1")):
            with self.assertRaises(SystemExit) as ctx:
                await self.prepare(HL_TOKEN=token)
            self.assertIn("neither an agent's token nor a coordinator's", str(ctx.exception))
            self.assertNotIn(token, str(ctx.exception))
        self.assertEqual(FakePlatform.calls, [])

    # A harness that ran instructions it could not read whole would run something nobody chose.
    async def test_instructions_the_harness_cannot_run_fail_startup(self):
        settings = served()["settings"]
        for broken, said in (
            (served(settings={**settings, "model": ""}), "name no model"),
            (served(settings={k: v for k, v in settings.items() if k != "effort"}), "settings lack the fields effort"),
            (served(role="coordinator"), "cannot run as"),
            (served(prompt="{{rules}} and {{workdir}}"), "holds '{{rules}} and '"),
            (served(placeholders=["hl_commands", "workdir", "secret"]), "leaves secret to the harness, which fills only hl_commands, workdir"),
        ):
            FakePlatform.instructions = broken
            with self.assertRaises(SystemExit) as ctx:
                await self.prepare()
            self.assertIn(said, str(ctx.exception))

    async def test_an_agent_the_platform_serves_no_instructions_fails_startup(self):
        client = api_client.PlatformClient(self.url, "agent-token")
        with mock.patch.object(client, "get_json", side_effect=api_client.APIError("GET", "/x", 404, "instructions not found")), \
                mock.patch.dict(os.environ, {"API_URL": self.url, "HL_TOKEN": AGENT_TOKEN}, clear=True), \
                self.assertRaises(SystemExit) as ctx:
            await instructions.read(client, config.Config())
        self.assertIn("serves agent-a no instructions (HTTP 404)", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
