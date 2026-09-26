#!/usr/bin/env bash
# Local development's auth, and the one place its values live: the signing secret every local
# process is started with, the key the local control plane verifies a person's JWT against, and
# hl-admin for tokens and cluster registration against the compose stack's postgres.

LIB_AUTH_REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
export HYPOTHESISLOOP_SIGNING_SECRET="${HYPOTHESISLOOP_SIGNING_SECRET:-local-development-signing-secret-do-not-deploy}"

# Who signs people in on the local stack. The first is always a key made on this machine by the
# first render, which never leaves it: the e2e suite and the browser run sign their users with it.
# LOCAL_IDENTITY_PROVIDER=supabase in .env lists the Supabase project beside it, so a person
# signs in for real on the same stack the tests run against.
export HL_DEV_JWT_KEY="${LIB_AUTH_REPO}/localdev/.dev-jwt-key.pem"
export HL_DEV_JWT_KID="local-dev"
export HL_DEV_JWT_ISSUER="http://localhost/hypothesisloop-dev"
export HL_DEV_JWT_AUDIENCE="hypothesisloop-dev"

# The GitHub App local development reaches repositories through, when the repository's .env
# names one by GITHUB_APP_ID (.env.example). The rendered settings then get a github section, and
# its two secrets reach every process that loads them (the compose services, hl-admin) through
# the two variables exported here. With no .env or no id there is no App and no section.
lib_env_value() {
  (set +u; . "${LIB_AUTH_REPO}/.env"; printf '%s' "${!1:-}")
}
HL_LOCAL_IDENTITY_PROVIDER=""
HL_GITHUB_APP_ID=""
if [[ -f "${LIB_AUTH_REPO}/.env" ]]; then
  HL_GITHUB_APP_ID="$(lib_env_value GITHUB_APP_ID)"
  HL_LOCAL_IDENTITY_PROVIDER="$(lib_env_value LOCAL_IDENTITY_PROVIDER)"
fi
if [[ -n "${HL_GITHUB_APP_ID}" ]]; then
  for lib_auth_key in GITHUB_APP_SLUG GITHUB_APP_CLIENT_ID GITHUB_APP_CLIENT_SECRET GITHUB_APP_PRIVATE_KEY; do
    if [[ -z "$(lib_env_value "${lib_auth_key}")" ]]; then
      echo "auth: .env sets GITHUB_APP_ID and not ${lib_auth_key} (.env.example says what it is)" >&2
      return 1 2>/dev/null || exit 1
    fi
  done
  unset lib_auth_key
  HL_GITHUB_APP_SLUG="$(lib_env_value GITHUB_APP_SLUG)"
  HL_GITHUB_APP_CLIENT_ID="$(lib_env_value GITHUB_APP_CLIENT_ID)"
  HYPOTHESISLOOP_GITHUB_APP_CLIENT_SECRET="$(lib_env_value GITHUB_APP_CLIENT_SECRET)"
  HYPOTHESISLOOP_GITHUB_APP_PRIVATE_KEY="$(lib_env_value GITHUB_APP_PRIVATE_KEY)"
  export HYPOTHESISLOOP_GITHUB_APP_CLIENT_SECRET HYPOTHESISLOOP_GITHUB_APP_PRIVATE_KEY
fi

