# The image a job's command runs in on rented compute. It carries the platform's client and git
# and nothing of any experiment: the job's code comes from its repository through hl-clone and
# its dependencies install when the job starts. Build context is the repo root.

FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS hl-builder
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY cli/ cli/
COPY runtime/shared/exitline/ runtime/shared/exitline/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/hl ./cli

FROM python:3.13-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl git \
    && rm -rf /var/lib/apt/lists/*
COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /usr/local/bin/
COPY --from=hl-builder /out/hl /usr/local/bin/hl
COPY agents/coordinator/experiments/hl-clone /usr/local/bin/hl-clone
WORKDIR /workspace
