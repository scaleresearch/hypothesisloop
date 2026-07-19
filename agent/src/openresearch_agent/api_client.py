"""Thin stdlib-only HTTP client for the one platform call the outer loop makes itself: checking
whether its platform experiment has closed. Everything else the agent does goes through the
`call_api`/`run_bash` tools (see tools.py) — the model drives those itself, so this client stays
intentionally minimal rather than growing a method per endpoint.
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
    def __init__(self, quota_url: str, sched_url: str, registry_url: str):
        self.quota_url = quota_url.rstrip("/")
        self.sched_url = sched_url.rstrip("/")
        self.registry_url = registry_url.rstrip("/")

    def get_platform_experiment(self, pe_id: str) -> dict:
        return _request("GET", f"{self.quota_url}/platform-experiments/{pe_id}")

    def fetch_api_guide(self) -> str:
        """Fetch the live, self-documenting compact API reference from each service's
        /explore endpoint and concatenate it. This replaces the old hand-maintained
        docs/API.md: the contract is generated from the running services' Huma-registered
        operations, so it never drifts. The quota-service digest also carries the
        cross-cutting platform rules. Each service additionally serves the full
        OpenAPI 3.1 spec at /openapi.json.
        """
        parts: list[str] = []
        for base in (self.quota_url, self.sched_url, self.registry_url):
            url = f"{base}/explore"
            try:
                req = urllib.request.Request(url, method="GET")
                with urllib.request.urlopen(req, timeout=15.0) as resp:
                    parts.append(resp.read().decode(errors="replace"))
            except (urllib.error.HTTPError, urllib.error.URLError) as e:
                parts.append(f"# (could not fetch {url}: {e})\n")
        return "\n\n".join(parts)
