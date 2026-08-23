.PHONY: up down reset build test e2e-py lint images sparse-sdpa-workload-image \
	experimentator-base-image experimentator-image check-clean-tree registry-up registry-prune \
	helm-prepare helm-push helm-lint helm-template \
	controlplane-up controlplane-down controlplane-destroy render-settings \
	cluster-agent-up cluster-agent-down \
	k3s-up k3s-down full-up k3s-dev-nodes-up k3s-dev-nodes-down k3s-e2e \
	full-stop full-start reload \
	tt-up tt-down tt-status tt-stop tt-start tt-dev-nodes-up tt-dev-nodes-down tt-hardware-image tt-e2e tt-reload \
	git-daemon-start git-daemon-stop git-daemon-status git-daemon-destroy git-daemon-test

# ---- Control plane: one instance, runs anywhere (the brain) -----------------
COMPOSE_FILE := localdev/controlplane/docker-compose.yml
RENDERED_SETTINGS := controlplane/settings/.hypothesisloop.rendered.yaml
REGISTRY_HOST_IP_FILE := localdev/.registry-host-ip

# In production REGISTRY names a real registry the operator provisioned and every cluster
# node already has network access to (see runtime/k8s/infra/install.sh's REGISTRY_URL) --
# this default is dev-only. TAG is the git SHA of the tree that was actually built: content-
# addressed, so a manifest that pins one tag can never silently start running different bytes
# under imagePullPolicy: IfNotPresent. REGISTRY_TLS_VERIFY=false only because the dev registry
# in docker-compose.yml serves plain HTTP; a prod REGISTRY with real TLS overrides it to true.
REGISTRY ?= localhost:5000
GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null)
TAG ?= $(GIT_SHA)
REGISTRY_TLS_VERIFY ?= false

# Refuses a dirty tree rather than tagging a build with an ambiguous `-dirty` suffix: a mutable
# tag defeats the whole point of pinning DaemonSet/Deployment/Job manifests to a SHA, since two
# builds from a dirty tree at different times could tag identically while differing in bytes.
check-clean-tree:
	@test -n "$(GIT_SHA)" || { echo "make: not a git repository -- can't compute an image TAG" >&2; exit 1; }
	@git diff --quiet && git diff --cached --quiet || { \
		echo "make: git tree is dirty -- commit or stash before building images (make images)." >&2; \
		echo "  A tag built from a dirty tree can't be trusted to mean one fixed set of bytes." >&2; \
		exit 1; \
	}

# data_store.endpoint in controlplane/settings/hypothesisloop.yaml carries a
# REPLACE-WITH-HOST-LAN-IP placeholder rather than an address, because a k3s job pod has its own
# network namespace and can't reach the host's loopback -- the real address is specific to
# whichever machine runs this stack, and baking one host's address into a tracked file breaks it
# for every other. This renders the real address in at start-up; the control plane rejects a
# loopback endpoint outright, so a host where detection picks the wrong interface fails here,
# not hours into a run with nothing saved.
#
# The same detected address is written to REGISTRY_HOST_IP_FILE for the local registry: a k3s
# node container reaches the compose-published registry the same way it reaches the data store
# -- over the host's LAN address, never localhost/127.0.0.1 -- and this must stay the one place
# that detection happens (see localdev/lib/node.sh's registries.yaml comment).
render-settings:
	@host_ip=$$(ip -4 addr show 2>/dev/null | awk '/inet /{print $$2}' | \
		grep -vE '^(127\.|10\.88\.|172\.1[6-9]\.|172\.2[0-9]\.|172\.3[01]\.)' | head -1 | cut -d/ -f1); \
	if [ -z "$$host_ip" ]; then \
		echo "make render-settings: could not detect a host LAN address for data_store.endpoint." >&2; \
		echo "  Set it by hand in controlplane/settings/hypothesisloop.yaml (see the comment there)." >&2; \
		exit 1; \
	fi; \
	sed "s|REPLACE-WITH-HOST-LAN-IP|$$host_ip|g" controlplane/settings/hypothesisloop.yaml > $(RENDERED_SETTINGS); \
	echo "$$host_ip" > $(REGISTRY_HOST_IP_FILE); \
	echo "make render-settings: data_store.endpoint and registry host rendered against $$host_ip"

registry-up:
	podman compose -f $(COMPOSE_FILE) up -d registry
	@until curl -fsS -o /dev/null http://localhost:5000/v2/; do sleep 1; done

registry-prune:
	bash localdev/controlplane/registry-prune.sh

controlplane-up: registry-up render-settings images
	TAG=$(TAG) podman compose -f $(COMPOSE_FILE) up -d
	@until curl -fsS -o /dev/null http://localhost:8081/health; do sleep 1; done
	@until curl -fsS -o /dev/null http://localhost:8084/health; do sleep 1; done

