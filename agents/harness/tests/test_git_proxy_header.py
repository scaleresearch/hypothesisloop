import http.server
import os
import subprocess
import tempfile
import threading
import unittest
from unittest import mock

from hypothesisloop_agent import core

TOKEN = "agent-token-1"


class _Recorder(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.server.seen.append((self.path.split("?")[0], self.headers.get("Authorization")))
        self.send_response(404)
        self.end_headers()

    def log_message(self, *args):
        pass


def _serve(test):
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _Recorder)
    server.seen = []
    threading.Thread(target=server.serve_forever, daemon=True).start()
    test.addCleanup(server.server_close)
    test.addCleanup(server.shutdown)
    return server


class GitTokenScopeTest(unittest.TestCase):
    """The agent's token goes to the platform's git proxy for its repository and nowhere else: not
    to another repository of the same host, nor to any other host the agent's git reaches."""

    def test_the_token_header_is_sent_to_the_proxy_url_alone(self):
        proxy, elsewhere = _serve(self), _serve(self)
        base = f"http://127.0.0.1:{proxy.server_port}"
        git_url = f"{base}/git/pe-1.git"
        home = tempfile.mkdtemp()
        with mock.patch.dict(os.environ, {"HOME": home, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_COUNT": "0"}):
            core.git_through_platform("https://github.com/acme/model.git", git_url, TOKEN)

            def asked(url):
                subprocess.run(["git", "ls-remote", url], capture_output=True, timeout=30, cwd=home)

            asked("https://github.com/acme/model.git")
            asked(git_url)
            asked(f"{base}/git/pe-2.git")
            asked(f"{base}/git/pe-1.git-other")
            asked(f"http://127.0.0.1:{elsewhere.server_port}/acme/model.git")

        sent = {path: auth for path, auth in proxy.seen}
        self.assertEqual(sent.get("/git/pe-1.git/info/refs"), "Bearer " + TOKEN, proxy.seen)
        self.assertEqual(sum(1 for path, _ in proxy.seen if path == "/git/pe-1.git/info/refs"), 2,
                         "the repository's own address was not rewritten to the proxy")
        self.assertIsNone(sent.get("/git/pe-2.git/info/refs"), "token sent to another repository of the proxy")
        self.assertIsNone(sent.get("/git/pe-1.git-other/info/refs"), "token sent to an address that only begins like the proxy's")
        self.assertTrue(elsewhere.seen)
        self.assertTrue(all(auth is None for _, auth in elsewhere.seen), elsewhere.seen)
        self.assertTrue(all(auth == "Bearer " + TOKEN for path, auth in proxy.seen if path.startswith("/git/pe-1.git/")), proxy.seen)

    # The workspace clone goes through the same rewrite, with the token on the proxy's address.
    def test_the_workspace_clone_reaches_the_proxy_with_the_token(self):
        proxy = _serve(self)
        git_url = f"http://127.0.0.1:{proxy.server_port}/git/pe-1.git"
        home, workdir = tempfile.mkdtemp(), tempfile.mkdtemp()
        with mock.patch.dict(os.environ, {"HOME": home, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_COUNT": "0"}):
            core.git_through_platform("https://github.com/acme/model.git", git_url, TOKEN)
            with self.assertRaises(SystemExit):
                core.clone_workspace("https://github.com/acme/model.git", "main", workdir)
        self.assertIn(("/git/pe-1.git/info/refs", "Bearer " + TOKEN), proxy.seen)
        self.assertTrue(all(auth == "Bearer " + TOKEN for _, auth in proxy.seen), proxy.seen)


if __name__ == "__main__":
    unittest.main()
