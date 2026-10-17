import ast
import contextlib
import io
import json
import os
import runpy
import sys
import tempfile
import threading
import unittest
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest import mock

REPO = Path(__file__).resolve().parents[3]
# A coordinator's experiments are git-ignored: the tests on them run where they are on disk.

# Every script that carries its own copy of post_metric: the template's and the seeds' are
# copied into an experiment's repository and workloads into an image, so none of them can import
# a shared one.
METRIC_HELPERS = [path for path in [
    "contract/repotemplate/files/metrics.py",
    "tests/workloads/generic/train.py",
    "tests/workloads/generic/data_stage.py",
    "tests/workloads/generic/train_distributed.py",
    "tests/workloads/nvidia/train.py",
    "tests/workloads/tenstorrent/train.py",
] if (REPO / path).exists()]


TEMPLATE_HELPER = "contract/repotemplate/files/metrics.py"


def load_post_metric(path, api_url):
    """The file's post_metric alone, without importing torch or opening a device. The template's
    imports nothing but the standard library and is loaded whole."""
    if path == TEMPLATE_HELPER:
        return runpy.run_path(str(REPO / path))["post_metric"]
    tree = ast.parse((REPO / path).read_text())
    function = next(n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name == "post_metric")
    for default in function.args.defaults:
        if isinstance(default, ast.Name):
            default.id = "METRIC"
    namespace = {"json": json, "os": os, "sys": sys, "urllib": urllib, "API_URL": api_url, "EXP_ID": "job-1", "METRIC": "loss"}
    exec(compile(ast.Module(body=[function], type_ignores=[]), path, "exec"), namespace)
    return namespace["post_metric"]


class Answering:
    """A control plane that answers every metric POST the same way."""

    def __init__(self, status, body):
        answering = self
        self.status = status
        self.requests = []
        self.bodies = []

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                answering.requests.append((self.path, self.headers.get("Authorization")))
                answering.bodies.append(json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0)))))
                self.send_response(answering.status)
                self.end_headers()
                self.wfile.write(body.encode())

            def log_message(self, *args):
                pass

        self.server = HTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.server.server_port}"

    def close(self):
        self.server.shutdown()
        self.server.server_close()


class MetricHelperTest(unittest.TestCase):
    def setUp(self):
        spool = tempfile.TemporaryDirectory()
        self.addCleanup(spool.cleanup)
        patch = mock.patch.object(tempfile, "tempdir", spool.name)
        patch.start()
        self.addCleanup(patch.stop)

    def post_three(self, path, status, body):
        control_plane = Answering(status, body)
        self.addCleanup(control_plane.close)
        env = {"HYPOTHESISLOOP_TOKEN": "job-token", "HYPOTHESISLOOP_API_URL": control_plane.url, "HYPOTHESISLOOP_EXPERIMENT_ID": "job-1"}
        stderr = io.StringIO()
        with mock.patch.dict(os.environ, env), contextlib.redirect_stderr(stderr):
            post_metric = load_post_metric(path, control_plane.url)
            for step in range(3):
                if "data_stage" in path or "train_distributed" in path:
                    post_metric("loss", 1.0, step / 3)
                else:
                    post_metric(step / 3, 1.0, "loss")
        return control_plane.requests, stderr.getvalue()

    def test_a_refused_token_is_reported_once_with_the_reason(self):
        for path in METRIC_HELPERS:
            for status in (401, 403):
                with self.subTest(path=path, status=status):
                    requests, stderr = self.post_three(path, status, '{"error":"unauthenticated: job job-1 is not running"}')
                    self.assertEqual(requests, [("/experiments/job-1/metrics", "Bearer job-token")] * 3)
                    self.assertEqual(len(stderr.splitlines()), 1, stderr)
                    self.assertIn(str(status), stderr)
                    self.assertIn("job job-1 is not running", stderr)

    def test_another_failure_is_reported_each_time_with_the_reason(self):
        for path in METRIC_HELPERS:
            with self.subTest(path=path):
                _, stderr = self.post_three(path, 422, '{"error":"metric_value must be finite"}')
                self.assertEqual(len(stderr.splitlines()), 3, stderr)
                self.assertIn("metric_value must be finite", stderr)

    def test_an_accepted_sample_says_nothing(self):
        for path in METRIC_HELPERS:
            with self.subTest(path=path):
                requests, stderr = self.post_three(path, 204, "")
                self.assertEqual(len(requests), 3)
                self.assertEqual(stderr, "")


class TemplateSpoolTest(unittest.TestCase):
    """The template's helper keeps a report the platform could not take and sends it later."""

    def setUp(self):
        spool = tempfile.TemporaryDirectory()
        self.addCleanup(spool.cleanup)
        self.control_plane = Answering(503, "")
        self.addCleanup(self.control_plane.close)
        env = {"HYPOTHESISLOOP_TOKEN": "job-token", "HYPOTHESISLOOP_API_URL": self.control_plane.url, "HYPOTHESISLOOP_EXPERIMENT_ID": "job-1"}
        self.at_exit = []
        for patch in (mock.patch.object(tempfile, "tempdir", spool.name), mock.patch.dict(os.environ, env),
                      mock.patch("atexit.register", lambda *args: self.at_exit.append(args)),
                      contextlib.redirect_stderr(io.StringIO())):
            patch.__enter__()
            self.addCleanup(patch.__exit__, None, None, None)
        self.module = runpy.run_path(str(REPO / TEMPLATE_HELPER))
        self.spool = self.module["_spool_path"]("job-1")

    def test_a_report_not_taken_is_sent_before_the_next(self):
        self.module["post_metric"](0.1, 1.0, "loss")
        self.module["post_metric"](0.2, 2.0, "loss")
        self.assertEqual(len(Path(self.spool).read_text().splitlines()), 2)
        self.assertEqual(len(self.at_exit), 1)
        self.control_plane.status = 204
        self.module["post_metric"](0.3, 3.0, "loss")
        self.assertEqual([b["metric_value"] for b in self.control_plane.bodies], [1.0, 1.0, 1.0, 2.0, 3.0])
        self.assertEqual(Path(self.spool).read_text(), "")

    def test_a_refusal_that_sending_again_cannot_change_is_not_kept(self):
        self.control_plane.status = 422
        self.module["post_metric"](0.1, 1.0, "loss")
        self.assertEqual(Path(self.spool).read_text(), "")

    def test_what_is_kept_is_sent_at_exit_with_retries(self):
        self.module["post_metric"](0.1, 1.0, "loss")
        url = f"{self.control_plane.url}/experiments/job-1/metrics"
        sleeps = []

        def sleep(s):
            sleeps.append(s)
            if len(sleeps) == 2:
                self.control_plane.status = 204

        with mock.patch("time.sleep", sleep):
            self.module["_flush_at_exit"](url, "job-token", self.spool)
        self.assertEqual(sleeps, [1, 2])
        self.assertEqual(len(self.control_plane.bodies), 4)
        self.assertEqual(Path(self.spool).read_text(), "")


if __name__ == "__main__":
    unittest.main()
