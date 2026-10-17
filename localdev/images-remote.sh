#!/usr/bin/env bash
# images-remote.sh BUILDER [--import] [JOB_IMAGE_BASE...]
#
# Builds the images a hosted run uses on the machine BUILDER names (an ssh host, linux/amd64 with
# docker buildx) and pushes them from there to the registry CONTAINER_REGISTRY_PROVIDER in .env
# names, so no image leaves this machine. Builds HEAD under its short SHA, the tag
# localdev/lib/image-tag.sh gives a clean tree, so the tree must be clean. An image the registry
# already holds under that tag is not built again.
#
#   IMAGES    the images to build, by name (default: the agent image, the job images and the
#             runtimes a cluster's attach command installs, and the runtime chart)
#   --import  also import every image into the k3s containerd of BUILDER, so its jobs pull nothing
#   JOB_IMAGE_BASE  name=base[=torch] (runtime/job/build/bases.mk): a framework job image to add
#   SSH_OPTIONS  more options for ssh, such as -F <config> for a host reached through a proxy
#
# The registry token is minted here (gcloud, as GCP_ACCOUNT of .env), lives an hour, and reaches
# BUILDER only on ssh's stdin. BUILDER logs in with a docker config of its own and deletes it after.
set -euo pipefail

usage="usage: images-remote.sh BUILDER [--import] [JOB_IMAGE_BASE...]"
builder="${1:?${usage}}"
shift
import=false
if [[ "${1:-}" == "--import" ]]; then
  import=true
  shift
fi
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

read -r -a images <<<"${IMAGES:-experimentator-base job-base job cluster-agent node-agent bare-agent chart}"
for entry in "$@"; do
  images+=("${entry%%=*}-base" "${entry%%=*}")
done
remote_dir="hl-build/$(id -un)"
box="${remote_dir}/src/localdev/images-remote-box.sh"
read -r -a ssh <<<"ssh -o BatchMode=yes ${SSH_OPTIONS:-}"

# ssh_box COMMAND...: runs the box script with a registry token as the first line of stdin. gcloud
# hands out the token it holds until that one nearly expires, so each long step asks again.
ssh_box() {
  local token
  token="$(gcloud auth print-access-token --account "${account}")"
  "${ssh[@]}" "${builder}" "bash ${box} $*" <<<"${token}"
}

started="${SECONDS}"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
# docs/ and the UI are in no image built here.
git archive HEAD -- . ':!docs' ':!controlplane/ui' | tar -x -C "${work}"
echo "==> sending HEAD ($(git rev-parse --short HEAD)) to ${builder}:${remote_dir}/src"
COPYFILE_DISABLE=1 tar -cz --no-xattrs -C "${work}" . | "${ssh[@]}" "${builder}" "rm -rf ${remote_dir}/src && mkdir -p ${remote_dir}/src && tar -xz -C ${remote_dir}/src"

echo "==> asking ${HL_REGISTRY} which of ${images[*]} it lacks under ${tag}"
listing="$(ssh_box missing "${HL_REGISTRY}" "${tag}" "${images[@]}")"
missing=()
[[ -z "${listing}" ]] || read -r -a missing <<<"${listing//$'\n'/ }"
results="${work}/results"
: >"${results}"
if [[ ${#missing[@]} -gt 0 ]]; then
  echo "==> building on ${builder}: ${missing[*]}"
  ssh_box build "${HL_REGISTRY}" "${tag}" "'$*'" "${missing[@]}" | tee -a "${results}"
  echo "==> pushing from ${builder}: ${missing[*]}"
  for name in "${missing[@]}"; do
    ssh_box push "${HL_REGISTRY}" "${tag}" "${name}" | tee -a "${results}"
  done
fi
if [[ "${import}" == true ]]; then
  echo "==> importing into the k3s containerd of ${builder}"
  ssh_box import "${HL_REGISTRY}" "${tag}" "${images[@]}" | tee -a "${results}"
fi

echo
printf '%-22s %-10s %-8s %9s %8s\n' IMAGE TAG RESULT SIZE BUILD
for name in "${images[@]}"; do
  if [[ " ${missing[*]-} " == *" ${name} "* ]]; then
    size="$(awk -v n="${name}" '$1 == "SIZE" && $2 == n { print $3 }' "${results}")"
    built="$(awk -v n="${name}" '$1 == "BUILT" && $2 == n { print $3 "s" }' "${results}")"
    printf '%-22s %-10s %-8s %9s %8s\n' "${name}" "${tag}" pushed "${size:--}" "${built:--}"
  else
    printf '%-22s %-10s %-8s %9s %8s\n' "${name}" "${tag}" skipped - -
  fi
done
echo
echo "images-remote: ${HL_REGISTRY}/hypothesisloop-*:${tag} in $((SECONDS - started))s. To run on it, set IMAGE_TAG='${tag}' in .env"