# Stops and removes the control-plane containers but KEEPS the data volumes, so `down` + `up` is
# always safe. To delete the data too, ask for it by name: `make controlplane-destroy`.
controlplane-down:
	podman compose -f $(COMPOSE_FILE) down

# Irreversible: deletes the postgres + GreptimeDB + MinIO volumes (every platform experiment,
# experiment record, metric, hypothesis and stored checkpoint). Prompts unless CONFIRM=--yes.
controlplane-destroy:
	@if [ "$(CONFIRM)" != "--yes" ]; then \
		echo "make controlplane-destroy: this DELETES ALL control-plane data. There is no backup and no undo." >&2; \
		echo "  Re-run with CONFIRM=--yes to proceed." >&2; \
		exit 1; \
	fi
	podman compose -f $(COMPOSE_FILE) down --volumes

# Back-compat aliases for the old target names.
up: controlplane-up
down: controlplane-down

reset:
	$(MAKE) controlplane-down
	$(MAKE) controlplane-up

# ---- Cluster agent: one per target Kubernetes cluster running jobs ----------
# Installs/removes the node-agent DaemonSet + cluster-agent Deployment (the
# only component with real k8s credentials) on the cluster KUBE_CONTEXT/
# KUBECONFIG_PATH point at (or current kubectl context).
# Usage: make cluster-agent-up CLUSTER=<name> [KUBECONFIG_PATH=...] [KUBE_CONTEXT=...] [API_URL=...]
cluster-agent-up:
	CLUSTER_NAME="$(CLUSTER)" bash runtime/k8s/infra/install.sh

cluster-agent-down:
	CLUSTER_NAME="$(CLUSTER)" bash runtime/k8s/infra/destroy.sh

# ---- Local dev cluster bootstrap --------------------------------------------
# Spins up a local k3s cluster (control-plane only, tainted no-workload — see install.sh), then
# installs the cluster-agent bundle onto it (localdev/k3s-macos/install.sh calls
# runtime/k8s/infra/install.sh itself) and provisions the dev nodes (see k3s-dev-nodes-up below),
# so this target alone produces a fully working local dev cluster. Set NODE_COUNT=0 to stay
# control-plane only.
k3s-up: images
	bash localdev/k3s-macos/install.sh

# Unlike controlplane-down, this one IS destructive: the k3s cluster's own state (etcd/sqlite,
# every Job/Deployment/Pod it knows about) lives only inside the k3s node itself, so tearing the
# node down is the whole cluster's `destroy`, not a `down`. No confirmation prompt today — the
# cluster is ephemeral local-dev infra, not the durable postgres/GreptimeDB data controlplane-down
# protects. Use `make full-stop` / `full-start` for a same-state pause/resume instead.
k3s-down:
	bash localdev/k3s-macos/destroy.sh

# Provisions (or re-syncs) the dev node containers — idempotent, safe to call again after
# a VM restart killed the background agent containers, without redoing the whole bootstrap.
# These same nodes serve both interactive dev work and the portable e2e suite (k3s-e2e below).
# Usage: make k3s-dev-nodes-up [NODE_COUNT=4]
k3s-dev-nodes-up:
	NODE_COUNT="$(NODE_COUNT)" bash localdev/k3s-macos/dev-nodes-up.sh

# Tears the dev nodes back down, returning the cluster to control-plane-only.
k3s-dev-nodes-down:
	bash localdev/k3s-macos/dev-nodes-down.sh

# Provisions dev nodes, runs the portable e2e suite, tears them back down — pass/fail either way.
k3s-e2e:
	bash localdev/k3s-macos/run-e2e.sh

# ---- Convenience: local cluster + control plane in one command -------------
full-up: k3s-up controlplane-up

# Pause/resume everything (podman machine, k3s, control plane) without destroying
# cluster state — much faster than full-up/k3s-down for a daily on/off cycle.
full-stop:
	bash localdev/k3s-macos/stop.sh

full-start:
	bash localdev/k3s-macos/start.sh

# Rebuild every image from current source and push it into every place that caches one
# (podman store, k3s server, each dev node container), then bounce the control-plane
# containers and cluster-agent/node-agent pods so they run it. Use this after a Go change,
# before re-running the e2e suites — faster than a manual images+import+restart chain since
# every step polls for readiness instead of sleeping a fixed guess.
reload:
	bash localdev/k3s-macos/reload.sh

