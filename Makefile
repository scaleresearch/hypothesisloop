.PHONY: up down reset build test lint images \
	controlplane-up controlplane-down \
	cluster-agent-up cluster-agent-down \
	k3s-up k3s-down full-up k3s-add-fake-nodes \
	full-stop full-start reload \
	tt-up tt-down tt-status tt-stop tt-start

COMPOSE_FILE := controlplane/infra/docker-compose.yaml

# ---- Control plane: one instance, runs anywhere (the brain) -----------------
controlplane-up: images
	podman compose -f $(COMPOSE_FILE) up -d

controlplane-down:
	podman compose -f $(COMPOSE_FILE) down -v

# Back-compat aliases for the old target names.
up: controlplane-up
down: controlplane-down

reset:
	$(MAKE) controlplane-down
	$(MAKE) controlplane-up

# ---- Cluster agent: one per target Kubernetes cluster running jobs ----------
# Install/remove the node-agent DaemonSet + cluster-agent Deployment (the only
# component with real k8s credentials — polls the control plane, never dialed
# into) on whatever cluster KUBE_CONTEXT/KUBECONFIG_PATH point at (or the
# current kubectl context).
# Usage: make cluster-agent-up CLUSTER=<name> [KUBECONFIG_PATH=...] [KUBE_CONTEXT=...] [CONTROLPLANE_URL=...] [REGISTRY_URL=...]
cluster-agent-up:
	CLUSTER_NAME="$(CLUSTER)" bash cluster/infra/install.sh

cluster-agent-down:
	CLUSTER_NAME="$(CLUSTER)" bash cluster/infra/destroy.sh

# ---- Local dev cluster bootstrap --------------------------------------------
# Spins up a local k3s cluster, then installs the cluster-agent bundle onto it
# (localdev/k3s-macos/install.sh calls cluster/infra/install.sh itself), so this
# target alone produces a fully working local dev target cluster. install.sh also adds a
# few extra fake-accelerator-type nodes at the end (see k3s-add-fake-nodes below) so the cluster has
# more than one accelerator type to schedule onto out of the box; set EXTRA_NODES=0 to skip that.
k3s-up: images
	bash localdev/k3s-macos/install.sh

k3s-down:
	bash localdev/k3s-macos/destroy.sh

# Re-runs just the extra-fake-accelerator-node step (idempotent — install.sh already calls this once
# at the end of k3s-up). Useful to add more nodes, or to recover them after a VM restart
# killed the background agent processes without redoing the whole cluster bootstrap.
# Usage: make k3s-add-fake-nodes [EXTRA_NODES=3]
k3s-add-fake-nodes:
	EXTRA_NODES="$(EXTRA_NODES)" bash localdev/k3s-macos/add-fake-nodes.sh

# ---- Convenience: local cluster + control plane in one command -------------
full-up: k3s-up controlplane-up

# Pause/resume everything (podman machine, k3s, control plane) without destroying
# cluster state — much faster than full-up/k3s-down for a daily on/off cycle.
full-stop:
	bash localdev/k3s-macos/stop.sh

full-start:
	bash localdev/k3s-macos/start.sh

# Rebuild every image from current source and push it into every place that caches one
# (podman store, k3s server, each fake-accelerator-node container), then bounce the control-plane
# containers and cluster-agent/node-agent pods so they run it. Use this after a Go change,
# before re-running the e2e suites — faster than a manual images+import+restart chain since
# every step polls for readiness instead of sleeping a fixed guess.
reload:
	bash localdev/k3s-macos/reload.sh

# ---- Real Tenstorrent hardware: k3s + tt-operator device stack --------------
# Counterpart to k3s-up, for an actual Tenstorrent host instead of simulated
# fake-accelerator nodes. See localdev/k3s-tenstorrent-qb2/README.md.
tt-up:
	bash localdev/k3s-tenstorrent-qb2/install.sh

tt-down:
	bash localdev/k3s-tenstorrent-qb2/destroy.sh

tt-status:
	bash localdev/k3s-tenstorrent-qb2/status.sh

tt-stop:
	bash localdev/k3s-tenstorrent-qb2/stop.sh

tt-start:
	bash localdev/k3s-tenstorrent-qb2/start.sh

# ---- Real Tenstorrent hardware: k3s + tt-operator device stack --------------
# Counterpart to k3s-up, for an actual Tenstorrent host instead of simulated
# fake-accelerator nodes. See tenstorrent/README.md.
tt-up:
	bash tenstorrent/install.sh

tt-down:
	bash tenstorrent/destroy.sh

tt-status:
	bash tenstorrent/status.sh

tt-stop:
	bash tenstorrent/stop.sh

tt-start:
	bash tenstorrent/start.sh

# Tagged explicitly under localhost/ (not just the short name) because the DaemonSet/
# Deployment/Job specs reference these images as localhost/openresearch-*:latest with
# imagePullPolicy: Never — podman defaults unqualified build tags to localhost/ already, but
# Docker's CLI (and podman in rootful mode talking through the docker-compatible socket)
# defaults to docker.io/library/ instead, which silently breaks that pull-policy match.
images:
	podman build -f controlplane/build/Dockerfile.control-service    -t localhost/openresearch-control-service .
	podman build -f controlplane/build/Dockerfile.metrics-service    -t localhost/openresearch-metrics-service .
	podman build -f cluster/build/Dockerfile.node-agent              -t localhost/openresearch-node-agent .
	podman build -f cluster/build/Dockerfile.cluster-agent           -t localhost/openresearch-cluster-agent .
	podman build -f tests/workloads/generic/Dockerfile.train    -t localhost/openresearch-workload tests/workloads/generic/
	podman build -f tests/workloads/robotics/Dockerfile.train -t localhost/openresearch-robotics-workload tests/workloads/robotics/

build:
	go build ./...

test:
	go test ./... -timeout 60s

lint:
	golangci-lint run ./...
