#!/usr/bin/env bash
# e2b-template.sh <image> <cores> <memory-mib>
#
# Builds the E2B template the e2b adapter starts a sandbox of <image> from at that size. E2B
# boots from a template, and a template fixes cores and memory, so there is one per image and
# size. Its name is a function of the three arguments, the same one the adapter computes
# (runtime/cloud/internal/provider/e2b/template.go), so nothing maps images to templates.
#
# E2B takes the image's files and drops its config, so the image's ENV, WORKDIR and USER are read
# from the registry and added as build steps, which needs base64 in the image. ENTRYPOINT and CMD
# are not needed: the adapter starts the command it is given.
#
# The image tag is part of the name and its content is not: after pushing new content under an
# old tag, run this again.
#
# Needs curl and jq, E2B_API_KEY, and for an image in a private registry
# E2B_REGISTRY_USERNAME and E2B_REGISTRY_PASSWORD, which both this script and E2B pull with.
set -euo pipefail

IMAGE="${1:?usage: e2b-template.sh <image> <cores> <memory-mib>}"
CORES="${2:?usage: e2b-template.sh <image> <cores> <memory-mib>}"
MEMORY_MIB="${3:?usage: e2b-template.sh <image> <cores> <memory-mib>}"
: "${E2B_API_KEY:?E2B_API_KEY must hold the team API key}"
API="https://api.e2b.app"

if command -v sha256sum >/dev/null; then
  DIGEST="$(printf '%s' "${IMAGE}" | sha256sum)"
else
  DIGEST="$(printf '%s' "${IMAGE}" | shasum -a 256)"
fi
NAME="hl-${DIGEST:0:20}-${CORES}c-${MEMORY_MIB}m"

# The secrets reach curl through file descriptors, never through its arguments.
e2b() {
  local method="$1" path="$2"
  curl -sS --fail-with-body -X "${method}" \
    -H @<(printf 'X-API-Key: %s\nContent-Type: application/json\n' "${E2B_API_KEY}") \
    --data-binary @- "${API}${path}"
}

# --- the image's config, from its registry ---------------------------------------------------

REF="${IMAGE}"
REGISTRY="registry-1.docker.io"
case "${REF}" in
  */*)
    case "${REF%%/*}" in
      *.*|*:*|localhost) REGISTRY="${REF%%/*}"; REF="${REF#*/}" ;;
    esac ;;
  *) REF="library/${REF}" ;;
esac
case "${REF}" in
  *@*) REPOSITORY="${REF%%@*}"; REFERENCE="${REF#*@}" ;;
  *:*) REPOSITORY="${REF%:*}"; REFERENCE="${REF##*:}" ;;
  *) REPOSITORY="${REF}"; REFERENCE="latest" ;;
esac

registry_credentials() {
  if [[ -n "${E2B_REGISTRY_USERNAME:-}" ]]; then
    printf 'Authorization: Basic %s\n' "$(printf '%s:%s' "${E2B_REGISTRY_USERNAME}" "${E2B_REGISTRY_PASSWORD:?E2B_REGISTRY_PASSWORD must be set with E2B_REGISTRY_USERNAME}" | base64 | tr -d '\n')"
  fi
}

