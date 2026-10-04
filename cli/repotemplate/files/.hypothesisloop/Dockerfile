ARG BASE
FROM ${BASE}
# Built from this file alone with no build context, so COPY and ADD see no repository files: the job clones the code at start.
# Fetch and build the pinned dependencies here too (pip or uv install of the lock, cargo fetch and build, maven go-offline), so a job compiles only the code it varies.
# A RUN reads the repository at a commit it pins through the secret git-credentials, which stays out of the image, e.g.:
# RUN --mount=type=secret,id=git-credentials git -c credential.helper='store --file=/run/secrets/git-credentials' clone <repository url> /tmp/src && git -C /tmp/src checkout <commit> && cd /tmp/src && cargo fetch && rm -rf /tmp/src
# RUN apt-get update && apt-get install -y --no-install-recommends build-essential && rm -rf /var/lib/apt/lists/*
