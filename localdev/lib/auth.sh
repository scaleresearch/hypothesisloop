#!/usr/bin/env bash
# Local development's auth, and the one place its values come from: the signing secret every
# local process is started with, the key the local control plane verifies a person's JWT against,
# and hl-admin for tokens and cluster registration against the compose stack's postgres.
LIB_AUTH_REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
. "${LIB_AUTH_REPO}/localdev/lib/clusters.sh"

# The signing secret every local process is started with is made on this machine by the first
# render and never leaves it. It has one source, the file: a value from the caller's environment
# is not used. A new one ends every hls_ token derived so far (agents', jobs', runtimes', staff).
# A second machine that mints tokens for this one's control plane (a node's install.sh) needs a
# copy of this file, mode 600.
HL_SIGNING_SECRET_FILE="${LIB_AUTH_REPO}/localdev/.run/signing-secret"
lib_signing_secret() {
  if [[ ! -r "${HL_SIGNING_SECRET_FILE}" ]]; then
    echo "auth: cannot read the signing secret at ${HL_SIGNING_SECRET_FILE}: make render-settings makes it when it is missing" >&2
    return 1
  fi
  HYPOTHESISLOOP_SIGNING_SECRET="$(<"${HL_SIGNING_SECRET_FILE}")"
  if (( ${#HYPOTHESISLOOP_SIGNING_SECRET} < 32 )); then
    echo "auth: the signing secret at ${HL_SIGNING_SECRET_FILE} is shorter than 32 characters: delete it and run make render-settings" >&2
    return 1
  fi
  export HYPOTHESISLOOP_SIGNING_SECRET
}
unset HYPOTHESISLOOP_SIGNING_SECRET
if [[ -e "${HL_SIGNING_SECRET_FILE}" ]]; then
  lib_signing_secret || return 1 2>/dev/null || exit 1
fi

# Who signs people in on the local stack. The first is always a key made on this machine by the
# first render, which never leaves it: the e2e suite and the browser run sign their users with it.
# IDENTITY_PROVIDER in .env lists the sign-in service it names beside it, so a person signs in for
# real on the same stack the tests run against.
export HL_DEV_JWT_KEY="${LIB_AUTH_REPO}/localdev/.dev-jwt-key.pem"
export HL_DEV_JWT_KID="local-dev"
export HL_DEV_JWT_ISSUER="http://localhost/hypothesisloop-dev"
export HL_DEV_JWT_AUDIENCE="hypothesisloop-dev"

# The Google credentials the compose stack's control service mounts (docker-compose.yml): the
# gcloud application default credentials when this machine has them, else /dev/null, since a
# bind mount of a missing file fails.
if [[ -z "${HL_GOOGLE_CREDENTIALS_FILE:-}" ]]; then
  HL_GOOGLE_CREDENTIALS_FILE="${HOME}/.config/gcloud/application_default_credentials.json"
  [[ -f "${HL_GOOGLE_CREDENTIALS_FILE}" ]] || HL_GOOGLE_CREDENTIALS_FILE=/dev/null
fi
export HL_GOOGLE_CREDENTIALS_FILE

lib_env_value() {
  (set +u; . "${LIB_AUTH_REPO}/.env"; printf '%s' "${!1:-}")
}
HL_IDENTITY_PROVIDER=""
if [[ -f "${LIB_AUTH_REPO}/.env" ]]; then
  HL_IDENTITY_PROVIDER="$(lib_env_value IDENTITY_PROVIDER)"
fi

# The git host local platform experiments keep their code on, as GIT_HOST_PROVIDER in .env names
# it (.env.example): github, the GitHub App of the GIT_HOST_GITHUB_APP_* keys, or local, the git
# server `make dev` runs on this machine (localdev/local-git). That one serves the bare
# repositories under HL_GIT_ROOT (make git-repository) to its one credential, whose password is
# made on this machine by the first use and never leaves it. The secrets of either reach every
# process that loads the rendered settings (the compose services, hl-admin) through the
# variables exported here.
HL_GIT_PORT=8085
HL_GIT_ROOT="${LIB_AUTH_REPO}/localdev/.run/git"
HL_GIT_USERNAME=hypothesisloop
HL_GIT_HOST_PROVIDER=""
if [[ -f "${LIB_AUTH_REPO}/.env" ]]; then
  HL_GIT_HOST_PROVIDER="$(lib_env_value GIT_HOST_PROVIDER)"
fi
lib_git_host_provider() {
  case "${HL_GIT_HOST_PROVIDER}" in
    github|local) echo "${HL_GIT_HOST_PROVIDER}" ;;
    "") echo "auth: .env does not set GIT_HOST_PROVIDER, which is github or local (.env.example says what each is)" >&2; return 1 ;;
    *) echo "auth: .env sets GIT_HOST_PROVIDER=${HL_GIT_HOST_PROVIDER}, which is github or local" >&2; return 1 ;;
  esac
}
case "${HL_GIT_HOST_PROVIDER}" in
  github)
    for lib_auth_key in GIT_HOST_GITHUB_APP_ID GIT_HOST_GITHUB_APP_SLUG GIT_HOST_GITHUB_APP_PRIVATE_KEY; do
      if [[ -z "$(lib_env_value "${lib_auth_key}")" ]]; then
        echo "auth: .env sets GIT_HOST_PROVIDER=github and not ${lib_auth_key} (.env.example says what it is)" >&2
        return 1 2>/dev/null || exit 1
      fi
    done
    unset lib_auth_key
    HL_GIT_HOST_GITHUB_APP_ID="$(lib_env_value GIT_HOST_GITHUB_APP_ID)"
    HL_GIT_HOST_GITHUB_APP_SLUG="$(lib_env_value GIT_HOST_GITHUB_APP_SLUG)"
    HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_PRIVATE_KEY="$(lib_env_value GIT_HOST_GITHUB_APP_PRIVATE_KEY)"
    export HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_PRIVATE_KEY
    ;;
  local)
    if [[ ! -s "${LIB_AUTH_REPO}/localdev/.run/git-password" ]]; then
      mkdir -p "${LIB_AUTH_REPO}/localdev/.run"
      (umask 077 && openssl rand -hex 24 > "${LIB_AUTH_REPO}/localdev/.run/git-password")
    fi
    HYPOTHESISLOOP_GIT_HOST_PLAIN_PASSWORD="$(cat "${LIB_AUTH_REPO}/localdev/.run/git-password")"
    export HYPOTHESISLOOP_GIT_HOST_PLAIN_PASSWORD
    ;;
  "") ;;
  *) lib_git_host_provider || return 1 2>/dev/null || exit 1 ;;
