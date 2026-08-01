.PHONY: up down reset build test lint images \
	controlplane-up controlplane-down \
	cluster-agent-up cluster-agent-down \
	k3s-up k3s-down full-up k3s-dev-nodes-up k3s-dev-nodes-down k3s-e2e \
	full-stop full-start reload \
	tt-up tt-down tt-status tt-stop tt-start tt-dev-nodes-up tt-dev-nodes-down tt-hardware-image tt-e2e

# ---- Control plane: one instance, runs anywhere (the brain) -----------------
controlplane-up: images
	bash controlplane/infra/podman.sh up

controlplane-down:
	bash controlplane/infra/podman.sh down

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

# Detaches it again, returning the cluster to control-plane-only so real training work outside
# k3s isn't sharing capacity with anything scheduled onto it.
tt-dev-nodes-down:
	bash localdev/k3s-tenstorrent-qb2/dev-nodes-down.sh

tt-status:
	bash localdev/k3s-tenstorrent-qb2/status.sh

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

# Root build context (not agents/experimentator/) so the Dockerfile can bake in
# agents/coordinator/tasks as the agent's WORKLOAD_SAMPLES starting point.
experimentator-image:
	podman build -f agents/experimentator/Dockerfile -t localhost/hypothesisloop-experimentator .

build:
	go build ./...

test:
	go test ./... -timeout 60s

lint:
	golangci-lint run ./...
