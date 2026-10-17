"""What a turn's result says: the context its last model call carried, and refusals that asking
again cannot change."""
from __future__ import annotations

from claude_agent_sdk import ResultMessage


def context_tokens(usage: dict) -> int:
    """Context carried by one model call, from its usage block."""
    return sum(int(usage.get(key, 0) or 0) for key in (
        "input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "output_tokens",
    ))


# What the CLI answers a turn with when the model provider refuses the credential itself. A rate
# limit, an overloaded provider or a network failure is none of these and is waited out.
_CREDENTIAL_REFUSALS = (
    "not logged in", "please run /login", "invalid api key", "invalid x-api-key", "authentication_error",
    "oauth token has expired", "oauth token has been revoked", "invalid bearer token",
)


def credential_refused(result: str | None) -> bool:
    text = (result or "").lower()
    return any(marker in text for marker in _CREDENTIAL_REFUSALS)


# What the CLI or the API answers a turn with when the session no longer fits the model's context.
# Resuming that session fails the same way every time.
_CONTEXT_OVERFLOWS = (
    "prompt is too long", "exceed context limit", "exceeds the context window", "context_length_exceeded",
    "maximum context length",
)


def context_overflowed(message: ResultMessage) -> bool:
    text = " ".join([message.result or "", *(message.errors or [])]).lower()
    return any(marker in text for marker in _CONTEXT_OVERFLOWS)


# Statuses of a request the API refuses for what it asks, which asking again cannot change: a
# coordinator on a model its CLI was too old for got a 400 on every turn and was asked again for
# hours. A timeout, a conflict and a rate limit pass with time, as does every 5xx.
_RETRYABLE_4XX = (408, 409, 429)


def request_refused(message: ResultMessage) -> bool:
    """Whether the API refused the turn's request for good. An overflowed context is rotated instead."""
    status = message.api_error_status
    return (message.is_error and status is not None and 400 <= status < 500 and status not in _RETRYABLE_4XX
            and not context_overflowed(message))
