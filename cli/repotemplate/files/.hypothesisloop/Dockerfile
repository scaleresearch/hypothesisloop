ARG BASE
FROM ${BASE}
# Built from this file alone with no build context, so COPY and ADD see no repository files: the job clones the code at start.
# Fetch and build the pinned dependencies here too (pip or uv install of the lock, cargo fetch and build, maven go-offline), so a job compiles only the code it varies.
# RUN apt-get update && apt-get install -y --no-install-recommends build-essential && rm -rf /var/lib/apt/lists/*