# A registry answers its first request with the address of the token service to ask.
CHALLENGE="$(curl -sS -o /dev/null -D - "https://${REGISTRY}/v2/" | tr -d '\r' | grep -i '^www-authenticate:' || true)"
REGISTRY_AUTH=""
case "${CHALLENGE}" in
  *[Bb]earer*)
    REALM="$(sed -n 's/.*realm="\([^"]*\)".*/\1/p' <<<"${CHALLENGE}")"
    SERVICE="$(sed -n 's/.*service="\([^"]*\)".*/\1/p' <<<"${CHALLENGE}")"
    TOKEN="$(curl -sS --fail-with-body -G -H @<(registry_credentials) \
      --data-urlencode "service=${SERVICE}" --data-urlencode "scope=repository:${REPOSITORY}:pull" "${REALM}" | jq -r '.token // .access_token')"
    REGISTRY_AUTH="Authorization: Bearer ${TOKEN}" ;;
  *[Bb]asic*) REGISTRY_AUTH="$(registry_credentials)" ;;
esac

registry() {
  curl -sS --fail-with-body -L -H @<(printf '%s\n' "${REGISTRY_AUTH}") \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
    "https://${REGISTRY}/v2/${REPOSITORY}/$1"
}

MANIFEST="$(registry "manifests/${REFERENCE}")"
if jq -e '.manifests' >/dev/null <<<"${MANIFEST}"; then
  PLATFORM_DIGEST="$(jq -r '[.manifests[] | select(.platform.os == "linux" and .platform.architecture == "amd64")][0].digest // empty' <<<"${MANIFEST}")"
  [[ -n "${PLATFORM_DIGEST}" ]] || { echo "e2b-template: ${IMAGE} has no linux/amd64 image, which is what E2B runs" >&2; exit 1; }
  MANIFEST="$(registry "manifests/${PLATFORM_DIGEST}")"
fi
CONFIG="$(registry "blobs/$(jq -r '.config.digest' <<<"${MANIFEST}")")"

# A template keeps an ENV step for the build only, so the image's environment goes into a file
# the adapter's supervisor reads before it sets the command's own (supervise.go). Every value is
# in single quotes, inside which a shell reads each byte as itself. USER comes last: the file and
# the working directory are made by root.
# The names are checked first: a name is written into that file unquoted, and one that is not a
# variable name would be read by the shell of every sandbox as a command.
BAD_NAME="$(jq -r '[(.config.Env // [])[] | split("=")[0] | select(test("\\A[A-Za-z_][A-Za-z0-9_]*\\z") | not)][0] // empty | @json' <<<"${CONFIG}")"
[[ -z "${BAD_NAME}" ]] || { echo "e2b-template: ${IMAGE} sets ${BAD_NAME} in its ENV, which is not an environment variable name" >&2; exit 1; }
IMAGE_ENV="$(jq -r '(.config.Env // [])[] | capture("^(?<k>[^=]*)=(?<v>.*)$"; "s")
  | "export \(.k)=\u0027\(.v | gsub("\u0027"; "\u0027\\\u0027\u0027"))\u0027"' <<<"${CONFIG}")"
STEPS="$(jq -c --arg env "$(printf '%s\n' "${IMAGE_ENV}" | base64 | tr -d '\n')" '
  .config as $c
  | [ {type: "RUN", args: ["echo \($env) | base64 -d > /etc/hl-image.env && chmod 644 /etc/hl-image.env", "root"]},
      (($c.WorkingDir // "") | select(. != "") | {type: "WORKDIR", args: [.]}),
      (($c.User // "") | select(. != "") | {type: "USER", args: [.]}) ]' <<<"${CONFIG}")"

# --- the template ----------------------------------------------------------------------------

echo "==> template ${NAME} from ${IMAGE}: ${CORES} cores, ${MEMORY_MIB} MiB"
echo "    steps taken from the image: $(jq -c 'map(.type)' <<<"${STEPS}")"

CREATED="$(jq -n --arg name "${NAME}" --argjson cores "${CORES}" --argjson memory "${MEMORY_MIB}" \
  '{name: $name, cpuCount: $cores, memoryMB: $memory}' | e2b POST /v3/templates)"
TEMPLATE_ID="$(jq -r '.templateID' <<<"${CREATED}")"
BUILD_ID="$(jq -r '.buildID' <<<"${CREATED}")"

jq -n --arg image "${IMAGE}" --argjson steps "${STEPS}" '
  {fromImage: $image, steps: $steps}
  + (if (env.E2B_REGISTRY_USERNAME // "") != ""
     then {fromImageRegistry: {type: "registry", username: env.E2B_REGISTRY_USERNAME, password: env.E2B_REGISTRY_PASSWORD}}
     else {} end)' | e2b POST "/v2/templates/${TEMPLATE_ID}/builds/${BUILD_ID}" >/dev/null

OFFSET=0
while :; do
  STATUS="$(e2b GET "/templates/${TEMPLATE_ID}/builds/${BUILD_ID}/status?logsOffset=${OFFSET}&limit=100" </dev/null)"
  jq -r '.logEntries[] | "    \(.message)"' <<<"${STATUS}" | tr -d '\r'
  OFFSET=$((OFFSET + $(jq '.logEntries | length' <<<"${STATUS}")))
  case "$(jq -r '.status' <<<"${STATUS}")" in
    ready) break ;;
    error) echo "e2b-template: the build of ${NAME} failed: $(jq -r '.reason.message // "no reason given"' <<<"${STATUS}")" >&2; exit 1 ;;
  esac
  sleep 5
done
echo "e2b-template: ${NAME} is ready (template ${TEMPLATE_ID})"
