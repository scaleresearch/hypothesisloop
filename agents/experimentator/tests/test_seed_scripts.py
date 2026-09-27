import ast
import contextlib
import io
import json
import os
import sys
import threading
import unittest
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest import mock

REPO = Path(__file__).resolve().parents[3]
EXPERIMENTS = REPO / "agents/coordinator/experiments"

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


class ExperimentFilesTest(unittest.TestCase):
    def test_the_description_is_the_block_of_experiment_md(self):
        files = sorted(EXPERIMENTS.glob("*/.hypothesisloop/experiment.yaml"))
        self.assertTrue(files)
        for path in files:
            with self.subTest(experiment=path.parents[1].name):
                block = (path.parents[1] / "experiment.md").read_text().split("## EXPERIMENT DESCRIPTION", 1)[1]
                if "```" in block:
                    block = block.split("```")[1]
                scalar = path.read_text().split("\ndescription: |\n", 1)[1]
                lines = []
                for line in scalar.split("\n"):
                    if line.strip() and not line.startswith("  "):
                        break
                    lines.append(line[2:])
                self.assertEqual("\n".join(lines).strip(), "\n".join(line if line.strip() else "" for line in block.strip().split("\n")))

    def test_one_job_spec_and_no_placeholder_of_another_time(self):
        self.assertEqual(sorted(p for p in EXPERIMENTS.glob("*/seed/*.yaml")), [])
        for path in sorted(EXPERIMENTS.glob("*/.hypothesisloop/*.yaml")):
            with self.subTest(path=path.relative_to(EXPERIMENTS)):
                text = path.read_text()
                for stale in ("__WORKLOAD_IMAGE__", "hypothesis_id:", "max_agents:", "ends_at:"):
                    self.assertNotIn(stale, text)


class SampleJobFilesTest(unittest.TestCase):
    """The samples an agent's image carries: none retries a job, since a failed gate would only
    fail again, and every file one of them names is in the image."""

    def test_no_sample_retries_and_the_reference_they_name_is_shipped(self):
        samples = sorted(EXPERIMENTS.glob("*/.hypothesisloop/job.yaml"))
        self.assertTrue(samples)
        dockerfile = (REPO / "agents/experimentator/Dockerfile.base").read_text()
        shipped = "controlplane/settings/examples/experiment-submission.yaml"
        self.assertIn(f"COPY {shipped} $WORKLOAD_SAMPLES/", dockerfile)
        self.assertIn("! -name seed ! -name .hypothesisloop -exec rm -rf {} +", dockerfile)
        self.assertIn("-path '*/.hypothesisloop/*' ! -name job.yaml -delete", dockerfile)
        self.assertTrue((EXPERIMENTS / "template/seed/metrics.py").is_file())
        self.assertTrue((REPO / shipped).is_file())
        for path in samples:
            with self.subTest(path=path.name):
                text = path.read_text()
                self.assertNotIn("max_retries", text)
                if "experiment-submission.yaml" in text:
                    self.assertIn("under $WORKLOAD_SAMPLES", text)


class CatalogueImageJobTest(unittest.TestCase):
    """dummy-data-search-demo builds no image: its job clones its commit and installs what
    train.py imports."""

    def test_the_command_runs_train_py_of_the_clone_with_every_import_installed(self):
        demo = EXPERIMENTS / "dummy-data-search-demo"
        spec = (demo / ".hypothesisloop/job.yaml").read_text()
        line = next(line for line in spec.splitlines() if line.strip().startswith("command:"))
        shell, flag, script = json.loads(line.split("command:", 1)[1])
        self.assertEqual((shell, flag), ("bash", "-lc"))
        clone, run = script.split(" && ")
        self.assertEqual(clone, 'cd "$(hl-clone)"')
        words = run.split()
        self.assertEqual(words[:3] + words[-1:], ["exec", "uv", "run", "seed/train.py"])
        installed = {words[i + 1].split("==")[0] for i, word in enumerate(words) if word == "--with"}
        tree = ast.parse((demo / "seed/train.py").read_text())
        imported = {alias.name.split(".")[0] for node in ast.walk(tree) if isinstance(node, ast.Import) for alias in node.names}
        self.assertEqual(installed, imported - set(sys.stdlib_module_names))
        self.assertEqual(sorted(path.name for path in (demo / "seed").iterdir() if path.name != "__pycache__"), ["train.py"])


if __name__ == "__main__":
    unittest.main()
