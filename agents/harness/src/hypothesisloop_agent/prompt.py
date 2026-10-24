"""The system prompt as the harness completes it. The platform serves the prompt of the agent type
version an agent is pinned to with its own values filled in (instructions.py), and leaves the
placeholders only the machine has a value for: hl's listing of its commands, from the hl on this
PATH, and the directory the agent works in."""
from __future__ import annotations

import re
import subprocess

from hypothesisloop_agent import config

# Each role reads hl's listing, which holds the rules every command shares, and the help of the
# commands it acts with; it reads the help of any other when it runs one. A planner runs nothing,
# so the listing is not for it: it is shown its two commands.
_HL_HELP_EXECUTOR = ([], ["hypothesis", "submit"], ["job", "submit"])
_HL_HELP_RESEARCHER = ([], ["hypothesis"], ["hypothesis", "merge"], ["hypothesis", "retire"], ["hypothesis", "unlink"])
_HL_HELP_PLANNER = (["platform-experiments", "propose"], ["platform-experiments", "decline"])
_HL_HELP_BASELINE = ([], ["job", "submit"], ["job", "summary"], ["platform-experiments", "agents", "finish"])
_HL_HELP_COORDINATOR = ([], ["platform-experiments", "update"], ["platform-experiments", "summary"],
                        ["platform-experiments", "close"], ["platform-experiments", "agents", "add"],
                        ["hypothesis", "comment"])


def hl_commands(cfg: config.Config) -> str:
    """hl's own listing of its commands and of the subcommands the briefing of cfg's role leans on."""
    groups = _HL_HELP_EXECUTOR
    if config.coordinates(cfg):
        groups = _HL_HELP_COORDINATOR
    elif config.plans(cfg):
        groups = _HL_HELP_PLANNER
    elif config.measures_baseline(cfg):
        groups = _HL_HELP_BASELINE
    elif config.researches(cfg):
        groups = _HL_HELP_RESEARCHER
    # hl prints the help of a group on stdout and the help of one command on stderr.
    return "\n\n".join(
        subprocess.run(["hl", *group, "--help"], check=True, text=True,
                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT).stdout.strip()
        for group in groups)


def fill(served: str, placeholders: list[str], values: dict[str, str]) -> str:
    """served with each of placeholders replaced by its value in one pass, so that no value is
    expanded again. A placeholder the harness has no value for, or a {{ left in the prompt once
    they are taken out, ends the harness: a prompt the agent would read a placeholder in is
    never run."""
    unknown = [name for name in placeholders if name not in values]
    if unknown:
        raise SystemExit(f"agent: the platform's prompt leaves {', '.join(unknown)} to the harness, which fills only "
                         f"{', '.join(values)}")
    pieces = re.split(r"\{\{(" + "|".join(re.escape(name) for name in placeholders) + r")\}\}", served) if placeholders else [served]
    left = next((piece for piece in pieces[0::2] if "{{" in piece), None)
    if left is not None:
        at = left.index("{{")
        raise SystemExit(f"agent: the platform's prompt holds {left[at:at + 40]!r} after the harness filled "
                         f"{', '.join('{{' + name + '}}' for name in placeholders) or 'nothing'}")
    return "".join(piece if i % 2 == 0 else values[piece] for i, piece in enumerate(pieces))
