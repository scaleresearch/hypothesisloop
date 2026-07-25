# Thin, deliberately unopinionated: uv-managed deps (claude-agent-sdk bundles its own Claude
# Code CLI binary, no separate install needed) + curl (the agent will likely reach for it via
# its Bash tool anyway). No sandboxing of what the agent can do *inside* this container — the
# container boundary itself is the isolation, same as the request that led to this file.
FROM python:3.13-slim

# git: the agent pushes the exact code each job runs to its branch in the shared CODE_REPO_URL
# (system prompt step 5), so every experiment's code_ref resolves to a real, cloneable commit —
# that is the trace from a submitted job back to its source.
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates git \
    && rm -rf /var/lib/apt/lists/*
COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv

# Non-interactive git auth, baked so the agent never handles credentials: a helper emits $GIT_TOKEN
# (injected at runtime as a secret, never baked) for HTTPS clone/push to CODE_REPO_URL, plus a
# default commit identity so commits don't fail on a fresh container. The agent hands the same
# token to job pods via job.env, so the pod can clone the code_ref it runs.
RUN git config --system credential.helper \
      '!f() { echo "username=${GIT_USER:-x-access-token}"; echo "password=${GIT_TOKEN}"; }; f' \
 && git config --system user.name  "hypothesisloop-agent" \
 && git config --system user.email "agent@hypothesisloop.local" \
 && git config --system init.defaultBranch main

# permission_mode="bypassPermissions" (unrestricted tool use — the whole point of this agent)
# maps to the CLI's --dangerously-skip-permissions, which refuses to run as root/sudo as a
# safety guard — confirmed live: "cannot be used with root/sudo privileges". A non-root user is
# therefore required, not optional, for this container to start at all.
RUN useradd --create-home --shell /bin/bash agent

# tt-metal + TT-NN source baked in: the agent container has no /dev/tenstorrent (compute happens
# in job pods running Tenstorrent's release image), so this is here to be *read*, not run — the
# op sources, kernels and tests the agent needs to form hypotheses about what to change. TT-NN
# is not a separate repo: it ships inside tt-metal as ttnn/, so one clone baked in covers both
# (TTNN_SRC below points at it directly). Shallow + blobless: full history/blobs are ~GBs and
# nothing here needs them; git backfills any blob on demand when the agent actually reads a file.
ENV TT_METAL_SRC=/opt/tt-metal
ENV TTNN_SRC=/opt/tt-metal/ttnn
RUN git clone --depth 1 --filter=blob:none \
    https://github.com/tenstorrent/tt-metal.git $TT_METAL_SRC \
    && chown -R agent:agent $TT_METAL_SRC

# Build context is the repo root (see Makefile `experimentator-image`) so the workload samples
# can be baked in: WORKLOAD_SAMPLES is the agent's starting point for what a job workload looks
# like — read, copy, adapt into its own repo. Not run here (no device); read-only reference.
ENV WORKLOAD_SAMPLES=/opt/workloads-samples
COPY tests/workloads $WORKLOAD_SAMPLES

WORKDIR /agent
COPY agents/experimentator/pyproject.toml agents/experimentator/uv.lock .
RUN uv sync --no-dev --locked

COPY agents/experimentator/src/ src/
# No docs/ copy: the API contract is fetched live from each service's /llms.txt at runtime
# (see api_client.PlatformClient.fetch_api_guide), not baked into the image.
RUN mkdir -p /agent/workspace && chown -R agent:agent /agent
ENV AGENT_WORKDIR=/agent/workspace

USER agent
ENTRYPOINT ["uv", "run", "python3", "-m", "hypothesisloop_agent"]