esac

# The container registry CONTAINER_REGISTRY_PROVIDER in .env names, read from that provider's own
# keys. lib_container_registry sets HL_REGISTRY, the address the deployment's images are pushed
# under, which make dev-hosted runs them from; HL_ENVIRONMENTS_REPOSITORY, where local
# development's environments are built to; and HL_ENVIRONMENTS_PROVIDER, the lines of the settings'
# environments section after repository: the provider that hands out the logins to it and its own
# section. With no provider all three are empty, and the local control plane builds no environments.
#
#   gcp  Google Artifact Registry: the repository CONTAINER_REGISTRY_GCP_REPOSITORY in
#        CONTAINER_REGISTRY_GCP_LOCATION of CONTAINER_REGISTRY_GCP_PROJECT. Environments go to the
#        private repository <repository>-environments beside it, whose env-pusher and env-puller
#        service accounts the control plane mints tokens of with this machine's application
#        default credentials (gcloud auth application-default login), whose account must be
#        allowed to mint them.
lib_container_registry() {
  local provider key project
  HL_REGISTRY="" HL_ENVIRONMENTS_REPOSITORY="" HL_ENVIRONMENTS_PROVIDER=""
  provider="$(lib_env_value CONTAINER_REGISTRY_PROVIDER)"
  case "${provider}" in
    "") ;;
    gcp)
      for key in CONTAINER_REGISTRY_GCP_PROJECT CONTAINER_REGISTRY_GCP_LOCATION CONTAINER_REGISTRY_GCP_REPOSITORY; do
        if [[ -z "$(lib_env_value "${key}")" ]]; then
          echo "auth: .env sets CONTAINER_REGISTRY_PROVIDER=gcp and not ${key} (.env.example says what it is)" >&2
          return 1
        fi
      done
      project="$(lib_env_value CONTAINER_REGISTRY_GCP_PROJECT)"
      HL_REGISTRY="$(lib_env_value CONTAINER_REGISTRY_GCP_LOCATION)-docker.pkg.dev/${project}/$(lib_env_value CONTAINER_REGISTRY_GCP_REPOSITORY)"
      HL_ENVIRONMENTS_REPOSITORY="${HL_REGISTRY}-environments/environment"
      HL_ENVIRONMENTS_PROVIDER="$(printf '  provider: gcp\n  gcp:\n    pusher: env-pusher@%s.iam.gserviceaccount.com\n    puller: env-puller@%s.iam.gserviceaccount.com' "${project}" "${project}")"
      ;;
    *)
      echo "auth: .env sets CONTAINER_REGISTRY_PROVIDER=${provider}; it is empty or gcp" >&2
      return 1
      ;;
  esac
}
HL_REGISTRY="" HL_ENVIRONMENTS_REPOSITORY="" HL_ENVIRONMENTS_PROVIDER=""
if [[ -f "${LIB_AUTH_REPO}/.env" ]]; then
  lib_container_registry || return 1 2>/dev/null || exit 1
