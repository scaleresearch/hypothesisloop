"""What the real-provider test asks the vendors themselves: what is rented right now, and the two
faults it injects, a sandbox the vendor loses and a harness that hangs. The platform's own code
names no vendor outside its adapters; a test that checks the bill has to.

Every key comes from the environment and is never part of a URL, an argument or a message.
"""
from __future__ import annotations

import base64
import json
import os
import struct

import requests

TIMEOUT = 30


def _fly_headers() -> dict[str, str]:
    token = os.environ["SANDBOX_FLY_API_TOKEN"]
    return {"Authorization": token if " " in token else f"Bearer {token}"}


def _fly_machines() -> list[dict]:
    r = requests.get(f"https://api.machines.dev/v1/apps/{os.environ['SANDBOX_FLY_APP']}/machines", headers=_fly_headers(), timeout=TIMEOUT)
    r.raise_for_status()
    return r.json()


def _e2b_headers() -> dict[str, str]:
    return {"X-API-Key": os.environ["SANDBOX_E2B_API_KEY"]}


def _e2b_sandboxes() -> list[dict]:
    r = requests.get("https://api.e2b.app/v2/sandboxes", headers=_e2b_headers(), timeout=TIMEOUT)
    r.raise_for_status()
    return r.json()


def sandboxes(provider: str) -> dict[str, str]:
    """Every sandbox the vendor holds for the account, as {instance name: vendor id}."""
    if provider == "fly":
        return {(m.get("config") or {}).get("metadata", {}).get("hl_name", m["name"]): m["id"] for m in _fly_machines()}
    if provider == "e2b":
        return {(s.get("metadata") or {}).get("hl_name", s["sandboxID"]): s["sandboxID"] for s in _e2b_sandboxes()}
    raise ValueError(f"no sandbox provider {provider!r}")


def lose_sandbox(provider: str, vendor_id: str) -> None:
    """Destroys a sandbox behind the platform's back, as a vendor that lost the machine would."""
    if provider == "fly":
        r = requests.delete(f"https://api.machines.dev/v1/apps/{os.environ['SANDBOX_FLY_APP']}/machines/{vendor_id}",
                            params={"force": "true"}, headers=_fly_headers(), timeout=TIMEOUT)
    else:
        r = requests.delete(f"https://api.e2b.app/sandboxes/{vendor_id}", headers=_e2b_headers(), timeout=TIMEOUT)
    r.raise_for_status()


# Stops every process of the harness where it stands and leaves the machine running: the vendor
# goes on listing it as started, and only the platform's own health rule can tell.
_FREEZE = ("for p in /proc/[0-9]*; do if tr '\\0' ' ' < $p/cmdline 2>/dev/null | grep -q '^python3 -m hypothesisloop_agent'; "
           "then kill -STOP ${p#/proc/} && echo frozen ${p#/proc/}; fi; done")


def _e2b_run(sandbox_id: str, script: str) -> str:
    """Runs a shell script inside a sandbox through the vendor's process service there, which
    speaks the Connect protocol: envelopes of a flag byte, a length and a JSON message."""
    shown = requests.get(f"https://api.e2b.app/sandboxes/{sandbox_id}", headers=_e2b_headers(), timeout=TIMEOUT)
    shown.raise_for_status()
    message = json.dumps({"process": {"cmd": "sh", "args": ["-c", script]}, "stdin": False}).encode()
    r = requests.post(f"https://49983-{sandbox_id}.e2b.app/process.Process/Start", data=struct.pack(">BI", 0, len(message)) + message,
                      headers={"X-Access-Token": shown.json()["envdAccessToken"], "Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"},
                      timeout=60)
    r.raise_for_status()
    out, rest = b"", r.content
    while len(rest) >= 5:
        flags, size = struct.unpack(">BI", rest[:5])
        event, rest = json.loads(rest[5:5 + size] or b"{}"), rest[5 + size:]
        if flags & 0x02:
            assert not event.get("error"), f"the process service refused the command: {event}"
            break
        data = (event.get("event") or {}).get("data") or {}
        out += base64.b64decode(data.get("stdout") or "")
    return out.decode()


def freeze_harness(provider: str, vendor_id: str) -> str:
    """Freezes the agent harness inside a sandbox, by the vendor's own way of running a command
    in one."""
    if provider == "e2b":
        out = _e2b_run(vendor_id, _FREEZE)
        assert "frozen" in out, f"no harness process was found to freeze: {out!r}"
        return out.strip()
    r = requests.post(f"https://api.machines.dev/v1/apps/{os.environ['SANDBOX_FLY_APP']}/machines/{vendor_id}/exec",
                      json={"command": ["sh", "-c", _FREEZE], "timeout": 20}, headers=_fly_headers(), timeout=60)
    r.raise_for_status()
    out = r.json().get("stdout") or ""
    assert "frozen" in out, f"no harness process was found to freeze: {r.json()}"
    return out.strip()


def rented_instances() -> dict[str, str]:
    """Every instance rented for jobs, as {label: status}."""
    r = requests.get("https://console.vast.ai/api/v0/instances/", headers={"Authorization": f"Bearer {os.environ['CLOUD_COMPUTE_VAST_API_KEY']}"}, timeout=TIMEOUT)
    r.raise_for_status()
    return {str(i.get("label") or i["id"]): str(i.get("actual_status")) for i in r.json().get("instances", [])}


def job_credit() -> float:
    """What is left on the account that rents for jobs, in dollars."""
    r = requests.get("https://console.vast.ai/api/v0/users/current/", headers={"Authorization": f"Bearer {os.environ['CLOUD_COMPUTE_VAST_API_KEY']}"}, timeout=TIMEOUT)
    r.raise_for_status()
    return float(r.json()["credit"])