# ---- Real Tenstorrent hardware: k3s + tt-operator device stack --------------
# Counterpart to k3s-up, for an actual Tenstorrent host instead of simulated
# fake-accelerator nodes. Also control-plane-only by default (tainted no-workload) — see
# tt-dev-nodes-up below. See localdev/k3s-tenstorrent-qb2/README.md.
tt-up: images
	bash localdev/k3s-tenstorrent-qb2/install.sh

# Destructive the same way k3s-down is (see its comment) — tears down the whole k3s cluster on
# this Tenstorrent host, not just this repo's containers. Use `make tt-stop`/`tt-start` to
# pause/resume without losing cluster state.
tt-down:
	bash localdev/k3s-tenstorrent-qb2/destroy.sh

# Attaches this host's own node as a full worker (no fake containers — a real Tenstorrent box
# is the one node the portable and hardware e2e suites both need, and the one node real serving/
# training work runs on). Idempotent.
tt-dev-nodes-up:
	bash localdev/k3s-tenstorrent-qb2/dev-nodes-up.sh

# Builds and pushes the Tenstorrent hardware workload image to $(REGISTRY) -- the tt-quietbox
# k3s node pulls it normally (registries.yaml, see localdev/lib/node.sh), same as every other
# image; no more local save/import.
tt-hardware-image: check-clean-tree
	podman build -f tests/workloads/tenstorrent/Dockerfile.train -t $(REGISTRY)/hypothesisloop-tenstorrent-workload:$(TAG) tests/workloads/tenstorrent/
	podman tag $(REGISTRY)/hypothesisloop-tenstorrent-workload:$(TAG) $(REGISTRY)/hypothesisloop-tenstorrent-workload:latest
	podman push --tls-verify=$(REGISTRY_TLS_VERIFY) $(REGISTRY)/hypothesisloop-tenstorrent-workload:$(TAG)
	podman push --tls-verify=$(REGISTRY_TLS_VERIFY) $(REGISTRY)/hypothesisloop-tenstorrent-workload:latest

tt-e2e: tt-hardware-image
	bash localdev/k3s-tenstorrent-qb2/run-e2e.sh

# Counterpart to `reload` for the real Tenstorrent host: rebuild every image, import into
# k3s's own containerd (native on Linux, no podman-machine hop), and bounce cluster-agent/
# node-agent so they run current code.
tt-reload:
	bash localdev/k3s-tenstorrent-qb2/reload.sh

# Detaches it again, returning the cluster to control-plane-only so real training work outside
# k3s isn't sharing capacity with anything scheduled onto it.
tt-dev-nodes-down:
	bash localdev/k3s-tenstorrent-qb2/dev-nodes-down.sh

tt-status:
	bash localdev/k3s-tenstorrent-qb2/status.sh

# ---- git:// daemon: serves per-experiment code repos to experimentator agent containers ----------
# Always restarts (not just "start if not running") and always passes --enable=receive-pack --
# see localdev/local-git/git-daemon.sh's header comment for why both matter. Run `start` again any time a new
# experiment repo is created under the daemon's base-path, since a running daemon won't pick it up on
# its own.
git-daemon-start:
	bash localdev/local-git/git-daemon.sh start

git-daemon-stop:
	bash localdev/local-git/git-daemon.sh stop

git-daemon-status:
	bash localdev/local-git/git-daemon.sh status

git-daemon-destroy:
	bash localdev/local-git/git-daemon.sh destroy

tt-stop:
	bash localdev/k3s-tenstorrent-qb2/stop.sh

tt-start:
	bash localdev/k3s-tenstorrent-qb2/start.sh

# Builds every platform image and pushes it to $(REGISTRY) under two tags: $(TAG) (the git SHA
# that produced it -- the one every manifest pins) and latest (a mutable convenience pointer for
# interactive dev, never referenced by a manifest). Every consumer (k3s nodes via
# registries.yaml, the Helm chart's images.registry) pulls normally instead of the old
# podman-save-pipe-ctr-import sideload, which is what left an unpinned image ID for kubelet's
# own image GC to reclaim mid-run (see the fix-later.md incident this replaced).
IMAGE_TARGETS := control-service:controlplane/build/Dockerfile.control-service:. \
	metrics-service:controlplane/build/Dockerfile.metrics-service:. \
	node-agent:runtime/k8s/build/Dockerfile.node-agent:. \
	cluster-agent:runtime/k8s/build/Dockerfile.cluster-agent:. \
	workload:tests/workloads/generic/Dockerfile.train:tests/workloads/generic