fi

# lib_render_settings <host address> <output>: the settings the local control plane runs with,
# which are the committed ones with this host's address and the local identity providers filled
# in, the git host GIT_HOST_PROVIDER names: the GitHub App or the local git server,
# and the environments section when .env names a registry.
lib_render_settings() {
  local host_address="$1" output="$2" coordinate providers project issuer keys rendered
  if [[ ! -e "${HL_SIGNING_SECRET_FILE}" ]]; then
    mkdir -p "$(dirname "${HL_SIGNING_SECRET_FILE}")"
    (umask 077 && openssl rand -hex 32 > "${HL_SIGNING_SECRET_FILE}")
  fi
  lib_signing_secret || return 1
  if [[ ! -f "${HL_DEV_JWT_KEY}" ]]; then
    (umask 077 && openssl ecparam -name prime256v1 -genkey -noout | openssl pkcs8 -topk8 -nocrypt -out "${HL_DEV_JWT_KEY}")
  fi
  coordinate() {
    openssl ec -in "${HL_DEV_JWT_KEY}" -pubout -outform DER 2>/dev/null | tail -c 64 | "$@" | base64 | tr '+/' '-_' | tr -d '=\n'
  }
  providers="{issuer: \"${HL_DEV_JWT_ISSUER}\", audience: ${HL_DEV_JWT_AUDIENCE}, jwt_keys: [{kid: ${HL_DEV_JWT_KID}, kty: EC, alg: ES256, crv: P-256, x: \"$(coordinate head -c 32)\", y: \"$(coordinate tail -c 32)\"}]}"
  if [[ "${HL_IDENTITY_PROVIDER}" == "supabase" ]]; then
    project="$(lib_env_value IDENTITY_SUPABASE_PROJECT_REF)"
    if [[ -z "${project}" ]]; then
      echo "render-settings: .env sets IDENTITY_PROVIDER=supabase and not IDENTITY_SUPABASE_PROJECT_REF" >&2
      return 1
    fi
    issuer="https://${project}.supabase.co/auth/v1"
    keys="$(curl -fsS "${issuer}/.well-known/jwks.json" | python3 -c 'import json, sys; print(json.dumps([{f: k[f] for f in ("kid", "kty", "alg", "crv", "x", "y", "n", "e") if f in k} for k in json.load(sys.stdin)["keys"]]))')" || {
      echo "render-settings: could not read the signing keys of ${issuer}" >&2
      return 1
    }
    providers="${providers}, {issuer: \"${issuer}\", audience: authenticated, jwt_keys: ${keys}}"
  elif [[ -n "${HL_IDENTITY_PROVIDER}" ]]; then
    echo "render-settings: .env sets IDENTITY_PROVIDER=${HL_IDENTITY_PROVIDER}; it is empty or supabase" >&2
    return 1
  fi
  # The local runtimes install the images `make images` pushes to the dev registry, under the
  # tag of this tree (make's own TAG when make runs this).
  local image_tag="${TAG:-}"
  if [[ -z "${image_tag}" ]]; then
    image_tag="$(bash "${LIB_AUTH_REPO}/localdev/lib/image-tag.sh")" && [[ -n "${image_tag}" ]] || {
      echo "render-settings: no image tag: localdev/lib/image-tag.sh failed, set TAG" >&2
      return 1
    }
  fi
  sed -e "s|REPLACE-WITH-HOST-ADDRESS|${host_address}|g" \
      -e "s|REPLACE-WITH-IMAGE-TAG|${image_tag}|g" \
      -e "s|REPLACE-WITH-UI-ORIGIN|http://localhost:3000|g" \
      -e "s|^  identity_providers: \[\]$|  identity_providers: [${providers}]|" \
      "${LIB_AUTH_REPO}/controlplane/settings/hypothesisloop.yaml" > "${output}"
  for rendered in "issuer: \"${HL_DEV_JWT_ISSUER}\"" ${issuer:+"issuer: \"${issuer}\""}; do
    grep -qF -- "${rendered}" "${output}" || { echo "render-settings: the auth section of the settings file changed shape, '${rendered}' was not rendered" >&2; return 1; }
  done
  if grep -qE '^(git_host|environments):' "${output}"; then
    echo "render-settings: the settings file has a git_host or environments section, and the render would add a second one" >&2
    return 1
  fi
  if [[ -n "${HL_ENVIRONMENTS_REPOSITORY}" ]]; then
    printf '\nenvironments:\n  repository: %s\n%s\n' "${HL_ENVIRONMENTS_REPOSITORY}" "${HL_ENVIRONMENTS_PROVIDER}" >> "${output}"
  fi
  case "$(lib_git_host_provider)" in
    github)
      # The values are JSON strings, which YAML reads as they are.
      printf '\ngit_host:\n  provider: github\n  github:\n    app_id: %d\n    app_slug: "%s"\n    private_key: ${HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_PRIVATE_KEY}\n' \
        "${HL_GIT_HOST_GITHUB_APP_ID}" "${HL_GIT_HOST_GITHUB_APP_SLUG}" >> "${output}"
      ;;
    local)
      printf '\ngit_host:\n  provider: plain\n  plain:\n    base_url: http://%s:%s\n    allow_http: true\n    username: %s\n    password: ${HYPOTHESISLOOP_GIT_HOST_PLAIN_PASSWORD}\n' \
        "${host_address}" "${HL_GIT_PORT}" "${HL_GIT_USERNAME}" >> "${output}"
      ;;
    *) return 1 ;;
  esac

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
  lib_signing_secret || return 1
  lib_fresh_settings || return 1
  (cd "${LIB_AUTH_REPO}" && go run ./controlplane/cmd/hl-admin "$@")
}

