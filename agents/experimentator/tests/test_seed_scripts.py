import ast
import contextlib
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest import mock

REPO = Path(__file__).resolve().parents[3]
SEED = Path(__file__).resolve().parents[2] / "coordinator/experiments/dummy-data-search-demo/seed"

# Every script that carries its own copy of post_metric: seeds are copied into an experiment's
# repository and workloads into an image, so none of them can import a shared one.
METRIC_HELPERS = [
    "agents/coordinator/experiments/template/seed/metrics.py",
    "agents/coordinator/experiments/shampoo-family-optimizers/seed/metrics.py",
    "agents/coordinator/experiments/sparse-sdpa/seed/harness.py",
    "agents/coordinator/experiments/dummy-data-search-demo/seed/train.py",
    "agents/coordinator/experiments/dummy-tt-matmul/seed/train.py",
    "tests/workloads/generic/train.py",
    "tests/workloads/generic/data_stage.py",
    "tests/workloads/generic/train_distributed.py",
    "tests/workloads/nvidia/train.py",
    "tests/workloads/tenstorrent/train.py",
]


def load_post_metric(path, api_url):
    """The file's post_metric alone, without importing torch or opening a device."""
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
        self.requests = []

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                answering.requests.append((self.path, self.headers.get("Authorization")))
                self.rfile.read(int(self.headers.get("Content-Length", 0)))
                self.send_response(status)
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


class BuildAndPushTest(unittest.TestCase):
    def test_names_the_missing_registry_in_one_line(self):
        with tempfile.TemporaryDirectory() as repo:
            shutil.copytree(SEED, Path(repo) / "seed")
            subprocess.run(["git", "init", "-q", repo], check=True)
            env = {k: v for k, v in os.environ.items() if k != "REGISTRY"}
            done = subprocess.run(["bash", "seed/build_and_push.sh"], cwd=repo, env=env, capture_output=True, text=True)
        self.assertEqual(done.returncode, 1)
        self.assertEqual(done.stdout, "")
        self.assertEqual(len(done.stderr.splitlines()), 1)
        self.assertIn("set REGISTRY=host:port", done.stderr)


if __name__ == "__main__":
    unittest.main()