images: check-clean-tree registry-up
	@for entry in $(IMAGE_TARGETS); do \
		name="$${entry%%:*}"; rest="$${entry#*:}"; dockerfile="$${rest%%:*}"; ctx="$${rest#*:}"; \
		echo "==> building hypothesisloop-$$name:$(TAG)"; \
		podman build -f "$$dockerfile" -t "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" "$$ctx" || exit 1; \
		podman tag "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" "$(REGISTRY)/hypothesisloop-$$name:latest"; \
		podman push --tls-verify=$(REGISTRY_TLS_VERIFY) "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" || exit 1; \
		podman push --tls-verify=$(REGISTRY_TLS_VERIFY) "$(REGISTRY)/hypothesisloop-$$name:latest" || exit 1; \
	done

# Shared base every experiment's own Dockerfile.experimentator FROMs (see
# agents/experimentator/Dockerfile.base's header) -- the agent harness, git/Claude Code plumbing,
# and WORKLOAD_SAMPLES, none of which is experiment-specific. Root build context (not
# agents/experimentator/) so it can bake in agents/coordinator/experiments as WORKLOAD_SAMPLES.
experimentator-base-image:
	podman build -f agents/experimentator/Dockerfile.base -t localhost/hypothesisloop-experimentator-base .

# Builds one experiment's own experimentator image on top of the base -- e.g.
# `make experimentator-image EXPERIMENT=sparse-sdpa`. Each experiment owns its Dockerfile.experimentator
# (agents/coordinator/experiments/<name>/Dockerfile.experimentator) for whatever runtime source/pins
# its agent needs to read (tt-metal today; nothing here assumes that's the only case).
experimentator-image: experimentator-base-image
	test -n "$(EXPERIMENT)" || { echo "usage: make experimentator-image EXPERIMENT=<name>" >&2; exit 1; }
	test -f agents/coordinator/experiments/$(EXPERIMENT)/Dockerfile.experimentator || \
		{ echo "no agents/coordinator/experiments/$(EXPERIMENT)/Dockerfile.experimentator" >&2; exit 1; }
	podman build -f agents/coordinator/experiments/$(EXPERIMENT)/Dockerfile.experimentator \
		-t localhost/hypothesisloop-experimentator-$(EXPERIMENT) agents/coordinator/experiments/$(EXPERIMENT)

# Pre-builds the sparse-sdpa experiment's own job image (harness.py + torch + pinned upstream
# sparse_sdpa test-utils baked in -- see agents/coordinator/experiments/sparse-sdpa/seed/Dockerfile.workload)
# so every job an agent submits starts instantly with zero per-job setup. Run this once before
# starting a sparse-sdpa platform experiment, same as experimentator-image.
sparse-sdpa-workload-image: check-clean-tree registry-up
	podman build -f agents/coordinator/experiments/sparse-sdpa/seed/Dockerfile.workload \
		-t $(REGISTRY)/hypothesisloop-sparse-sdpa-workload:$(TAG) agents/coordinator/experiments/sparse-sdpa/seed/
	podman tag $(REGISTRY)/hypothesisloop-sparse-sdpa-workload:$(TAG) $(REGISTRY)/hypothesisloop-sparse-sdpa-workload:latest
	podman push --tls-verify=$(REGISTRY_TLS_VERIFY) $(REGISTRY)/hypothesisloop-sparse-sdpa-workload:$(TAG)
	podman push --tls-verify=$(REGISTRY_TLS_VERIFY) $(REGISTRY)/hypothesisloop-sparse-sdpa-workload:latest

# Stages the one file the helm chart renders verbatim (never edited in the chart itself, so
# there is exactly one source of truth for platform config) — run before any helm
# lint/template/install/upgrade. schema.sql is NOT staged here: control-service/metrics-service
# each bake it into their own image and self-apply it on startup (db.ApplySchema) — see
# controlplane/build/Dockerfile.control-service.
HELM_CHART := controlplane/infra/helm/hypothesisloop
helm-prepare:
	cp controlplane/settings/hypothesisloop.yaml $(HELM_CHART)/files/hypothesisloop.yaml

helm-lint: helm-prepare
	helm lint $(HELM_CHART)

helm-template: helm-prepare
	helm template hypothesisloop $(HELM_CHART)

# `make images` already builds and pushes every image the chart references (control-service,
# metrics-service, cluster-agent, node-agent) to $(REGISTRY):$(TAG) -- this is only a named
# alias so a helm install/upgrade reads as `make helm-push && helm upgrade --install ...
# --set images.registry=$(REGISTRY) --set images.tag=$(TAG)`, the two values images already
# computed for you.
helm-push: images

build:
	go build ./...

test:
	go test ./... -timeout 60s

# Portable e2e suite (pytest), API-only/parallel lane -- fast local loop. See tests/improve.md for
# the marker scheme (parallel/exclusive/slow/hardware/accelerator) and the migration this replaces.
e2e-py:
	cd tests && uv run pytest e2e -m "not exclusive and not slow and not hardware"

lint:
	golangci-lint run ./...
