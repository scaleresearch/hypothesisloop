"""prune-images.py [--dry-run] [TAG...]

Keeps one build of this project's images on this machine: in podman's image store and in the dev
registry, every tag of a hypothesisloop image is removed except the ones in use.

In use are: the tag `make local-images` last pushed to the dev registry (localdev/.image-tag), the tag
the local cluster's runtime is installed with, the deployed tag (IMAGE_TAG of .env), every TAG
named here (a build names the one it just pushed). The chart is tagged
<version>_<tag> and follows its tag.

Nothing else is touched: an image that is not this project's, an image a container was created
from, and a repository that holds none of the tags in use (a hand-built image has no build to
replace it).
"""
import json
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
REGISTRY_CONTAINER = "controlplane-registry-1"
MANIFESTS = ("application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, "
             "application/vnd.oci.image.index.v1+json")


def run(*cmd: str, check: bool = True) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, capture_output=True, text=True, check=check)


def ours(repository: str) -> bool:
    return repository.rsplit("/", 1)[-1].startswith("hypothesisloop-")


def tags_in_use(named: list[str]) -> set[str]:
    in_use = set(named)
    pushed = REPO / "localdev" / ".image-tag"
    if pushed.exists():
        in_use.add(pushed.read_text().strip())
    deployed = run("bash", "-c", f'. "{REPO}/localdev/lib/auth.sh" && [[ ! -f "{REPO}/.env" ]] || lib_env_value IMAGE_TAG').stdout.strip()
    if deployed:
        in_use.add(deployed)
    # A cluster that is stopped pulls nothing, so its answer is only taken when it gives one.
    installed = run("kubectl", "--context", "k3s-local", "--request-timeout=5s", "-n", "hypothesisloop", "get",
                    "deployment/hypothesisloop-cluster-agent", "-o", "jsonpath={.spec.template.spec.containers[0].image}", check=False)
    if installed.returncode == 0 and ":" in installed.stdout:
        in_use.add(installed.stdout.rsplit(":", 1)[1])
    return in_use


def kept(tag: str, in_use: set[str]) -> bool:
    return tag in in_use or any(tag.endswith("_" + t) for t in in_use)


def prune_store(in_use: set[str], dry_run: bool) -> None:
    held = set(run("podman", "ps", "-a", "--no-trunc", "--format", "{{.ImageID}}").stdout.split())
    by_repository: dict[str, set[tuple[str, str]]] = {}
    for image in json.loads(run("podman", "images", "--no-trunc", "--format", "json").stdout):
        for name in image.get("Names") or []:
            repository, _, tag = name.rpartition(":")
            if ours(repository):
                by_repository.setdefault(repository, set()).add((tag, image["Id"].removeprefix("sha256:")))
    removed = 0
    for repository, tags in sorted(by_repository.items()):
        if not any(kept(tag, in_use) for tag, _ in tags):
            continue
        for tag, image_id in sorted(tags):
            if kept(tag, in_use) or image_id in held:
                continue
            removed += 1
            if not dry_run:
                run("podman", "rmi", f"{repository}:{tag}")
    if not dry_run:
        run("podman", "image", "prune", "--force")
    print(f"prune-images: {'would remove' if dry_run else 'removed'} {removed} tags of {len(by_repository)} repositories from the image store")


def registry(method: str, address: str, path: str) -> tuple[dict, bytes]:
    request = urllib.request.Request(f"http://{address}/v2/{path}", method=method, headers={"Accept": MANIFESTS})
    with urllib.request.urlopen(request, timeout=30) as answer:
        return dict(answer.headers), answer.read()


def prune_registry(in_use: set[str], dry_run: bool) -> None:
    address = run("bash", "-c", f'. "{REPO}/localdev/lib/node.sh" && lib_registry_publish_ip').stdout.strip() + ":5000"
    try:
        _, catalog = registry("GET", address, "_catalog?n=1000")
    except urllib.error.URLError:
        print(f"prune-images: no dev registry answers at {address}, nothing to remove there")
        return
    removed = 0
    for repository in json.loads(catalog)["repositories"]:
        if not ours(repository):
            continue
        _, listed = registry("GET", address, f"{repository}/tags/list")
        digests = {}
        for tag in json.loads(listed).get("tags") or []:
            headers, _ = registry("HEAD", address, f"{repository}/manifests/{tag}")
            digests[tag] = headers["Docker-Content-Digest"]
        if not any(kept(tag, in_use) for tag in digests):
            continue
        # A manifest is deleted by its digest, with every tag on it, so one a kept tag names stays.
        keep = {digest for tag, digest in digests.items() if kept(tag, in_use)}
        for digest in sorted(set(digests.values()) - keep):
            removed += 1
            if not dry_run:
                registry("DELETE", address, f"{repository}/manifests/{digest}")
    print(f"prune-images: {'would remove' if dry_run else 'removed'} {removed} builds from the dev registry at {address}")
    if dry_run or not removed:
        return
    run("podman", "exec", REGISTRY_CONTAINER, "registry", "garbage-collect", "/etc/docker/registry/config.yml")
    # The registry remembers blobs it has seen and does not forget the ones its garbage
    # collection deleted: a push right after would skip uploading one and leave a manifest that
    # cannot be read. A restart is the only thing that clears that.
    run("podman", "restart", REGISTRY_CONTAINER)
    for _ in range(60):
        try:
            registry("GET", address, "")
            return
        except (urllib.error.URLError, ConnectionError):
            run("sleep", "1")
    sys.exit(f"prune-images: the dev registry at {address} did not come back after its restart")


args = sys.argv[1:]
dry_run = "--dry-run" in args
in_use = tags_in_use([a for a in args if a != "--dry-run"])
if not in_use:
    sys.exit("prune-images: no tag is in use on this machine yet, so there is nothing to keep and nothing is removed")
print(f"prune-images: in use: {' '.join(sorted(in_use))}")
prune_store(in_use, dry_run)
prune_registry(in_use, dry_run)
