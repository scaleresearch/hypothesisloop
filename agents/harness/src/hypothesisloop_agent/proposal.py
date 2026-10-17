"""A proposal as the platform stores it, written as the YAML `hl platform-experiments propose`
reads: read back, the file is the stored value. A single word stays bare and other text goes in a
| block wherever a block holds it exactly."""
from __future__ import annotations

import json
import re

_WORD = re.compile(r"[A-Za-z_][A-Za-z0-9_.=/-]*")
_RESOLVED = ("true", "false", "null", "yes", "no", "on", "off", "y", "n")


def _quoted(text: str) -> str:
    return '"' + "".join(c if c.isprintable() and c not in '"\\' else f"\\U{ord(c):08X}" for c in text) + '"'


def _bare(text: str) -> bool:
    return bool(_WORD.fullmatch(text)) and text.lower() not in _RESOLVED


def _key(key: str) -> str:
    return key if _bare(key) else _quoted(key)


def _entry(head: str, value, indent: str) -> list[str]:
    inner = indent + "  "
    if isinstance(value, str) and _bare(value):
        return [f"{indent}{head} {value}"]
    if isinstance(value, str):
        body = value.rstrip("\n")
        if not body or body != body.strip() or not all(line.isprintable() for line in body.split("\n")):
            return [f"{indent}{head} {_quoted(value)}"]
        newlines = len(value) - len(body)
        return [f"{indent}{head} |{('-', '', '+')[min(newlines, 2)]}",
                *(inner + line if line else "" for line in body.split("\n")),
                *[""] * (newlines - 1)]
    if isinstance(value, dict) and value:
        return [f"{indent}{head}", *(line for key, item in value.items() for line in _entry(_key(key) + ":", item, inner))]
    if isinstance(value, list) and value:
        return [f"{indent}{head}", *(line for item in value for line in _entry("-", item, inner))]
    return [f"{indent}{head} {json.dumps(value)}"]


def render(proposal: dict) -> str:
    return "\n".join(line for key, value in proposal.items() for line in _entry(_key(key) + ":", value, "")) + "\n"
