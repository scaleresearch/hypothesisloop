import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

from hypothesisloop_agent import api_client


class FakePlatform(BaseHTTPRequestHandler):
    """Answers like the platform: register 409s once the agent exists, a repeat signup succeeds."""

    agents: set = set()
    calls: list = []
    tokens: set = set()

    def _reply(self, status, body):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(body).encode())

    def do_POST(self):
        FakePlatform.calls.append(("POST", self.path))
        FakePlatform.tokens.add(self.headers["Authorization"])
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if self.path == "/agents":
            if body["id"] in FakePlatform.agents:
                return self._reply(409, {"detail": "agent already exists"})
            FakePlatform.agents.add(body["id"])
            return self._reply(201, {"id": body["id"]})
        self._reply(200, {"status": "signed_up"})

    def log_message(self, *args):
        pass


class JoinTest(unittest.TestCase):
    def setUp(self):
        FakePlatform.agents, FakePlatform.calls, FakePlatform.tokens = set(), [], set()
        self.server = HTTPServer(("127.0.0.1", 0), FakePlatform)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.client = api_client.PlatformClient(f"http://127.0.0.1:{self.server.server_port}", "agent-token")

    def test_first_start_registers_and_signs_up(self):
        self.client.join("agent-a", "pe-1")
        self.assertEqual(FakePlatform.calls, [
            ("POST", "/agents"),
            ("POST", "/platform-experiments/pe-1/signup"),
        ])

    def test_every_call_carries_the_token(self):
        self.client.join("agent-a", "pe-1")
        self.assertEqual(FakePlatform.tokens, {"Bearer agent-token"})

    def test_restart_joins_again(self):
        self.client.join("agent-a", "pe-1")
        FakePlatform.calls = []
        self.client.join("agent-a", "pe-1")
        self.assertEqual(FakePlatform.calls, [
            ("POST", "/agents"),
            ("POST", "/platform-experiments/pe-1/signup"),
        ])

    def test_refused_signup_fails_startup(self):
        original = FakePlatform.do_POST

        def refuse_signup(handler):
            if handler.path.endswith("/signup"):
                handler.rfile.read(int(handler.headers["Content-Length"]))
                return handler._reply(400, {"reason": "signup_closed: experiment is running"})
            original(handler)

        FakePlatform.do_POST = refuse_signup
        self.addCleanup(setattr, FakePlatform, "do_POST", original)
        with self.assertRaises(api_client.APIError) as ctx:
            self.client.join("agent-a", "pe-1")
        self.assertEqual(ctx.exception.status, 400)


if __name__ == "__main__":
    unittest.main()
