import base64
import json
import os
import shutil
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest import mock

from hypothesisloop_agent import api_client, config, core


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

    def _reply(self, status, body):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(body).encode())

    def do_GET(self):
        FakePlatform.calls.append(("GET", self.path))
        FakePlatform.tokens.add(self.headers["Authorization"])
        if self.path == "/explore/rules":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"## platform rules\n")
            return
        if self.path == "/platform-experiments/pe-1":
            body = {"id": "pe-1", "status": "running", "coordinator": {"model": "model-1"}}
            if FakePlatform.repository_url:
                body["repository_url"] = FakePlatform.repository_url
            return self._reply(200, body)
        agent_id = self.path.rsplit("/", 1)[-1]
        if agent_id in FakePlatform.joined:
            return self._reply(200, {"agent_id": agent_id, "role": "competitor", **FakePlatform.signup})
        self._reply(FakePlatform.status_for_strangers, {"error": "unauthenticated: agent is not in platform experiment"})

    def do_POST(self):
        FakePlatform.calls.append(("POST", self.path))
        self._reply(500, {"error": "the harness must not write at startup"})

    def log_message(self, *args):
        pass


class PlatformCase(unittest.TestCase):
    def setUp(self):
        FakePlatform.joined, FakePlatform.calls, FakePlatform.tokens = {"agent-a"}, [], set()
        FakePlatform.signup = {"flavor": "generalist", "model": "model-1", "hyperparameters": {"batch_size": 64}}
        FakePlatform.status_for_strangers = 401
        FakePlatform.repository_url = "https://github.com/acme/model.git"
        self.server = HTTPServer(("127.0.0.1", 0), FakePlatform)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        self.client = api_client.PlatformClient(self.url, "agent-token")


class RequireJoinedTest(PlatformCase):
    def test_an_added_agent_starts_after_one_read_and_no_write(self):
        self.client.require_joined("agent-a", "pe-1")
        self.assertEqual(FakePlatform.calls, [("GET", "/platform-experiments/pe-1/signups/agent-a")])

    def test_every_call_carries_the_token(self):
        self.client.require_joined("agent-a", "pe-1")
        self.assertEqual(FakePlatform.tokens, {"Bearer agent-token"})

    def test_a_restart_checks_again_and_still_writes_nothing(self):
        self.client.require_joined("agent-a", "pe-1")
        FakePlatform.calls = []
        self.client.require_joined("agent-a", "pe-1")
        self.assertEqual(FakePlatform.calls, [("GET", "/platform-experiments/pe-1/signups/agent-a")])

    def test_an_agent_nobody_added_fails_startup_and_is_told_how_to_join(self):
        for status in (401, 403, 404):
            FakePlatform.status_for_strangers = status
            with self.assertRaises(SystemExit) as ctx:
                self.client.require_joined("agent-b", "pe-1")
            message = str(ctx.exception)
            self.assertIn("agent-b is not in platform experiment pe-1", message)
            self.assertIn("hl platform-experiments agents add --platform-experiment pe-1", message)
            self.assertNotIn("agent-token", message)
        self.assertNotIn("POST", [method for method, _ in FakePlatform.calls])

    def test_a_platform_failure_is_not_read_as_not_joined(self):
        FakePlatform.status_for_strangers = 500
        with self.assertRaises(api_client.APIError) as ctx:
            self.client.require_joined("agent-b", "pe-1")
        self.assertEqual(ctx.exception.status, 500)

    def test_an_unreachable_platform_is_not_read_as_not_joined(self):
        client = api_client.PlatformClient("http://127.0.0.1:1", "agent-token")
        with self.assertRaises(api_client.APIError) as ctx:
            client.require_joined("agent-a", "pe-1")
        self.assertEqual(ctx.exception.status, 0)

    def test_the_client_has_no_way_to_register_or_sign_up(self):
        for gone in ("join", "register", "signup"):
            self.assertFalse(hasattr(self.client, gone), f"PlatformClient.{gone} exists")


