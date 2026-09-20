#!/usr/bin/env bash
# publish.sh <stack>
#
# Builds every image for linux/amd64 (rented hosts and agent VMs are amd64) and pushes it to the
# stack's registry, then pins the stack to the tag it pushed.
set -euo pipefail

STACK="${1:?usage: publish.sh <stack>}"
: "${GCP_ACCOUNT:?GCP_ACCOUNT must name the Google account that may push to the registry}"

INFRA_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_DIR="$(dirname "${INFRA_DIR}")"
PLATFORM="linux/amd64"

REGISTRY="$("${INFRA_DIR}/pulumi.sh" "${STACK}" stack output registry)"
TAG="$(bash "${REPO_DIR}/localdev/lib/image-tag.sh")"

gcloud auth print-access-token --account "${GCP_ACCOUNT}" \
  | podman login --username oauth2accesstoken --password-stdin "${REGISTRY%%/*}"

# What the UI image compiles into the browser bundle, all public: the API as a browser reaches
# it and the Supabase project that signs people in.
make -C "${REPO_DIR}" images REGISTRY="${REGISTRY}" TAG="${TAG}" REGISTRY_TLS_VERIFY=true IMAGE_PLATFORM="${PLATFORM}" \
  NEXT_PUBLIC_API_URL="$("${INFRA_DIR}/pulumi.sh" "${STACK}" stack output browserAPIURL)" \
  NEXT_PUBLIC_SUPABASE_URL="$("${INFRA_DIR}/pulumi.sh" "${STACK}" stack output supabaseURL)" \
  NEXT_PUBLIC_SUPABASE_PUBLISHABLE_KEY="$("${INFRA_DIR}/pulumi.sh" "${STACK}" config get supabasePublishableKey)"

# The two images that are built from the repo root and are no part of `make images`:
# the agent VM's image and the image a job's command runs in.
publish() {
  local name="$1" dockerfile="$2"
  echo "==> building hypothesisloop-${name}:${TAG}"
  podman build --platform "${PLATFORM}" -f "${dockerfile}" -t "${REGISTRY}/hypothesisloop-${name}:${TAG}" "${REPO_DIR}"
  podman push "${REGISTRY}/hypothesisloop-${name}:${TAG}"
}
publish experimentator-base "${REPO_DIR}/agents/experimentator/Dockerfile.base"
publish job "${INFRA_DIR}/images/job/Dockerfile"

"${INFRA_DIR}/pulumi.sh" "${STACK}" config set imageTag "${TAG}"
echo "publish: pushed ${REGISTRY}/hypothesisloop-*:${TAG}"
