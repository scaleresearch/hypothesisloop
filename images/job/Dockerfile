# BASE is the job image without the platform's tools (Dockerfile.base), built first.
ARG BASE

# The image a job's command runs in on rented compute. It carries the platform's client and git
# and nothing of any experiment: the job's code comes from its repository through hl-clone and
# its dependencies install when the job starts. Build context is the repo root. The tools are its
# last layers, as they are an environment's (domain.PlatformTools), so a release that changes
# them rebuilds only these.

FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS hl-builder
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY cli/ cli/
COPY contract/ contract/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/hl ./cli

FROM ${BASE}
COPY --from=hl-builder /out/hl /usr/local/bin/hl
COPY agents/coordinator/experiments/hl-clone /usr/local/bin/hl-clone
