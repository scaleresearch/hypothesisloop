"""hl-admin, the operator's hand on tenancy: registers a person as their first sign-in would,
hands out their first API key, registers clusters, decides who is staff and which clusters are
shared, and mints derived tokens. It reads HYPOTHESISLOOP_CONFIG and
DATABASE_URL from the environment, which the make e2e targets set for the local stack.
"""
from __future__ import annotations

import json
import subprocess
from functools import cache
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
HL_ADMIN_BIN = REPO_ROOT / "bin" / "hl-admin"


@cache
def _binary() -> Path:
    HL_ADMIN_BIN.parent.mkdir(parents=True, exist_ok=True)
    subprocess.run(["go", "build", "-o", str(HL_ADMIN_BIN), "./controlplane/cmd/hl-admin"], cwd=REPO_ROOT, check=True)
    return HL_ADMIN_BIN


def _run(*args: str) -> str:
    proc = subprocess.run([str(_binary()), *args], capture_output=True, text=True, timeout=60)
    if proc.returncode != 0:
        raise RuntimeError(f"hl-admin {' '.join(args)} exited {proc.returncode}: {proc.stderr}")
    return proc.stdout.strip()


def token(scope: str) -> str:
    return _run("token", scope)


def add_user(subject: str, name: str) -> dict:
    """{user_id, org_id, project_id}: the user, their home org and its default project."""
    return json.loads(_run("user", "add", subject, name))


def add_api_key(subject: str, org_id: str, project_id: str = "") -> str:
    args = ["api-key", "add", subject, org_id] + ([project_id] if project_id else [])
    return json.loads(_run(*args))["token"]


def add_cluster(name: str, org_id: str) -> str:
    """Registers the cluster for the org and returns its runtime token."""
    return _run("cluster", "add", name, org_id)


def set_staff(subject: str, staff: bool) -> None:
    _run("user", "staff", subject, "on" if staff else "off")


def share_cluster(name: str, shared: bool) -> None:
    """Places every org's jobs on the cluster, or only its own org's."""
    _run("cluster", "share", name, "on" if shared else "off")
