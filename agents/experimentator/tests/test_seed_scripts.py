import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

SEED = Path(__file__).resolve().parents[2] / "coordinator/experiments/dummy-data-search-demo/seed"


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
