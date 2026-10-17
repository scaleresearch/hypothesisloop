"""The few platform calls the harness makes itself: checking before the first turn that it was
added to its platform experiment, reading its instructions, keeping its session and checking
whether its run goes on. Everything else the agent does itself through `hl`. Each call is made
once: what fails is raised to the caller, which decides what a failure means.
"""
from __future__ import annotations

import json
from typing import Any

import httpx


class APIError(Exception):
    """status 0 when no HTTP answer came back: unreachable, reset or timed out."""

    def __init__(self, method: str, path: str, status: int, body: str):
        super().__init__(f"{method} {path} -> HTTP {status}: {body[:500]}")
        self.status = status
        self.body = body


_TIMEOUT_S = 15.0
# A session snapshot is megabytes, on whatever line the machine has.
_BYTES_TIMEOUT_S = 120.0


class PlatformClient:
    def __init__(self, api_url: str, token: str):
        self._http = httpx.AsyncClient(base_url=api_url.rstrip("/"), headers={"Authorization": f"Bearer {token}"})

    async def aclose(self) -> None:
        await self._http.aclose()

    async def _send(self, method: str, path: str, content: bytes | None = None, timeout: float = _TIMEOUT_S) -> bytes:
        headers = {"Content-Type": "application/octet-stream"} if content is not None else {}
        try:
            response = await self._http.request(method, path, content=content, headers=headers, timeout=timeout)
        except httpx.HTTPError as e:
            raise APIError(method, path, 0, f"{type(e).__name__}: {e}") from e
        if response.is_error:
            raise APIError(method, path, response.status_code, response.text)
        return response.content

    async def get_json(self, path: str) -> Any:
        body = await self._send("GET", path)
        return json.loads(body) if body else None

    async def require_joined(self, agent_id: str, pe_id: str) -> dict:
        """The agent's signup, which says what it runs as, or an exit unless the platform has
        this agent in this platform experiment and accepts its token. An agent never joins by
        itself: whoever runs the experiment adds it, and that is where its HL_TOKEN comes from."""
        try:
            return await self.get_signup(agent_id, pe_id)
        except APIError as e:
            if e.status not in (401, 403, 404):
                raise
            raise SystemExit(
                f"agent: {agent_id} is not in platform experiment {pe_id}, or HL_TOKEN is not its token "
                f"(HTTP {e.status}). Add the agent with `hl platform-experiments agents add --platform-experiment {pe_id}` and "
                "start it with the HL_TOKEN that prints.") from None

    async def require_coordinated(self, pe_id: str) -> dict:
        """The platform experiment, or an exit unless HL_TOKEN is its coordinator's token: an
        agent's token reads it as well and can change nothing a coordinator is there to change."""
        try:
            await self.get_json(f"/platform-experiments/{pe_id}/agents")
            return await self.get_platform_experiment(pe_id)
        except APIError as e:
            if e.status not in (401, 403, 404):
                raise
            raise SystemExit(
                f"agent: HL_TOKEN does not coordinate platform experiment {pe_id} (HTTP {e.status}). A "
                "coordinator the platform runs is asked for with coordinator_in_agentsandbox when the platform "
                "experiment is created.") from None

    async def get_bytes(self, path: str) -> bytes | None:
        """The stored bytes at path, or None when the platform holds none (404)."""
        try:
            return await self._send("GET", path, timeout=_BYTES_TIMEOUT_S)
        except APIError as e:
            if e.status == 404:
                return None
            raise

    async def put_bytes(self, path: str, content: bytes) -> None:
        await self._send("PUT", path, content=content, timeout=_BYTES_TIMEOUT_S)

    async def get_signup(self, agent_id: str, pe_id: str) -> dict:
        return await self.get_json(f"/platform-experiments/{pe_id}/signups/{agent_id}")

    async def get_platform_experiment(self, pe_id: str) -> dict:
        return await self.get_json(f"/platform-experiments/{pe_id}")
