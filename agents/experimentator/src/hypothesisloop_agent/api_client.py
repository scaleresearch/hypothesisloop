"""Thin stdlib-only HTTP client for the few platform calls the outer loop makes itself: joining
its platform experiment before the first turn, reading the rules and checking whether it has closed. Everything else
the agent does itself through `hl`, so this client stays minimal.
"""
from __future__ import annotations

import json
import urllib.error
import urllib.request
from typing import Any


class APIError(Exception):
    def __init__(self, method: str, url: str, status: int, body: str):
        super().__init__(f"{method} {url} -> HTTP {status}: {body[:500]}")
        self.status = status
        self.body = body


def _request(method: str, url: str, payload: dict | None = None, timeout: float = 15.0) -> Any:
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(url, data=data, method=method,
                                  headers={"Content-Type": "application/json"} if data else {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read()
            return json.loads(body) if body else None
    except urllib.error.HTTPError as e:
        raise APIError(method, url, e.code, e.read().decode(errors="replace")) from None
    except urllib.error.URLError as e:
        raise APIError(method, url, 0, str(e.reason)) from None


class PlatformClient:
    def __init__(self, api_url: str):
        self.api_url = api_url.rstrip("/")

    def join(self, agent_id: str, pe_id: str) -> None:
        """Registers the agent and signs it up, safe to repeat on restart: a repeat register is a
        409 and a repeat signup succeeds."""
        try:
            _request("POST", f"{self.api_url}/agents", {"id": agent_id, "kind": "agent"})
        except APIError as e:
            if e.status != 409:
                raise
        _request("POST", f"{self.api_url}/platform-experiments/{pe_id}/signup", {"agent_id": agent_id})

    def get_platform_experiment(self, pe_id: str) -> dict:
        return _request("GET", f"{self.api_url}/platform-experiments/{pe_id}")

    def fetch_rules(self) -> str:
        """The platform rules, which the system prompt carries verbatim. A failure raises: an
        agent briefed without the rules would compete blind."""
        url = f"{self.api_url}/explore/rules"
        try:
            with urllib.request.urlopen(url, timeout=15.0) as resp:
                return resp.read().decode(errors="replace")
        except urllib.error.HTTPError as e:
            raise APIError("GET", url, e.code, e.read().decode(errors="replace")) from None
        except urllib.error.URLError as e:
            raise APIError("GET", url, 0, str(e.reason)) from None
