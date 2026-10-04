from __future__ import annotations

import gzip
import hashlib
import io
import logging
import tarfile
from pathlib import Path

from hypothesisloop_agent import api_client, config

log = logging.getLogger("llm_agent")

# domain.MaxAgentSessionBytes on the platform's side.
MAX_BYTES = 32 << 20

# The two roots a snapshot's members live under, by the name each has inside the archive. The
# harness's state is named workdir there: sessions stored while it was kept in the working
# directory are restored under that name.
_STATE, _MODEL_HOME = "workdir", "model-home"


class Sessions:
    """Keeps this agent's model session with the platform, so that it outlives the machine.

    A snapshot is the files the run loop names: its own state in its state directory and the
    model CLI's transcript under the CLI's home. It is stored after a turn when it changed and
    restored before the first turn on a machine that has none. The working tree is no part of
    it: the repository, or with none the platform, holds what the agent pushed and is read again.
    """

    def __init__(self, client: api_client.PlatformClient, cfg: config.Config, statedir: str, model_home: str):
        self._client = client
        self._path = (f"/platform-experiments/{cfg.platform_experiment_id}/coordinator/session" if config.coordinates(cfg)
                      else f"/platform-experiments/{cfg.platform_experiment_id}/agents/{cfg.agent_id}/session")
        self._roots = {_STATE: Path(statedir), _MODEL_HOME: Path(model_home)}
        self._stored: str | None = None

    def restore(self) -> bool:
        """Puts the stored session's files in place. False when the platform holds none."""
        snapshot = self._read()
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

    def _read(self) -> bytes | None:
        # Starting without a session that exists would throw the agent's work so far away, so a
        # read that fails, after the client has asked again, ends the harness.
        try:
            return self._client.get_bytes(self._path)
        except api_client.APIError as e:
            raise SystemExit(f"agent: could not read the stored session: {e}") from None

    def save(self, state_files: list[str], model_home_files: list[str]) -> bool:
        """Stores the named files, relative to their roots, when they differ from what is stored.
        True when something was sent. Raises NotStored for a session the platform does not take
        or could not be sent."""
        snapshot = self._pack({_STATE: state_files, _MODEL_HOME: model_home_files})
        digest = hashlib.sha256(snapshot).hexdigest()
        if digest == self._stored:
            return False
        if len(snapshot) > MAX_BYTES:
            raise NotStored(f"it is {len(snapshot) / (1 << 20):.1f} MiB packed and the platform stores {MAX_BYTES >> 20} MiB")
        try:
            self._client.put_bytes(self._path, snapshot)
        except api_client.APIError as e:
            log.warning("could not store the session (%s)", e)
            raise NotStored(f"the platform answered HTTP {e.status}" if e.status else "the platform could not be reached") from None
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


class NotStored(Exception):
    """Why a session was not stored, in words that hold nothing secret. A replacement machine
    would resume from the last one stored."""