# lib_runtime_token <cluster>: the token a cluster's agents present. It is accepted once the
# cluster is registered (lib_register_clusters, or lib_register_local_cluster for local).
lib_runtime_token() {
  lib_hl_admin token "runtime:$1"
}

# The clusters on the platform's own machines (a Tenstorrent box, an NVIDIA host) that
# OWNED_COMPUTE_CLUSTERS in .env lists (lib_owned_clusters), registered for the org every schema
# starts with, and the launcher the committed settings give hosted agents to (agents.launcher).
# Needs the control plane to have started once, since it creates the tables.
lib_register_clusters() {
  local clusters line name ceiling
  clusters="$(lib_owned_clusters)" || return 1
  while read -r line; do
    [[ -n "${line}" ]] || continue
    name="${line%% *}"
    lib_hl_admin cluster add "${name}" default >/dev/null
    ceiling="$(lib_cluster_field silence_ceiling_seconds "${line}")"
    if [[ -n "${ceiling}" ]]; then
      lib_hl_admin cluster settings "${name}" "{\"status_silence_ceiling_seconds\":${ceiling}}"
    fi
  done <<<"${clusters}"
  lib_hl_admin launcher add main >/dev/null
}

# The k3s cluster on this machine (localdev/k3s-macos), whose nodes fake their accelerators and
# run x86_64 images under emulation. Only a stack that rents nothing (the e2e suite's) has it.
lib_register_local_cluster() {
  lib_hl_admin cluster add local default >/dev/null
}
