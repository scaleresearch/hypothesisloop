.PHONY: up down reset build test lint images check-experiment-seeds sparse-sdpa-workload-image \
	experimentator-base-image experimentator-image \
	controlplane-up controlplane-down controlplane-destroy \
	cluster-agent-up cluster-agent-down \
	k3s-up k3s-down full-up k3s-dev-nodes-up k3s-dev-nodes-down k3s-e2e \
	full-stop full-start reload \
	tt-up tt-down tt-status tt-stop tt-start tt-dev-nodes-up tt-dev-nodes-down tt-hardware-image tt-e2e tt-reload \
	git-daemon-start git-daemon-stop git-daemon-status git-daemon-destroy git-daemon-test

# ---- Control plane: one instance, runs anywhere (the brain) -----------------
controlplane-up: images
	bash controlplane/infra/podman.sh up

# Stops and removes the control-plane containers but KEEPS the data volumes, so `down` + `up` is
# always safe. To delete the data too, ask for it by name: `make controlplane-destroy`.
controlplane-down:
	bash controlplane/infra/podman.sh down

# Irreversible: deletes the postgres + GreptimeDB volumes (every platform experiment, experiment
# record, metric and hypothesis). Prompts unless CONFIRM=--yes is passed.
controlplane-destroy:
	bash controlplane/infra/podman.sh destroy $(CONFIRM)

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
# Usage: make cluster-agent-up CLUSTER=<name> [KUBECONFIG_PATH=...] [KUBE_CONTEXT=...] [CONTROLPLANE_URL=...] [REGISTRY_URL=...]
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

# Provisions the serving node, runs the portable e2e suite, detaches it — pass/fail either way.
tt-hardware-image:
	podman build -f tests/workloads/tenstorrent/Dockerfile.train -t localhost/hypothesisloop-tenstorrent-workload tests/workloads/tenstorrent/
	podman save localhost/hypothesisloop-tenstorrent-workload:latest | sudo k3s ctr images import -

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
# see localdev/git/git-daemon.sh's header comment for why both matter. Run `start` again any time a new
# experiment repo is created under the daemon's base-path, since a running daemon won't pick it up on
# its own.
git-daemon-start:
	bash localdev/git/git-daemon.sh start

git-daemon-stop:
	bash localdev/git/git-daemon.sh stop

git-daemon-status:
	bash localdev/git/git-daemon.sh status

git-daemon-destroy:
	bash localdev/git/git-daemon.sh destroy

tt-stop:
	bash localdev/k3s-tenstorrent-qb2/stop.sh

tt-start:
	bash localdev/k3s-tenstorrent-qb2/start.sh

# Tagged explicitly under localhost/ (not just the short name) because the DaemonSet/
# Deployment/Job specs reference these images as localhost/hypothesisloop-*:latest with
# imagePullPolicy: Never — podman defaults unqualified build tags to localhost/ already, but
# Docker's CLI (and podman in rootful mode talking through the docker-compatible socket)
# defaults to docker.io/library/ instead, which silently breaks that pull-policy match.
images:
	podman build -f controlplane/build/Dockerfile.control-service    -t localhost/hypothesisloop-control-service .
	podman build -f controlplane/build/Dockerfile.metrics-service    -t localhost/hypothesisloop-metrics-service .
	podman build -f runtime/k8s/build/Dockerfile.node-agent              -t localhost/hypothesisloop-node-agent .
	podman build -f runtime/k8s/build/Dockerfile.cluster-agent           -t localhost/hypothesisloop-cluster-agent .
	podman build -f tests/workloads/generic/Dockerfile.train    -t localhost/hypothesisloop-workload tests/workloads/generic/

# Every file an experiment definition's Dockerfile COPYs in (or that COPY'd code imports) must exist in git --
# a seed/ that only works because of an untracked or previously-built-image-only file breaks
# silently for anyone starting from a clean checkout. See localdev/check-experiment-seeds.sh.
check-experiment-seeds:
	bash localdev/check-experiment-seeds.sh

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
experimentator-image: check-experiment-seeds experimentator-base-image
	test -n "$(EXPERIMENT)" || { echo "usage: make experimentator-image EXPERIMENT=<name>" >&2; exit 1; }
	test -f agents/coordinator/experiments/$(EXPERIMENT)/Dockerfile.experimentator || \
		{ echo "no agents/coordinator/experiments/$(EXPERIMENT)/Dockerfile.experimentator" >&2; exit 1; }
	podman build -f agents/coordinator/experiments/$(EXPERIMENT)/Dockerfile.experimentator \
		-t localhost/hypothesisloop-experimentator-$(EXPERIMENT) agents/coordinator/experiments/$(EXPERIMENT)

# Pre-builds the sparse-sdpa experiment's own job image (harness.py + torch + pinned upstream
# sparse_sdpa test-utils baked in -- see agents/coordinator/experiments/sparse-sdpa/seed/Dockerfile.workload)
# so every job an agent submits starts instantly with zero per-job setup. Run this once before
# starting a sparse-sdpa platform experiment, same as experimentator-image.
sparse-sdpa-workload-image: check-experiment-seeds
	podman build -f agents/coordinator/experiments/sparse-sdpa/seed/Dockerfile.workload \
		-t localhost/hypothesisloop-sparse-sdpa-workload agents/coordinator/experiments/sparse-sdpa/seed/

build:
	go build ./...

test:
	go test ./... -timeout 60s

lint:
	golangci-lint run ./...
