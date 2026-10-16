"""Thin stdlib-only HTTP client for the few platform calls the outer loop makes itself: checking
before the first turn that it was added to its platform experiment, reading its instructions and
checking whether the experiment has closed. Everything else the agent does itself through `hl`, so this
client stays minimal.
"""
from __future__ import annotations

import http.client
import json
import time
import urllib.error
import urllib.request
from typing import Any


class APIError(Exception):
    """status 0 when no HTTP answer came back: unreachable, reset or timed out."""

    def __init__(self, method: str, url: str, status: int, body: str, attempts: int = 1):
        tried = f" (gave up after {attempts} attempts)" if attempts > 1 else ""
        super().__init__(f"{method} {url} -> HTTP {status}: {body[:500]}{tried}")
        self.status = status
        self.body = body


# A read through the public tunnel timed out once and the harness died of it. Only a GET is asked
# again: a write that timed out may have landed.
_TRANSIENT_STATUSES = (0, 502, 503, 504)
_GET_ATTEMPTS = 5
_BACKOFF_S = 1.0


def _send(method: str, url: str, token: str, payload: dict | None = None, timeout: float = 15.0,
          content: bytes | None = None) -> bytes:
    """One request, asked again with backoff while a GET fails transiently. payload is sent as
    JSON, content as the bytes they are."""
    attempts = _GET_ATTEMPTS if method == "GET" else 1
    attempt = 1
    while True:
        try:
            return _send_once(method, url, token, payload, timeout, content)
        except APIError as e:
            if e.status not in _TRANSIENT_STATUSES or attempt == attempts:
                if attempt == 1:
                    raise
                raise APIError(method, url, e.status, e.body, attempt) from None
        time.sleep(_BACKOFF_S * 2 ** (attempt - 1))
        attempt += 1


def _send_once(method: str, url: str, token: str, payload: dict | None, timeout: float,
               content: bytes | None) -> bytes:
    data = json.dumps(payload).encode() if payload is not None else content
    headers = {"Authorization": f"Bearer {token}"}
    if data:
        headers["Content-Type"] = "application/json" if payload is not None else "application/octet-stream"
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.read()
    except urllib.error.HTTPError as e:
        raise APIError(method, url, e.code, e.read().decode(errors="replace")) from None
    except urllib.error.URLError as e:
        raise APIError(method, url, 0, str(e.reason)) from None
    # A timeout or reset while the answer is read is no URLError.
    except (OSError, http.client.HTTPException) as e:
        raise APIError(method, url, 0, f"{type(e).__name__}: {e}") from None


# A session snapshot is megabytes, on whatever line the machine has.
_BYTES_TIMEOUT_S = 120.0


class PlatformClient:
    def __init__(self, api_url: str, token: str):
        self.api_url = api_url.rstrip("/")
        self.token = token

    def _request(self, method: str, path: str, payload: dict | None = None) -> Any:
        body = _send(method, f"{self.api_url}{path}", self.token, payload)
        return json.loads(body) if body else None

    def require_joined(self, agent_id: str, pe_id: str) -> dict:
        """The agent's signup, which says what it runs as, or an exit unless the platform has
        this agent in this platform experiment and accepts its token. An agent never joins by
        itself: whoever runs the experiment adds it, and that is where its HL_TOKEN comes from."""
        try:
            return self._request("GET", f"/platform-experiments/{pe_id}/signups/{agent_id}")
        except APIError as e:
            if e.status not in (401, 403, 404):
                raise
            raise SystemExit(
                f"agent: {agent_id} is not in platform experiment {pe_id}, or HL_TOKEN is not its token "
                f"(HTTP {e.status}). Add the agent with `hl platform-experiments agents add --platform-experiment {pe_id}` and "
                "start it with the HL_TOKEN that prints.") from None

    def require_coordinated(self, pe_id: str) -> dict:
        """The platform experiment, or an exit unless HL_TOKEN is its coordinator's token: an
        agent's token reads it as well and can change nothing a coordinator is there to change."""
        try:
            self._request("GET", f"/platform-experiments/{pe_id}/agents")
            return self.get_platform_experiment(pe_id)
        except APIError as e:
            if e.status not in (401, 403, 404):
                raise
            raise SystemExit(
                f"agent: HL_TOKEN does not coordinate platform experiment {pe_id} (HTTP {e.status}). A "
                "coordinator the platform runs is asked for with coordinator_hosted when the platform "
                "experiment is created.") from None

    def get_bytes(self, path: str) -> bytes | None:
        """The stored bytes at path, or None when the platform holds none (404)."""
        try:
            return _send("GET", f"{self.api_url}{path}", self.token, timeout=_BYTES_TIMEOUT_S)
        except APIError as e:
            if e.status == 404:
                return None
            raise

    def put_bytes(self, path: str, content: bytes) -> None:
        _send("PUT", f"{self.api_url}{path}", self.token, content=content, timeout=_BYTES_TIMEOUT_S)

    def get_signup(self, agent_id: str, pe_id: str) -> dict:
        return self._request("GET", f"/platform-experiments/{pe_id}/signups/{agent_id}")

    def get_platform_experiment(self, pe_id: str) -> dict:
        return self._request("GET", f"/platform-experiments/{pe_id}")

    def get_json(self, path: str) -> Any:
        return self._request("GET", path)
