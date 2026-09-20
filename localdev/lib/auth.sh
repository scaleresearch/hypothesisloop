#!/usr/bin/env bash
# Tokens and cluster registration for local development: hl-admin against the development signing
# secret in controlplane/settings/hypothesisloop.yaml and the compose stack's postgres.

LIB_AUTH_REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
export HYPOTHESISLOOP_CONFIG="${HYPOTHESISLOOP_CONFIG:-${LIB_AUTH_REPO}/controlplane/settings/hypothesisloop.yaml}"
export DATABASE_URL="${DATABASE_URL:-postgres://hypothesisloop:hypothesisloop@localhost:5433/hypothesisloop?sslmode=disable}"

lib_hl_admin() {
  (cd "${LIB_AUTH_REPO}" && go run ./controlplane/cmd/hl-admin "$@")
}

# lib_runtime_token <cluster>: the token a cluster's agents present. It is accepted once the
# cluster is registered (lib_register_local_clusters).
lib_runtime_token() {
  lib_hl_admin token "runtime:$1"
}

# The cluster names the local setups run under, registered for the org every schema starts with.
# Needs the control plane to have started once, since it creates the tables.
lib_register_local_clusters() {
  local name
  for name in local box-a k3s-nvidia; do
    lib_hl_admin cluster add "${name}" default >/dev/null
  done
}
