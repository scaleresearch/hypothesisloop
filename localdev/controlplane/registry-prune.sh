#!/usr/bin/env bash
# Deletes tags older than REGISTRY_PRUNE_DAYS (default 7) from the local dev registry, then runs
# the registry's own garbage-collect to reclaim the now-unreferenced blobs. "Age" is the image
# config blob's `created` field (the actual build time baked in by `podman build`), not a
# registry-side timestamp -- the filesystem storage driver's own mtimes get touched by
# unrelated GC/repair runs and can't be trusted as "when this tag was built".
set -euo pipefail

REGISTRY="${REGISTRY:-localhost:5000}"
REGISTRY_PRUNE_DAYS="${REGISTRY_PRUNE_DAYS:-7}"
REGISTRY_CONTAINER="${REGISTRY_CONTAINER:-controlplane-registry-1}"

cutoff_epoch=$(( $(date +%s) - REGISTRY_PRUNE_DAYS * 86400 ))

repositories=$(curl -fsS "http://${REGISTRY}/v2/_catalog" | jq -r '.repositories[]')

for repo in $repositories; do
  tags=$(curl -fsS "http://${REGISTRY}/v2/${repo}/tags/list" | jq -r '.tags[]? // empty')
  for tag in $tags; do
    # podman builds and pushes OCI manifests by default -- the plain docker.distribution accept
    # header alone gets MANIFEST_UNKNOWN back from registry:2 for those, so both media types are
    # offered and the registry picks whichever it actually stored.
    #
    # `latest` and the git-SHA tag this repo's own build targets push always share one digest
    # (Makefile tags both before pushing), so deleting the SHA tag as stale already removes
    # `latest`'s manifest too -- this tag is then legitimately gone by the time its own turn
    # comes up, not a failure, so a 404 here is skipped rather than aborting the whole run.
    http_code=$(curl -sS -o /tmp/manifest.$$ -D /tmp/headers.$$ -w '%{http_code}' \
      -H "Accept: application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json" \
      "http://${REGISTRY}/v2/${repo}/manifests/${tag}")
    if [ "$http_code" = "404" ]; then
      echo "registry-prune: ${repo}:${tag} already gone (shared digest with an already-deleted tag)"
      rm -f /tmp/manifest.$$ /tmp/headers.$$
      continue
    fi
    [ "$http_code" = "200" ] || { echo "registry-prune: unexpected $http_code fetching ${repo}:${tag}" >&2; exit 1; }
    manifest_digest=$(awk -F': ' 'tolower($1)=="docker-content-digest"{print $2}' /tmp/headers.$$ | tr -d '\r')
    config_digest=$(jq -r '.config.digest' /tmp/manifest.$$)
    rm -f /tmp/manifest.$$ /tmp/headers.$$

    created=$(curl -fsS "http://${REGISTRY}/v2/${repo}/blobs/${config_digest}" | jq -r '.created')
    created_epoch=$(date -d "$created" +%s)

    if [ "$created_epoch" -lt "$cutoff_epoch" ]; then
      echo "registry-prune: deleting ${repo}:${tag} (built ${created})"
      curl -fsS -X DELETE "http://${REGISTRY}/v2/${repo}/manifests/${manifest_digest}" -o /dev/null
    fi
  done
done

echo "registry-prune: running garbage-collect"
podman exec "$REGISTRY_CONTAINER" registry garbage-collect /etc/docker/registry/config.yml

# registry:2 caches blob descriptors in memory and does not invalidate that cache when
# garbage-collect deletes the underlying blobs -- a push right after GC can report success while
# silently skipping upload of a blob GC just removed, leaving a manifest that 404s on read.
# Restarting drops the cache; there is no in-process way to do this instead.
podman restart "$REGISTRY_CONTAINER" >/dev/null
until curl -fsS -o /dev/null "http://${REGISTRY}/v2/"; do sleep 1; done
