import json
import os
import shutil
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest import mock

from hypothesisloop_agent import api_client, config, core


class FakePlatform(BaseHTTPRequestHandler):
    """Answers like the platform: an agent that was added reads its own signup, and the token of
    one that was not is refused before any handler runs."""

    joined: set = set()
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
            body = {"id": "pe-1", "status": "running"}
            if FakePlatform.repository_url:
                body["repository_url"] = FakePlatform.repository_url
            return self._reply(200, body)
        agent_id = self.path.rsplit("/", 1)[-1]
        if agent_id in FakePlatform.joined:
            return self._reply(200, {"agent_id": agent_id, "role": "competitor"})
        self._reply(FakePlatform.status_for_strangers, {"error": "unauthenticated: agent is not in platform experiment"})

    def do_POST(self):
        FakePlatform.calls.append(("POST", self.path))
        self._reply(500, {"error": "the harness must not write at startup"})

    def log_message(self, *args):
        pass


class PlatformCase(unittest.TestCase):
    def setUp(self):
        FakePlatform.joined, FakePlatform.calls, FakePlatform.tokens = {"agent-a"}, [], set()
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
            self.assertIn("hl platform-experiments agents add pe-1", message)
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
        env = {"API_URL": self.url, "HL_TOKEN": "agent-token", "AGENT_ID": "agent-a", "PLATFORM_EXPERIMENT_ID": "pe-1",
               "AGENT_FLAVOR": "generalist", "AGENT_MODEL": "model-1", "AGENT_WORKDIR": self.workdir}
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

    def test_prepare_checks_membership_before_anything_else_and_never_writes(self):
        setup = self.prepare()
        self.assertEqual(setup.cfg.agent_id, "agent-a")
        self.assertEqual(FakePlatform.calls, [
            ("GET", "/platform-experiments/pe-1/signups/agent-a"),
            ("GET", "/platform-experiments/pe-1"),
            ("GET", "/explore/rules"),
        ])
        self.assertIn("clone https://github.com/acme/model.git", setup.system_prompt)

    def test_prepare_stops_when_the_platform_experiment_names_no_repository(self):
        FakePlatform.repository_url = ""
        with self.assertRaises(SystemExit) as ctx:
            self.prepare()
        self.assertIn("names no repository", str(ctx.exception))

    def test_the_repository_address_is_the_platforms_and_never_the_environments(self):
        setup = self.prepare(CODE_REPO_URL="git://example.invalid/other", GIT_TOKEN="a-token")
        self.assertNotIn("example.invalid", setup.system_prompt)
        self.assertNotIn("a-token", setup.system_prompt)

    def test_prepare_stops_an_agent_nobody_added_before_it_reads_the_rules(self):
        with self.assertRaises(SystemExit):
            self.prepare(AGENT_ID="agent-b")
        self.assertEqual(FakePlatform.calls, [("GET", "/platform-experiments/pe-1/signups/agent-b")])

    def test_prepare_requires_the_identity_and_the_token_it_was_handed(self):
        for missing in ("AGENT_ID", "HL_TOKEN", "PLATFORM_EXPERIMENT_ID", "API_URL", "AGENT_FLAVOR", "AGENT_MODEL"):
            FakePlatform.calls = []
            with self.assertRaises(SystemExit) as ctx:
                self.prepare(**{missing: None})
            self.assertIn(missing, str(ctx.exception))
            self.assertEqual(FakePlatform.calls, [], f"the platform was called without {missing}")
            with self.assertRaises(SystemExit):
                self.prepare(**{missing: ""})


if __name__ == "__main__":
    unittest.main()