class StartupTest(PlatformCase):
    """core.prepare is what both backends start with."""

    def environment(self, **overrides):
        env = {"API_URL": self.url, "HL_TOKEN": AGENT_TOKEN, "AGENT_WORKDIR": self.workdir}
        env.update(overrides)
        return {k: v for k, v in env.items() if v is not None}

    def setUp(self):
        super().setUp()
        self.workdir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.workdir)

    def prepare(self, **overrides):
        with mock.patch.dict(os.environ, self.environment(**overrides), clear=True), \
                mock.patch.object(core, "hl_commands", return_value="usage: hl"):
            return core.prepare()

    def test_prepare_reads_who_it_is_from_the_token_and_what_it_runs_as_from_its_signup(self):
        # What a stale environment says of who this is counts for nothing.
        setup = self.prepare(AGENT_ID="left-over", PLATFORM_EXPERIMENT_ID="pe-stale", AGENT_FLAVOR="coordinator", AGENT_MODEL="other")
        cfg = setup.cfg
        self.assertEqual((cfg.agent_id, cfg.platform_experiment_id), ("agent-a", "pe-1"))
        self.assertEqual((cfg.flavor, cfg.model, cfg.hyperparameters), ("generalist", "model-1", {"batch_size": 64}))
        self.assertEqual(FakePlatform.calls, [
            ("GET", "/platform-experiments/pe-1/signups/agent-a"),
            ("GET", "/platform-experiments/pe-1"),
            ("GET", "/explore/rules"),
        ])
        self.assertIn("clone https://github.com/acme/model.git", setup.system_prompt)

    # An agent of a platform experiment that names no repository used to exit at startup.
    def test_with_no_repository_the_agent_starts_and_is_told_where_its_code_goes(self):
        FakePlatform.repository_url = ""
        setup = self.prepare()
        self.assertEqual(setup.repository_url, "")
        flat = " ".join(setup.system_prompt.split())
        self.assertIn("this experiment has no repository. Run `git init` in the directory you start in", flat)
        self.assertIn("`hl code push` stores your committed HEAD with the platform", flat)
        self.assertNotIn("git push", flat)
        self.assertNotIn("github", flat)

    def test_the_repository_address_is_the_platforms_and_never_the_environments(self):
        setup = self.prepare(CODE_REPO_URL="git://example.invalid/other", GIT_TOKEN="a-token")
        self.assertNotIn("example.invalid", setup.system_prompt)
        self.assertNotIn("a-token", setup.system_prompt)

    def test_prepare_stops_an_agent_nobody_added_before_it_reads_the_rules(self):
        with self.assertRaises(SystemExit):
            self.prepare(HL_TOKEN=derived("agent:pe-1/agent-b"))
        self.assertEqual(FakePlatform.calls, [("GET", "/platform-experiments/pe-1/signups/agent-b")])

    def test_prepare_requires_the_address_and_a_token_that_says_who_it_is(self):
        for missing in ("HL_TOKEN", "API_URL"):
            FakePlatform.calls = []
            with self.assertRaises(SystemExit) as ctx:
                self.prepare(**{missing: None})
            self.assertIn(missing, str(ctx.exception))
            self.assertEqual(FakePlatform.calls, [], f"the platform was called without {missing}")
            with self.assertRaises(SystemExit):
                self.prepare(**{missing: ""})
        for token in ("hlk_an-api-key", "hls_not-base64.mac", derived("launcher:main"), derived("agent:pe-1")):
            with self.assertRaises(SystemExit) as ctx:
                self.prepare(HL_TOKEN=token)
            self.assertIn("neither an agent's token nor a coordinator's", str(ctx.exception))
            self.assertNotIn(token, str(ctx.exception))
        self.assertEqual(FakePlatform.calls, [])

    def test_a_signup_that_names_no_flavor_or_no_model_fails_startup(self):
        for gone in ("flavor", "model"):
            FakePlatform.signup = {"flavor": "generalist", "model": "model-1", gone: ""}
            with self.assertRaises(SystemExit) as ctx:
                self.prepare()
            self.assertIn("names no flavor or no model", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
