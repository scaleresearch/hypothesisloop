"""The model CLI's own record of a session, its transcript, under the CLI's home."""
from __future__ import annotations

import json
import os
import shutil
from pathlib import Path

from hypothesisloop_agent import config


def model_home() -> str:
    return config.required("CLAUDE_CONFIG_DIR", "the directory the model CLI keeps its sessions in")


def _transcripts(session_id: str) -> list[Path]:
    return list(Path(model_home()).glob(f"projects/*/{session_id}.jsonl"))


def files(session_id: str | None) -> list[str]:
    """The model CLI's own record of the session, relative to its home: the transcript and
    whatever it keeps in a directory of the session's name beside it, but its subagents'
    transcripts. A resumed session stops every subagent, the lead's transcript holds what each
    reported, and a run's worth of them would rotate the session, which stops those at work."""
    if not session_id:
        return []
    home = Path(model_home())
    found = []
    for transcript in _transcripts(session_id):
        found.append(transcript)
        kept = transcript.with_suffix("")
        found.extend(path for path in kept.rglob("*") if path.is_file() and path.relative_to(kept).parts[0] != "subagents")
    return [str(path.relative_to(home)) for path in found]


def size(session_id: str | None) -> int:
    """The bytes of the session's record, as files() names it."""
    home = Path(model_home())
    return sum((home / name).stat().st_size for name in files(session_id))


def delete(session_id: str) -> None:
    for transcript in _transcripts(session_id):
        shutil.rmtree(transcript.with_suffix(""), ignore_errors=True)
        transcript.unlink(missing_ok=True)


# The CLI records the system prompt a session runs with in its transcript, as attachments of this
# type, and a resumed session runs with the recorded one whatever prompt it is started with. A
# session resumed without them records and runs with the prompt it is started with, its whole
# history kept, signed thinking and tool calls included (Claude Code 2.1.294, on the real API).
_PROMPT_SNAPSHOT = "prompt_snapshot"
# The fields of a transcript line that name another line by its uuid.
_LINE_LINKS = ("parentUuid", "logicalParentUuid", "leafUuid")


def _lines(session_id: str) -> tuple[Path, list[dict]]:
    """The session's transcript and its lines, parsed."""
    transcripts = _transcripts(session_id)
    if len(transcripts) != 1:
        raise FileNotFoundError(f"session {session_id} has {len(transcripts)} transcripts")
    transcript = transcripts[0]
    return transcript, [json.loads(line) for line in transcript.read_text().splitlines() if line.strip()]


def _is_prompt_snapshot(line: dict) -> bool:
    attachment = line.get("attachment")
    return line.get("type") == "attachment" and isinstance(attachment, dict) and attachment.get("type") == _PROMPT_SNAPSHOT


def records_prompt(session_id: str | None) -> bool:
    """Whether the session's transcript records the system prompt it runs with, which is what
    restarting it with another prompt replaces."""
    if not session_id:
        return False
    try:
        _, lines = _lines(session_id)
    except FileNotFoundError:
        return False
    return any(_is_prompt_snapshot(line) for line in lines)


def drop_recorded_prompt(session_id: str) -> None:
    """Removes the system prompt the session's transcript records, for the session to be resumed
    with another. The lines that follow a removed one are linked to the line it followed."""
    transcript, lines = _lines(session_id)
    dropped = {line["uuid"]: line.get("parentUuid") for line in lines if _is_prompt_snapshot(line)}
    kept = [line for line in lines if not _is_prompt_snapshot(line)]
    for line in kept:
        for link in _LINE_LINKS:
            while line.get(link) in dropped:
                line[link] = dropped[line[link]]
    tmp = transcript.with_suffix(".jsonl.tmp")
    tmp.write_text("".join(json.dumps(line, ensure_ascii=False) + "\n" for line in kept))
    os.replace(tmp, transcript)


def lead_texts(session_id: str | None) -> list[str]:
    """What the agent itself wrote in the session, oldest first: its subagents' words left out,
    and a line that cannot be read skipped."""
    texts: list[str] = []
    for transcript in _transcripts(session_id) if session_id else ():
        for line in transcript.read_text(errors="replace").splitlines():
            try:
                record = json.loads(line)
                if record.get("type") != "assistant" or record.get("isSidechain"):
                    continue
                texts += [block["text"] for block in record["message"]["content"]
                          if isinstance(block, dict) and block.get("type") == "text" and block.get("text", "").strip()]
            except (ValueError, KeyError, TypeError, AttributeError):
                continue
    return texts