# lib_render_settings <host address> <output>: the settings the local control plane runs with,
# which are the committed ones with this host's address and the local identity providers filled
# in, and the GitHub App's section when there is one.
lib_render_settings() {
  local host_address="$1" output="$2" coordinate providers project issuer keys rendered
  if [[ ! -f "${HL_DEV_JWT_KEY}" ]]; then
    (umask 077 && openssl ecparam -name prime256v1 -genkey -noout | openssl pkcs8 -topk8 -nocrypt -out "${HL_DEV_JWT_KEY}")
  fi
  coordinate() {
    openssl ec -in "${HL_DEV_JWT_KEY}" -pubout -outform DER 2>/dev/null | tail -c 64 | "$@" | base64 | tr '+/' '-_' | tr -d '=\n'
  }
  providers="{issuer: \"${HL_DEV_JWT_ISSUER}\", audience: ${HL_DEV_JWT_AUDIENCE}, jwt_keys: [{kid: ${HL_DEV_JWT_KID}, kty: EC, alg: ES256, crv: P-256, x: \"$(coordinate head -c 32)\", y: \"$(coordinate tail -c 32)\"}]}"
  if [[ "${HL_LOCAL_IDENTITY_PROVIDER}" == "supabase" ]]; then
    project="$(lib_env_value SUPABASE_PROJECT_REF)"
    if [[ -z "${project}" ]]; then
      echo "render-settings: .env sets LOCAL_IDENTITY_PROVIDER=supabase and not SUPABASE_PROJECT_REF" >&2
      return 1
    fi
    issuer="https://${project}.supabase.co/auth/v1"
    keys="$(curl -fsS "${issuer}/.well-known/jwks.json" | python3 -c 'import json, sys; print(json.dumps([{f: k[f] for f in ("kid", "kty", "alg", "crv", "x", "y", "n", "e") if f in k} for k in json.load(sys.stdin)["keys"]]))')" || {
      echo "render-settings: could not read the signing keys of ${issuer}" >&2
      return 1
    }
    providers="${providers}, {issuer: \"${issuer}\", audience: authenticated, jwt_keys: ${keys}}"
  elif [[ -n "${HL_LOCAL_IDENTITY_PROVIDER}" ]]; then
    echo "render-settings: .env sets LOCAL_IDENTITY_PROVIDER=${HL_LOCAL_IDENTITY_PROVIDER}; it is empty or supabase" >&2
    return 1
  fi
  # The local runtimes install the images `make images` pushes to the dev registry, under the
  # tag of this tree (make's own TAG when make runs this).
  local image_tag="${TAG:-$(bash "${LIB_AUTH_REPO}/localdev/lib/image-tag.sh")}"
  sed -e "s|REPLACE-WITH-HOST-ADDRESS|${host_address}|g" \
      -e "s|REPLACE-WITH-IMAGE-TAG|${image_tag}|g" \
      -e "s|^  identity_providers: \[\]$|  identity_providers: [${providers}]|" \
      "${LIB_AUTH_REPO}/controlplane/settings/hypothesisloop.yaml" > "${output}"
  for rendered in "issuer: \"${HL_DEV_JWT_ISSUER}\"" ${issuer:+"issuer: \"${issuer}\""}; do
    grep -qF -- "${rendered}" "${output}" || { echo "render-settings: the auth section of the settings file changed shape, '${rendered}' was not rendered" >&2; return 1; }
  done
  if [[ -n "${HL_GITHUB_APP_ID}" ]]; then
    if grep -q '^github:' "${output}"; then
      echo "render-settings: the settings file has a github section, and .env would add a second one" >&2
      return 1
    fi
    # The values are JSON strings, which YAML reads as they are.
    printf '\ngithub:\n  app_id: %d\n  app_slug: "%s"\n  client_id: "%s"\n  client_secret: ${HYPOTHESISLOOP_GITHUB_APP_CLIENT_SECRET}\n  private_key: ${HYPOTHESISLOOP_GITHUB_APP_PRIVATE_KEY}\n' \
      "${HL_GITHUB_APP_ID}" "${HL_GITHUB_APP_SLUG}" "${HL_GITHUB_APP_CLIENT_ID}" >> "${output}"
  fi
}
# hl-admin reads the settings the local control plane runs with. The committed file holds
# placeholders and is refused at load, so `make render-settings` comes first.
export HYPOTHESISLOOP_CONFIG="${HYPOTHESISLOOP_CONFIG:-${LIB_AUTH_REPO}/controlplane/settings/.hypothesisloop.rendered.yaml}"
export DATABASE_URL="${DATABASE_URL:-postgres://hypothesisloop:hypothesisloop@localhost:5433/hypothesisloop?sslmode=disable}"

# lib_fresh_settings: renders the local settings again when the committed file is newer than
# them. A caller outside make (a test runner, a script) otherwise loads settings rendered before
# a required key was added, and hl-admin stops on the key it misses. Settings named by the
# caller are the caller's to keep.
lib_fresh_settings() {
  local rendered="${LIB_AUTH_REPO}/controlplane/settings/.hypothesisloop.rendered.yaml"
  local address_file="${LIB_AUTH_REPO}/localdev/.host-address"
  if [[ "${HYPOTHESISLOOP_CONFIG}" != "${rendered}" ]]; then
    return 0
  fi
  if [[ -f "${rendered}" && ! "${LIB_AUTH_REPO}/controlplane/settings/hypothesisloop.yaml" -nt "${rendered}" ]]; then
    return 0
  fi
  if [[ ! -s "${address_file}" ]]; then
    echo "auth: the local settings are not rendered from the current controlplane/settings/hypothesisloop.yaml: run make render-settings" >&2
    return 1
  fi
  lib_render_settings "$(cat "${address_file}")" "${rendered}"
}

lib_hl_admin() {
  lib_fresh_settings || return 1
  (cd "${LIB_AUTH_REPO}" && go run ./controlplane/cmd/hl-admin "$@")
}

# lib_runtime_token <cluster>: the token a cluster's agents present. It is accepted once the
# cluster is registered (lib_register_local_clusters).
lib_runtime_token() {
  lib_hl_admin token "runtime:$1"
}

# The cluster names the local setups run under, registered for the org every schema starts with,
# and the launcher the committed settings give hosted agents to (agents.launcher).
# Needs the control plane to have started once, since it creates the tables.
lib_register_local_clusters() {
  local name
  for name in local box-a k3s-nvidia; do
    lib_hl_admin cluster add "${name}" default >/dev/null
  done
  lib_hl_admin launcher add main >/dev/null
}
