from __future__ import annotations

import gzip
import hashlib
import io
import logging
import os
import tarfile
from pathlib import Path

from hypothesisloop_agent import api_client, config

log = logging.getLogger("llm_agent")

# domain.MaxAgentSessionBytes on the platform's side.
MAX_BYTES = 32 << 20

# The two roots a snapshot's members live under, by the name each has inside the archive.
_WORKDIR, _MODEL_HOME = "workdir", "model-home"


class Sessions:
    """Keeps this agent's model session with the platform, so that it outlives the machine.

    A snapshot is the files the backend names: its own state in the working directory and the
    model CLI's transcript under the CLI's home. It is stored after a turn when it changed and
    restored before the first turn on a machine that has none. The working tree is no part of
    it: the repository is cloned again and holds what the agent pushed.
    """

    def __init__(self, client: api_client.PlatformClient, cfg: config.Config, workdir: str, model_home: str):
        self._client = client
        self._path = (f"/platform-experiments/{cfg.platform_experiment_id}/coordinator/session" if config.coordinates(cfg)
                      else f"/platform-experiments/{cfg.platform_experiment_id}/agents/{cfg.agent_id}/session")
        self._roots = {_WORKDIR: Path(workdir), _MODEL_HOME: Path(model_home)}
        self._stored: str | None = None

    def restore(self) -> bool:
        """Puts the stored session's files in place. False when the platform holds none."""
        snapshot = self._client.get_bytes(self._path)
        if snapshot is None:
            return False
        with tarfile.open(fileobj=io.BytesIO(snapshot), mode="r:gz") as archive:
            for member in archive.getmembers():
                root, _, relative = member.name.partition("/")
                parts = Path(relative).parts
                if not member.isfile() or root not in self._roots or not parts or ".." in parts or Path(relative).is_absolute():
                    raise SystemExit(f"agent: the stored session holds {member.name!r}, which is no file of a session")
                target = self._roots[root] / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(archive.extractfile(member).read())
        self._stored = hashlib.sha256(snapshot).hexdigest()
        log.info("session restored from the platform (%d bytes)", len(snapshot))
        return True

    def save(self, workdir_files: list[str], model_home_files: list[str]) -> bool:
        """Stores the named files, relative to their roots, when they differ from what is stored.
        True when something was sent. A failed store is logged and tried again after the next
        turn: the agent's work does not wait for it."""
        snapshot = self._pack({_WORKDIR: workdir_files, _MODEL_HOME: model_home_files})
        digest = hashlib.sha256(snapshot).hexdigest()
        if digest == self._stored:
            return False
        if len(snapshot) > MAX_BYTES:
            log.error("session is %d bytes packed, above the %d the platform stores: a replacement machine would resume from the last one stored", len(snapshot), MAX_BYTES)
            return False
        try:
            self._client.put_bytes(self._path, snapshot)
        except api_client.APIError as e:
            log.warning("could not store the session (%s): trying again after the next turn", e)
            return False
        self._stored = digest
        return True

    def _pack(self, files: dict[str, list[str]]) -> bytes:
        # Same files, same bytes: no time and no owner goes into the archive, so that an
        # unchanged session is recognised by its digest.
        packed = io.BytesIO()
        with gzip.GzipFile(fileobj=packed, mode="wb", mtime=0) as zipped, tarfile.open(fileobj=zipped, mode="w") as archive:
            for root in sorted(files):
                for relative in sorted(set(files[root])):
                    path = self._roots[root] / relative
                    if not path.is_file():
                        continue
                    content = path.read_bytes()
                    info = tarfile.TarInfo(f"{root}/{relative}")
                    info.size = len(content)
                    archive.addfile(info, io.BytesIO(content))
        return packed.getvalue()


def model_home() -> str:
    return os.environ.get("CLAUDE_CONFIG_DIR") or os.path.expanduser("~/.claude")
