"""Loads KEY=VALUE lines from a .env file into os.environ (only if not already set — real env
vars always win). No `python-dotenv` dependency: this is 10 lines, not worth a package."""
from __future__ import annotations

import os


def load(path: str = ".env") -> None:
    if not os.path.exists(path):
        return
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, _, value = line.partition("=")
            key = key.strip()
            if key and key not in os.environ:
                os.environ[key] = value.strip()
