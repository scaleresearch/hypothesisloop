.PHONY: registry-up prune-images controlplane-up controlplane-down controlplane-destroy \
	dev dev-api dev-status dev-down tunnels tunnels-down dev-hosted staff-token token runtime-token \
	k3s-up k3s-down k3s-dev-nodes-up k3s-dev-nodes-down full-stop full-start reload stack-not-held \
	tt-up tt-down tt-status tt-stop tt-start tt-dev-nodes-up tt-dev-nodes-down \
	git-repository images-remote

# ---- The local stack: control plane, dev registry and local clusters ----------
registry-up:
	$(COMPOSE) up -d registry
	$(call await,the dev registry,curl -fsS -o /dev/null http://$(REGISTRY_PUBLISH_IP):5000/v2/)

# One build of this project's images stays on this machine: every other tag leaves podman's
# image store and the dev registry (localdev/prune-images.py says what counts as in use). Each
# target that builds ends here. TAGS names tags to keep besides those.
prune-images:
	python3 localdev/images-lock.py python3 localdev/prune-images.py $(TAGS)

# Each compose service reads its keys from the environment file render-settings writes.
controlplane-up: registry-up render-settings images
	$(COMPOSE) up -d
	$(call await,the control service,curl -fsS -o /dev/null http://localhost:$${API_HOST_PORT:-8081}/health)
	$(call await,the metrics service,curl -fsS -o /dev/null http://localhost:8084/health)
	bash -c '. localdev/lib/auth.sh && lib_register_clusters && lib_register_local_cluster'

# The control plane and the UI as host processes against the compose datastores: starts in
# seconds, and a code change needs no image build. Every process runs detached with its log in
# localdev/.run, so the target returns and one process can be restarted alone. Runtimes on the
# local cluster still pull images (make reload).
dev: render-settings
	bash localdev/dev.sh up

# After a Go change: rebuilds and restarts the API and metrics-service, with the settings as they
# were last rendered. The UI (live reload), the tunnels and the runtime processes keep running.
dev-api:
	bash localdev/dev.sh api

dev-status:
	@bash localdev/dev.sh status

dev-down:
	bash localdev/dev.sh down

# Cloudflare quick tunnels to this machine's API, metrics-service and object store, and to its git
# server on a stack with no GitHub App, which is how a hosted agent and a rented instance reach them. They keep running across restarts of the dev
# stack, and their addresses change only when they are started again.
tunnels:
	bash localdev/local-tunnels/tunnels.sh up

tunnels-down:
	bash localdev/local-tunnels/tunnels.sh down

# make dev, with settings that have control-service rent hosted agents and jobs on the providers
# .env names, naming the running tunnels and the images under IMAGE_TAG, which must be in the
# registry, pushed again after a change to them. Needs
# `make tunnels` first. From here hosted agents and jobs placed on the cloud cluster rent
# machines, which cost money.
dev-hosted: render-settings
	bash localdev/dev.sh hosted

# Every request to the control plane carries a token. These two print the ones local work needs,
# derived from the signing secret in localdev/.run/signing-secret (localdev/lib/auth.sh).
# Usage: export HL_TOKEN="$(make -s staff-token)"
staff-token: render-settings
	@bash -c '. localdev/lib/auth.sh && lib_hl_admin token staff'

# Usage: make -s token SCOPE=agent:<platform experiment>/<agent> (or coordinator:<platform experiment>, job:<id>)
token: render-settings
	@test -n "$(SCOPE)" || { echo "make token: SCOPE=<scope> is required" >&2; exit 1; }
	@bash -c '. localdev/lib/auth.sh && lib_hl_admin token "$(SCOPE)"'

# Registers CLUSTER for ORG and prints the runtime token its agents start with.
# Usage: make -s runtime-token CLUSTER=<name> [ORG=default]
ORG ?= default
runtime-token: render-settings
	@test -n "$(CLUSTER)" || { echo "make runtime-token: CLUSTER=<name> is required" >&2; exit 1; }
	@bash -c '. localdev/lib/auth.sh && lib_hl_admin cluster add "$(CLUSTER)" "$(ORG)"'

# Stops and removes the control-plane containers but KEEPS the data volumes, so `down` + `up` is
# always safe. To delete the data too, ask for it by name: `make controlplane-destroy`.
controlplane-down:
	$(COMPOSE) down

# Irreversible: deletes the postgres + GreptimeDB + Memgraph + MinIO volumes (every platform
# experiment, experiment record, metric, hypothesis and stored checkpoint). Prompts unless CONFIRM=--yes.
controlplane-destroy:
	@if [ "$(CONFIRM)" != "--yes" ]; then \
		echo "make controlplane-destroy: this DELETES ALL control-plane data. There is no backup and no undo." >&2; \
		echo "  Re-run with CONFIRM=--yes to proceed." >&2; \
		exit 1; \
	fi
	$(COMPOSE) down --volumes

# ---- Local dev cluster bootstrap --------------------------------------------
# Spins up a local k3s cluster (control-plane only, tainted no-workload — see install.sh), then
# installs the cluster-agent bundle onto it (localdev/k3s-macos/install.sh calls
# deploy/charts/hypothesisloop-cluster-agent/install.sh itself) and provisions the dev nodes (see k3s-dev-nodes-up below),
# so this target alone produces a fully working local dev cluster. Set NODE_COUNT=0 to stay
# control-plane only.
k3s-up: render-settings images
	bash localdev/k3s-macos/install.sh

# Unlike controlplane-down, this one IS destructive: the k3s cluster's own state (etcd/sqlite,
# every Job/Deployment/Pod it knows about) lives only inside the k3s node itself, so tearing the
# node down is the whole cluster's `destroy`, not a `down`. No confirmation prompt today — the
# cluster is ephemeral local-dev infra, not the durable postgres/GreptimeDB/Memgraph data controlplane-down
# protects. Use `make full-stop` / `full-start` for a same-state pause/resume instead.
k3s-down:
	bash localdev/k3s-macos/destroy.sh

# Provisions (or re-syncs) the dev node containers — idempotent, safe to call again after
# a VM restart killed the background agent containers, without redoing the whole bootstrap.
# These same nodes serve both interactive dev work and the e2e suite (make e2e).
# Usage: make k3s-dev-nodes-up [NODE_COUNT=4]
k3s-dev-nodes-up:
	NODE_COUNT="$(NODE_COUNT)" bash localdev/k3s-macos/dev-nodes-up.sh

# Tears the dev nodes back down, returning the cluster to control-plane-only.
k3s-dev-nodes-down:
	bash localdev/k3s-macos/dev-nodes-down.sh

# Pause/resume everything (podman machine, k3s, control plane) without destroying
# cluster state — much faster than k3s-down/k3s-up for a daily on/off cycle.
full-stop:
	bash localdev/k3s-macos/stop.sh

full-start:
	bash localdev/k3s-macos/start.sh

# What still runs from images is the local cluster's runtime (cluster-agent, node-agent) and the
# workload the e2e tests' jobs run. This rebuilds every image from the tree, pushes it to the dev
# registry and installs the runtime on the local cluster under the new TAG. Needed after a change
# under runtime/k8s, tests/workloads/generic or a package those binaries compile in, and before
# the first `make e2e`. A control plane change needs `make dev-api` and no image.
reload: stack-not-held render-settings images
	bash localdev/k3s-macos/reload.sh
	$(MAKE) prune-images

# Restarting the local cluster's agents takes the cluster away from whoever runs jobs on it, so
# what does that stops while somebody holds the stack (make e2e does for as long as it runs).
stack-not-held:
	@if [ -d /tmp/hl-stack.lock ]; then \
		echo "make: the stack is held (/tmp/hl-stack.lock): $$(cat /tmp/hl-stack.lock/owner 2>/dev/null || echo no owner named)" >&2; exit 1; \
	fi

# ---- Real Tenstorrent hardware: k3s + tt-operator device stack --------------
# Counterpart to k3s-up, for an actual Tenstorrent host instead of simulated
# fake-accelerator nodes. Run on the box; the control plane's host then attaches it with
# cluster-agent-up.
tt-up:
	bash localdev/k3s-tenstorrent/install.sh

# Destructive the same way k3s-down is (see its comment) — tears down the whole k3s cluster on
# this Tenstorrent host, not just this repo's containers. Use `make tt-stop`/`tt-start` to
# pause/resume without losing cluster state.
tt-down:
	bash localdev/k3s-tenstorrent/destroy.sh

# Provisions fake-accelerator container nodes. Idempotent.
tt-dev-nodes-up:
	bash localdev/k3s-tenstorrent/dev-nodes-up.sh

# Builds and pushes the Tenstorrent hardware workload image to $(REGISTRY) -- the Tenstorrent
# host's k3s node pulls it normally (registries.yaml, see localdev/lib/node.sh), same as every other
# image; no more local save/import.
locked-tt-hardware-image: check-tag
	podman build --platform $(IMAGE_PLATFORM) -f tests/workloads/tenstorrent/Dockerfile.train -t $(REGISTRY)/hypothesisloop-tenstorrent-workload:$(TAG) tests/workloads/tenstorrent/
	podman tag $(REGISTRY)/hypothesisloop-tenstorrent-workload:$(TAG) $(REGISTRY)/hypothesisloop-tenstorrent-workload:latest
	podman push --tls-verify=$(REGISTRY_TLS_VERIFY) $(REGISTRY)/hypothesisloop-tenstorrent-workload:$(TAG)
	podman push --tls-verify=$(REGISTRY_TLS_VERIFY) $(REGISTRY)/hypothesisloop-tenstorrent-workload:latest

# Removes the fake nodes; the host's own node is left as it is.
tt-dev-nodes-down:
	bash localdev/k3s-tenstorrent/dev-nodes-down.sh

tt-status:
	bash localdev/k3s-tenstorrent/status.sh

tt-stop:
	bash localdev/k3s-tenstorrent/stop.sh

tt-start:
	bash localdev/k3s-tenstorrent/start.sh

# ---- Local git host: a stack whose GIT_HOST_PROVIDER is local keeps its code on the git server make dev runs ----
# One bare repository on it, which a platform experiment then names in `repository` as NAME.git:
# the platform creates none on a plain git host. Usage: make git-repository NAME=my-experiment
git-repository:
	@test -n "$(NAME)" || { echo "make git-repository: NAME is required" >&2; exit 1; }
	@bash -c '. localdev/lib/auth.sh && \
	  git_host="$$(lib_git_host_provider)" && \
	  { test "$$git_host" = local || { echo "make git-repository: this stack keeps its code on GitHub (GIT_HOST_PROVIDER=$$git_host in .env)" >&2; exit 1; }; } && \
	  git init -q --bare "$$HL_GIT_ROOT/$(NAME).git" && echo "$(NAME).git"'

# The images a hosted run uses, built on BUILDER (an ssh host with docker buildx, linux/amd64) and
# pushed from there to the registry of .env, so none is uploaded from this machine
# (localdev/images-remote.sh). Builds HEAD and prints the IMAGE_TAG to set.
# Usage: make images-remote BUILDER=<ssh host> [IMPORT=true] [FRAMEWORK_JOBS=true] [IMAGES="<name>..."] [SSH_OPTIONS=...]
images-remote:
	@test -n "$(BUILDER)" || { echo "usage: make images-remote BUILDER=<ssh host> [IMPORT=true] [FRAMEWORK_JOBS=true] [IMAGES=...]" >&2; exit 1; }
	@IMAGES="$(IMAGES)" SSH_OPTIONS="$(SSH_OPTIONS)" bash localdev/images-remote.sh "$(BUILDER)" $(if $(filter true,$(IMPORT)),--import) $(if $(filter true,$(FRAMEWORK_JOBS)),$(JOB_IMAGE_BASES))
