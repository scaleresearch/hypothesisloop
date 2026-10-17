"""The settings the local stack's control plane was started with, as make dev renders them, which
hl-admin and the browser run (controlplane/ui/e2e/settings.mjs) read too."""
from __future__ import annotations

import re
from pathlib import Path

SETTINGS = Path(__file__).resolve().parents[2] / "controlplane" / "settings" / ".hypothesisloop.dev.yaml"


def setting(key: str) -> str:
    match = re.search(rf"^\s+{re.escape(key)}:\s*(\S+)\s*$", SETTINGS.read_text(), re.MULTILINE)
    if not match:
        raise RuntimeError(f"{SETTINGS} names no {key}: render it with make dev")
    return match.group(1)
