#!/bin/sh
# The native build toolchain of the job image and the agent's image, so code an agent builds on
# its machine builds the same in a job. Run as root with curl present and RUSTUP_HOME, CARGO_HOME
# set; both homes are left writable for any user, since the agent's image runs as a non-root one
# and cargo writes its registry and builds into CARGO_HOME.
set -eu
RUST_VERSION=1.95.0

apt-get update
apt-get install -y --no-install-recommends build-essential cmake pkg-config
rm -rf /var/lib/apt/lists/*

curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
  | sh -s -- -y --no-modify-path --profile minimal --default-toolchain "$RUST_VERSION"
chmod -R a+w "$RUSTUP_HOME" "$CARGO_HOME"
