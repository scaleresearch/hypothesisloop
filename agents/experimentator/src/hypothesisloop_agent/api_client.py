"""Thin stdlib-only HTTP client for the one platform call the outer loop makes itself: checking
whether its platform experiment has closed. Everything else the agent does it does itself, by
curling the platform services from its harness's built-in shell tool, so this client stays
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
    def __init__(self, api_url: str):
        self.api_url = api_url.rstrip("/")

    def get_platform_experiment(self, pe_id: str) -> dict:
        return _request("GET", f"{self.api_url}/platform-experiments/{pe_id}")

    def fetch_api_guide(self) -> str:
        """The API's live /explore digest, generated from its registered operations so it can
        never drift. It also carries the platform rules, which the system prompt treats as
        binding rather than restating — so a failure to fetch raises instead of embedding a
        note: an agent briefed without the rules would compete blind.
        """
        parts: list[str] = []
        for base in (self.api_url,):
            url = f"{base}/explore"
            try:
                req = urllib.request.Request(url, method="GET")
                with urllib.request.urlopen(req, timeout=15.0) as resp:
                    parts.append(resp.read().decode(errors="replace"))
            except urllib.error.HTTPError as e:
                raise APIError("GET", url, e.code, e.read().decode(errors="replace")) from None
            except urllib.error.URLError as e:
                raise APIError("GET", url, 0, str(e.reason)) from None
        return "\n\n".join(parts)
