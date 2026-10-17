#!/usr/bin/env bash
# images-remote-box.sh REGISTRY TAG [--import]: the builder's half of localdev/images-remote.sh,
# run on the builder from the tree that script sent. Runs make publish with docker, then with
# --import loads every image into k3s's containerd (sudo). The first line of stdin is a registry
# access token. It is used only through a docker config and helm registry config of this run's
# own, deleted when the run ends, so the builder's own logins are untouched. The UI image's build
# arguments come from the environment.
set -euo pipefail

registry="${1:?usage: images-remote-box.sh REGISTRY TAG [--import]}"
tag="${2:?tag}"
import="${3:-}"
src="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
build_dir="$(dirname "${src}")"
cd "${src}"

IFS= read -r token
session="$(mktemp -d)"
trap 'docker --config "${session}/docker" logout "${registry%%/*}" >/dev/null 2>&1 || true; rm -rf "${session}"' EXIT
mkdir -p "${session}/docker"
docker --config "${session}/docker" login --username oauth2accesstoken --password-stdin "${registry%%/*}" <<<"${token}" >"${session}/login" 2>&1 \
  || { cat "${session}/login" >&2; exit 1; }
helm registry login --registry-config "${session}/helm.json" --username oauth2accesstoken --password-stdin "${registry%%/*}" <<<"${token}" >"${session}/login" 2>&1 \
  || { cat "${session}/login" >&2; exit 1; }
export DOCKER_CONFIG="${session}/docker" HELM_REGISTRY_CONFIG="${session}/helm.json"

# The builder's own go may be older than the toolchain go.mod names, so vendor/ is made in a container.
mkdir -p "${build_dir}/go-cache"
vendor="docker run --rm -u $(id -u):$(id -g) -e HOME=/tmp -e GOMODCACHE=/cache/mod -e GOCACHE=/cache/build -e GOFLAGS=-modcacherw -v ${build_dir}/go-cache:/cache -v ${src}:/src -w /src golang:1.25-bookworm go mod vendor"
make publish REGISTRY="${registry}" TAG="${tag}" CONTAINER_ENGINE=docker IMAGE_PLATFORM=linux/amd64 GO_VENDOR="${vendor}"

if [[ "${import}" == --import ]]; then
  for name in $(make -s --no-print-directory product-images); do
    echo "==> importing ${registry}/hypothesisloop-${name}:${tag} into k3s" >&2
    docker save "${registry}/hypothesisloop-${name}:${tag}" | sudo -n k3s ctr -n k8s.io images import - >&2
  done
fi
