import re
import unittest
from pathlib import Path

DOCKERFILE = (Path(__file__).resolve().parents[1] / "Dockerfile.base").read_text()


class RegistryLoginTest(unittest.TestCase):
    """The agent pushes images to the platform's registry with no login of its own: podman, run as
    root through sudo, asks hl as its credential helper, which fetches a fresh push login with the
    sandbox's token on every push (cli/internal/agent/registry.go)."""

    def test_podman_asks_hl_after_its_own_logins(self):
        self.assertIn('credential-helpers = ["containers-auth.json", "hl"]', DOCKERFILE)

    def test_hl_answers_as_docker_credential_hl(self):
        self.assertRegex(DOCKERFILE, r"ln -s hl /usr/local/bin/docker-credential-hl\b")

    def test_sudo_keeps_what_hl_reaches_the_platform_with(self):
        kept = re.search(r'Defaults env_keep \+= "([^"]*)"', DOCKERFILE)
        self.assertIsNotNone(kept)
        self.assertEqual(set(kept.group(1).split()), {"API_URL", "HL_TOKEN"})


if __name__ == "__main__":
    unittest.main()
