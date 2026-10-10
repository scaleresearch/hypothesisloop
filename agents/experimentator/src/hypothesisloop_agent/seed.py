"""What a planner reads first in every plan, read by the harness and handed over in its first
prompt: the draft, the capacity, and the repository's tree, README and .hypothesisloop/. And what
a baseline agent is told first: how soon its work is due.

A planner took 3m41s to propose, and its first turns were these reads, each a model call
over a growing context. Every step that fails is said in the prompt, and the planner does it
itself.
"""
from __future__ import annotations

import json
import logging
import os
import subprocess
from pathlib import Path

log = logging.getLogger("llm_agent")

TREE_LINES = 300
FILE_CHARS = 12_000
_TIMEOUT_S = 120

# A planner read /home/user/<repo>/... and ran `find /` before it used its working
# directory: nothing said where that was.
_OPENING = (
    "Your working directory is {workdir}, the root of the repository's clone: every path below is "
    "relative to it, and a file tool takes {workdir}/<path>. "
    "Read for you before this turn, as you would have read them yourself: the draft, the capacity, "
    "and the repository, cloned into {workdir} (its default branch, depth 1) unless said below, "
    "with its file list, README and .hypothesisloop/. Start from them, and read again only what a "
    "revision changes or a file not shown here.")


def _run(command: list[str], cwd: str | None = None) -> str:
    done = subprocess.run(command, cwd=cwd, text=True, capture_output=True, timeout=_TIMEOUT_S)
    if done.returncode != 0:
        raise RuntimeError(f"`{' '.join(command)}` exited {done.returncode}: {(done.stderr or done.stdout).strip()[-500:]}")
    return done.stdout


def _bounded(text: str, limit: int) -> str:
    text = text.strip()
    return text if len(text) <= limit else text[:limit] + f"\n[... cut at {limit} characters]"


def _section(title: str, body: str) -> str:
    return f"=== {title}\n{body.strip()}"


def _clone(url: str, workdir: str) -> None:
    if any(Path(workdir).iterdir()):
        return
    _run(["git", "clone", "--depth", "1", url, "."], cwd=workdir)


def _files(workdir: str) -> list[str]:
    return _run(["git", "ls-files"], cwd=workdir).splitlines()


def _readme(workdir: str, files: list[str]) -> str | None:
    top = sorted(f for f in files if "/" not in f and f.lower().startswith("readme"))
    return top[0] if top else None


def planner(workdir: str) -> str:
    """The opening reads of a planner whose session starts fresh, in workdir, as one prompt."""
    workdir = os.path.abspath(workdir)
    sections, failed = [], []
    draft = ""
    for title, command in (("hl platform-experiments get", ["hl", "platform-experiments", "get"]),
                           ("hl capacity", ["hl", "capacity"])):
        try:
            output = _run(command)
        except (OSError, RuntimeError, subprocess.TimeoutExpired) as e:
            failed.append(str(e))
            continue
        sections.append(_section(title, output))
        if command[1] == "platform-experiments":
            draft = output
    try:
        url = json.loads(draft)["repository_url"] if draft else ""
        if not url:
            raise RuntimeError("the draft names no repository_url")
        _clone(url, workdir)
        files = _files(workdir)
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.TimeoutExpired) as e:
        failed.append(f"the repository was not read: {e}")
        files = []
    if files:
        listing = files[:TREE_LINES]
        more = f"\n[... {len(files) - TREE_LINES} more files]" if len(files) > TREE_LINES else ""
        sections.append(_section(f"git ls-files ({len(files)} files) in {workdir}, your working directory", "\n".join(listing) + more))
        readme = _readme(workdir, files)
        for path in ([readme] if readme else []) + sorted(f for f in files if f.startswith(".hypothesisloop/")):
            try:
                text = (Path(workdir) / path).read_text(errors="replace")
            except OSError as e:
                failed.append(f"{path} was not read: {e}")
                continue
            sections.append(_section(path, _bounded(text, FILE_CHARS)))
        if not any(f.startswith(".hypothesisloop/") for f in files):
            sections.append(_section(".hypothesisloop/", "The repository has no .hypothesisloop/ directory."))
    if failed:
        log.warning("planner opening reads failed: %s", "; ".join(failed))
        sections.append(_section("Not read, do it yourself", "\n".join(failed)))
    return "\n\n".join([_OPENING.format(workdir=workdir), *sections])


# Baseline agents built the environment 9.5 to 14.5 min in, and the control
# waited for it. The opening says the run waits on the goal and what usually takes longest.
_BASELINE_OPENING = (
    "Your goal is the control and each reference measured and your branch ready for researchers, "
    "who wait for it to start. Start what is slowest first, often the "
    "environment build from the toolchain and dependency pins {source}, and do the rest alongside "
    "it, with subagents where parallel work is faster.")
_FROM_MAP = "the planner's map below names"
_FROM_REPOSITORY = "the repository's pin files state"


def baseline(repo_map: str) -> str:
    """The opening of a baseline agent whose session starts fresh: its goal, and the planner's map
    of the repository where it left one."""
    opening = _BASELINE_OPENING.format(source=_FROM_MAP if repo_map else _FROM_REPOSITORY)
    if not repo_map:
        return opening
    return "\n\n".join([opening, _section("The planner's map of the repository", repo_map)])
