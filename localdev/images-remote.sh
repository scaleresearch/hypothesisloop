#!/usr/bin/env bash
# images-remote.sh BUILDER [--import]
#
# Runs make publish on the machine BUILDER names (an ssh host, linux/amd64 with docker buildx,
# make and helm), pushing to the registry CONTAINER_REGISTRY_PROVIDER in .env names, so no image
# leaves this machine. Publishes HEAD under its short SHA, the tag localdev/lib/image-tag.sh gives
# a clean tree, so the tree must be clean.
#
#   --import     also import every image into the k3s containerd of BUILDER, so its jobs pull nothing
#   SSH_OPTIONS  more options for ssh, such as -F <config> for a host reached through a proxy
#
# The registry token is minted here (gcloud, as GCP_ACCOUNT of .env), lives an hour, and reaches
# BUILDER only on ssh's stdin. BUILDER logs in with a docker config of its own and deletes it after.
set -euo pipefail

usage="usage: images-remote.sh BUILDER [--import]"
builder="${1:?${usage}}"
import="${2:-}"
[[ -z "${import}" || "${import}" == --import ]] || { echo "${usage}" >&2; exit 1; }
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo}"
[[ -f .env ]] || { echo "images-remote: ${repo}/.env is missing; copy .env.example and fill it in" >&2; exit 1; }
. localdev/lib/auth.sh
[[ -n "${HL_REGISTRY}" ]] || { echo "images-remote: .env names no CONTAINER_REGISTRY_PROVIDER to push to" >&2; exit 1; }
account="$(lib_env_value GCP_ACCOUNT)"
[[ -n "${account}" ]] || { echo "images-remote: .env does not set GCP_ACCOUNT, the account that pushes" >&2; exit 1; }

tag="$(bash localdev/lib/image-tag.sh)"
if [[ "${tag}" != "$(git rev-parse --short HEAD)" ]]; then
  echo "images-remote: the tree has changes HEAD does not hold, and only HEAD is sent to ${builder}. Commit them first" >&2
  exit 1
fi

remote_dir="hl-build/$(id -un)"
read -r -a ssh <<<"ssh -o BatchMode=yes ${SSH_OPTIONS:-}"

started="${SECONDS}"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
git archive HEAD -- . ':!docs' | tar -x -C "${work}"
echo "==> sending HEAD (${tag}) to ${builder}:${remote_dir}/src"
COPYFILE_DISABLE=1 tar -cz --no-xattrs -C "${work}" . | "${ssh[@]}" "${builder}" "rm -rf ${remote_dir}/src && mkdir -p ${remote_dir}/src && tar -xz -C ${remote_dir}/src"

echo "==> make publish on ${builder}"
gcloud auth print-access-token --account "${account}" \
  | "${ssh[@]}" "${builder}" "bash ${remote_dir}/src/localdev/images-remote-box.sh $(printf '%q' "${HL_REGISTRY}") ${tag} ${import}"

echo "images-remote: published ${HL_REGISTRY}/hypothesisloop-*:${tag} and its charts in $((SECONDS - started))s. To run on it, set IMAGE_TAG='${tag}' in .env"
