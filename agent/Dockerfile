# Thin, deliberately unopinionated: uv-managed deps (claude-agent-sdk bundles its own Claude
# Code CLI binary, no separate install needed) + curl (the agent will likely reach for it via
# its Bash tool anyway). No sandboxing of what the agent can do *inside* this container — the
# container boundary itself is the isolation, same as the request that led to this file.
FROM python:3.12-slim

RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv

# permission_mode="bypassPermissions" (unrestricted tool use — the whole point of this agent)
# maps to the CLI's --dangerously-skip-permissions, which refuses to run as root/sudo as a
# safety guard — confirmed live: "cannot be used with root/sudo privileges". A non-root user is
# therefore required, not optional, for this container to start at all.
RUN useradd --create-home --shell /bin/bash agent

WORKDIR /agent
COPY pyproject.toml uv.lock .
RUN uv sync --no-dev --locked

COPY src/ src/
# No docs/ copy: the API contract is fetched live from each service's /llms.txt at runtime
# (see api_client.PlatformClient.fetch_api_guide), not baked into the image.
RUN mkdir -p /agent/workspace && chown -R agent:agent /agent
ENV AGENT_WORKDIR=/agent/workspace

USER agent
ENTRYPOINT ["uv", "run", "python3", "-m", "openresearch_agent"]
