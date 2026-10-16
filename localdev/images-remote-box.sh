#!/usr/bin/env bash
# images-remote-box.sh missing|build|push|import REGISTRY TAG ... : the builder's half of
# localdev/images-remote.sh, run on the builder from the tree that script sent. The first line of
# stdin is a registry access token. It is used only through a docker config and helm registry
# config of this run's own, deleted when the run ends, so the builder's own logins are untouched.
#
#   missing REGISTRY TAG NAME...                   the names the registry holds nothing under TAG for
#   build   REGISTRY TAG SAMPLES JOB_BASES NAME... builds each for linux/amd64 into docker's store
#   push    REGISTRY TAG NAME...                   pushes each
#   import  REGISTRY TAG NAME...                   imports each into k3s's containerd (sudo)
#
# chart is the runtime chart, pushed as the root Makefile's chart-push does.
set -euo pipefail

command="${1:?usage: images-remote-box.sh missing|build|push|import REGISTRY TAG ...}"
registry="${2:?registry}"
tag="${3:?tag}"
shift 3
src="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
build_dir="$(dirname "${src}")"
cd "${src}"
platform=linux/amd64
chart_repository=charts/hypothesisloop-cluster-agent
chart_version="0.1.0+${tag}"

IFS= read -r token
session="$(mktemp -d)"
trap 'docker --config "${session}/docker" logout "${registry%%/*}" >/dev/null 2>&1 || true; rm -rf "${session}"' EXIT
mkdir -p "${session}/docker"
docker --config "${session}/docker" login --username oauth2accesstoken --password-stdin "${registry%%/*}" <<<"${token}" >"${session}/login" 2>&1 \
  || { cat "${session}/login" >&2; exit 1; }
helm registry login --registry-config "${session}/helm.json" --username oauth2accesstoken --password-stdin "${registry%%/*}" <<<"${token}" >"${session}/login" 2>&1 \
  || { cat "${session}/login" >&2; exit 1; }

image() {
  echo "${registry}/hypothesisloop-$1:${tag}"
}

# spec NAME: the dockerfile, context and build arguments of the image NAME, or a failure.
spec() {
  args=() context=. dockerfile=""
  case "$1" in
    experimentator-base) dockerfile=agents/experimentator/Dockerfile.base; args=(--build-arg "EXPERIMENT_SAMPLES=${samples}") ;;
    job-base) dockerfile=infra/images/job/Dockerfile.base ;;
    job) dockerfile=infra/images/job/Dockerfile; args=(--build-arg "BASE=$(image job-base)") ;;
    control-service | metrics-service) dockerfile="controlplane/build/Dockerfile.$1" ;;
    cluster-agent | node-agent) dockerfile="runtime/k8s/build/Dockerfile.$1"; args=(--build-arg "VERSION=${tag}") ;;
    bare-agent) dockerfile=runtime/bare-metal/build/Dockerfile.bare-agent; args=(--build-arg "VERSION=${tag}") ;;
    cloud-agent | agent-launcher) dockerfile="runtime/cloud/build/Dockerfile.$1"; args=(--build-arg "VERSION=${tag}") ;;
    workload) dockerfile=tests/workloads/generic/Dockerfile.train; context=tests/workloads/generic ;;
    *)
      local entry base_name base torch
      for entry in ${job_bases}; do
        IFS='=' read -r base_name base torch <<<"${entry}"
        if [[ "$1" == "${base_name}-base" ]]; then
          dockerfile=infra/images/job/Dockerfile.base; args=(--build-arg "BASE=${base}" --build-arg "TORCH_CPU=${torch}")
        elif [[ "$1" == "${base_name}" ]]; then
          dockerfile=infra/images/job/Dockerfile; args=(--build-arg "BASE=$(image "${base_name}-base")")
        fi
      done
      ;;
  esac
  [[ -n "${dockerfile}" ]] || { echo "images-remote-box: no image is named $1" >&2; exit 1; }
}

# manifest_status REPOSITORY REFERENCE: the HTTP status of the registry's manifest of it.
manifest_status() {
  local host="${registry%%/*}" path="${registry#*/}"
  curl -sS -o "${session}/body" -w '%{http_code}' -H "Authorization: Bearer ${token}" \
    -H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.docker.distribution.manifest.list.v2+json' \
    "https://${host}/v2/${path}/$1/manifests/$2"
}

case "${command}" in
  missing)
    for name in "$@"; do
      if [[ "${name}" == chart ]]; then
        status="$(manifest_status "${chart_repository}" "${chart_version//+/_}")"
      else
        status="$(manifest_status "hypothesisloop-${name}" "${tag}")"
      fi
      case "${status}" in
        200) ;;
        404) echo "${name}" ;;
        *) echo "images-remote-box: ${registry} answered ${status} for ${name}: $(head -c 500 "${session}/body")" >&2; exit 1 ;;
      esac
    done
    ;;
  build)
    samples="$1" job_bases="$2"
    shift 2
    # The Go images copy a vendor/ that is not tracked. Made with the toolchain go.mod names, in a
    # container, since the builder's own go may be older.
    if [[ " $* " =~ \ (cluster-agent|node-agent|bare-agent|cloud-agent|agent-launcher|control-service|metrics-service)\  ]]; then
      mkdir -p "${build_dir}/go-cache"
      docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e GOMODCACHE=/cache/mod -e GOCACHE=/cache/build -e GOFLAGS=-modcacherw \
        -v "${build_dir}/go-cache:/cache" -v "${src}:/src" -w /src golang:1.25-bookworm go mod vendor
    fi
    for name in "$@"; do
      [[ "${name}" == chart ]] || spec "${name}"
    done
    for name in "$@"; do
      [[ "${name}" != chart ]] || continue
      spec "${name}"
      echo "==> building $(image "${name}")" >&2
      started="${SECONDS}"
      DOCKER_CONFIG="${session}/docker" docker buildx build --builder default --platform "${platform}" --load --progress plain \
        ${args[@]+"${args[@]}"} -f "${dockerfile}" -t "$(image "${name}")" "${context}" >&2
      echo "BUILT ${name} $((SECONDS - started))"
    done
    ;;
  push)
    for name in "$@"; do
      if [[ "${name}" == chart ]]; then
        helm package runtime/k8s/infra/chart --version "${chart_version}" --app-version "${tag}" --destination "${session}" >/dev/null
        helm push --registry-config "${session}/helm.json" "${session}"/hypothesisloop-cluster-agent-*.tgz "oci://${registry}/charts" >&2
        echo "SIZE chart $(du -k "${session}"/hypothesisloop-cluster-agent-*.tgz | cut -f1)K"
        continue
      fi
      echo "==> pushing $(image "${name}")" >&2
      started="${SECONDS}"
      docker --config "${session}/docker" push --quiet "$(image "${name}")" >&2
      echo "PUSHED ${name} $((SECONDS - started))"
      echo "SIZE ${name} $(docker image inspect --format '{{.Size}}' "$(image "${name}")" | numfmt --to=iec)"
    done
    ;;
  import)
    for name in "$@"; do
      [[ "${name}" != chart ]] || continue
      if ! docker image inspect "$(image "${name}")" >/dev/null 2>&1; then
        docker --config "${session}/docker" pull --quiet --platform "${platform}" "$(image "${name}")" >&2
      fi
      echo "==> importing $(image "${name}") into k3s" >&2
      docker save "$(image "${name}")" | sudo -n k3s ctr -n k8s.io images import - >&2
      echo "IMPORTED ${name}"
    done
    ;;
  *)
    echo "images-remote-box: no command ${command}" >&2
    exit 1
    ;;
esac
