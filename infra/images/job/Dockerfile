# BASE is the image the job's own tools are added to: Python alone for the bare job image, or an
# upstream framework image (Debian or Ubuntu) for one of the others `make job-images` builds.
ARG BASE=python:3.13-slim

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

# A stage of its own: a pull that fails here says why, and one that fails inside COPY --from
# reads as "no stage or image found with that name".
FROM ghcr.io/astral-sh/uv:latest AS uv

FROM ${BASE}
# An upstream image may leave another user set; the tools are installed as root.
USER root
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl git \
    && rm -rf /var/lib/apt/lists/*
COPY --from=uv /uv /uvx /usr/local/bin/
COPY --from=hl-builder /out/hl /usr/local/bin/hl
COPY agents/coordinator/experiments/hl-clone /usr/local/bin/hl-clone
# hl-clone fetches the job's commit from the platform experiment's repository. git asks hl for the
# credential on whichever host it talks to, and hl asks the platform as the job: a read credential
# for that one repository, and nothing for any other address.
RUN git config --system credential.helper \
      '!API_URL="$HYPOTHESISLOOP_API_URL" HL_TOKEN="$HYPOTHESISLOOP_TOKEN" hl git-credential' \
 && git config --system credential.useHttpPath true
WORKDIR /workspace
