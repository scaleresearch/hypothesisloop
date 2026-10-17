#!/usr/bin/env bash
LIB_ENV_REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LIB_ENV_DIR="${LIB_ENV_REPO}/localdev/.run/env"

# lib_setting KEY: the one value of KEY in the committed settings, the source of the ports the
# control plane listens on.
lib_setting() {
  local value
  value="$(sed -n "s/^  $1: *\([^ #]*\).*$/\1/p" "${LIB_ENV_REPO}/controlplane/settings/hypothesisloop.yaml")"
  if [[ -z "${value}" || "${value}" == *$'\n'* ]]; then
    echo "env: controlplane/settings/hypothesisloop.yaml does not name one $1" >&2
    return 1
  fi
  printf '%s' "${value}"
}
HL_API_PORT="$(lib_setting api_port)"
HL_METRICS_PORT="$(lib_setting metric_controller_port)"
export HL_API_PORT HL_METRICS_PORT

# lib_env_files <settings>: writes localdev/.run/env/<program>.env for every program, holding the
# keys it reads under these settings (localdev/envfiles). .env is read here and by nothing else.
lib_env_files() {
  local settings
  settings="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
  (cd "${LIB_ENV_REPO}" && env -i PATH="${PATH}" HOME="${HOME}" \
    bash -c 'set -a && . ./.env && set +a && exec go run ./localdev/envfiles "$1"' lib_env_files "${settings}")
}

# lib_rented <example> <provider> [<cluster_name>]: the sandbox or cloud_compute example of
# controlplane/settings with the provider .env names chosen, the values of its section .env sets,
# and the cloud_compute entry named cluster_name (localdev/rented).
lib_rented() {
  (cd "${LIB_ENV_REPO}" && env -i PATH="${PATH}" HOME="${HOME}" \
    bash -c 'set -a && . ./.env && set +a && exec go run ./localdev/rented "$@"' lib_rented "$@")
}

# lib_run <program> <command>...: the command with the keys of the program's file, the settings
# path the caller names, and what any command needs to run at all. It replaces the shell it runs
# in, so a caller runs it in a subshell or last.
lib_run() {
  local file="${LIB_ENV_DIR}/$1.env"
  shift
  if [[ ! -r "${file}" ]]; then
    echo "env: no ${file}: make render-settings writes it" >&2
    return 1
  fi
  exec env -i PATH="${PATH}" HOME="${HOME}" ${HYPOTHESISLOOP_CONFIG:+HYPOTHESISLOOP_CONFIG="${HYPOTHESISLOOP_CONFIG}"} \
    bash -c 'set -a && . "$1" && set +a && shift && exec "$@"' lib_run "${file}" "$@"
}
