#!/usr/bin/env bash
# Local development's auth, and the one place its values live: the signing secret every local
# process is started with, the key the local control plane verifies a person's JWT against, and
# hl-admin for tokens and cluster registration against the compose stack's postgres.

LIB_AUTH_REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
export HYPOTHESISLOOP_SIGNING_SECRET="${HYPOTHESISLOOP_SIGNING_SECRET:-local-development-signing-secret-do-not-deploy}"

# The identity provider local development stands in for. The private key is made on this machine
# by the first render and never leaves it; the e2e suite signs its users' tokens with it.
export HL_DEV_JWT_KEY="${LIB_AUTH_REPO}/localdev/.dev-jwt-key.pem"
export HL_DEV_JWT_KID="local-dev"
export HL_DEV_JWT_ISSUER="http://localhost/hypothesisloop-dev"
export HL_DEV_JWT_AUDIENCE="hypothesisloop-dev"

# lib_render_settings <host address> <output>: the settings the local control plane runs with,
# which are the committed ones with this host's address and the local JWT key filled in.
lib_render_settings() {
  local host_address="$1" output="$2" coordinate jwk
  if [[ ! -f "${HL_DEV_JWT_KEY}" ]]; then
    (umask 077 && openssl ecparam -name prime256v1 -genkey -noout | openssl pkcs8 -topk8 -nocrypt -out "${HL_DEV_JWT_KEY}")
  fi
  coordinate() {
    openssl ec -in "${HL_DEV_JWT_KEY}" -pubout -outform DER 2>/dev/null | tail -c 64 | "$@" | base64 | tr '+/' '-_' | tr -d '=\n'
  }
  jwk="{kid: ${HL_DEV_JWT_KID}, kty: EC, alg: ES256, crv: P-256, x: \"$(coordinate head -c 32)\", y: \"$(coordinate tail -c 32)\"}"
  sed -e "s|REPLACE-WITH-HOST-ADDRESS|${host_address}|g" \
      -e "s|^  issuer: \"\"$|  issuer: ${HL_DEV_JWT_ISSUER}|" \
      -e "s|^  audience: \"\"$|  audience: ${HL_DEV_JWT_AUDIENCE}|" \
      -e "s|^  jwt_keys: \[\]$|  jwt_keys: [${jwk}]|" \
      "${LIB_AUTH_REPO}/controlplane/settings/hypothesisloop.yaml" > "${output}"
  for rendered in "issuer: ${HL_DEV_JWT_ISSUER}" "audience: ${HL_DEV_JWT_AUDIENCE}" "kid: ${HL_DEV_JWT_KID}"; do
    grep -q "${rendered}" "${output}" || { echo "render-settings: the auth section of the settings file changed shape, '${rendered}' was not rendered" >&2; return 1; }
  done
}
# hl-admin reads the settings the local control plane runs with. The committed file holds
# placeholders and is refused at load, so `make render-settings` comes first.
export HYPOTHESISLOOP_CONFIG="${HYPOTHESISLOOP_CONFIG:-${LIB_AUTH_REPO}/controlplane/settings/.hypothesisloop.rendered.yaml}"
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
