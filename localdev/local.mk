.PHONY: registry-up prune-images controlplane-up controlplane-down controlplane-destroy \
	dev dev-api dev-status dev-down tunnels tunnels-down dev-hosted staff-token token runtime-token \
	k3s-up k3s-down k3s-dev-nodes-up k3s-dev-nodes-down full-stop full-start reload stack-not-held \
	tt-up tt-down tt-status tt-stop tt-start tt-dev-nodes-up tt-dev-nodes-down \
	nvidia-up nvidia-down nvidia-status \
	git-repository images-remote

# ---- The local stack: control plane, dev registry and local clusters ----------
registry-up:
	$(COMPOSE) up -d registry
	$(call await,the dev registry,curl -fsS -o /dev/null http://$(REGISTRY_PUBLISH_IP):5000/v2/)

# The local stack's own build, apart from publish: the images the compose control plane and the
# local cluster run, and the workload the e2e tests' jobs run, pushed to the dev registry under
# TAG. publish cannot serve here: its UI image needs a sign-in service the local stack does not
# build against (local development serves the UI with `npm run dev`), its framework job images
# pull gigabytes, and nothing local installs a published chart.
# IMAGE_TAG_FILE hands TAG to the e2e tests, whose jobs must pin it. It is written only by a push
# to the dev registry: the tests pull from there.
LOCAL_IMAGES := control-service metrics-service node-agent cluster-agent bare-agent job-base job workload
locked-local-images: check-tag $(if $(filter $(LOCAL_REGISTRY),$(REGISTRY)),registry-up)
	$(GO_VENDOR)
	$(MAKE) -j1 --no-print-directory $(addprefix image-,$(LOCAL_IMAGES)) || { rm -rf vendor; exit 1; }
	rm -rf vendor
	$(if $(filter $(LOCAL_REGISTRY),$(REGISTRY)),echo "$(TAG)" > $(IMAGE_TAG_FILE))
	$(MAKE) prune-images TAGS="$(TAG)"

# One build of this project's images stays on this machine: every other tag leaves podman's
# image store and the dev registry (localdev/prune-images.py says what counts as in use). Each
# target that builds ends here. TAGS names tags to keep besides those.
prune-images:
	python3 localdev/images-lock.py python3 localdev/prune-images.py $(TAGS)

# Each compose service reads its keys from the environment file render-settings writes.
controlplane-up: registry-up render-settings local-images
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
# Spins up a local k3s cluster (control-plane only, tainted no-workload, see install.sh), installs
# the cluster agent on it as cluster local and provisions the dev nodes (see k3s-dev-nodes-up
# below), so this target alone produces a fully working local dev cluster. Set NODE_COUNT=0 to
# stay control-plane only.
LOCAL_CLUSTER_AGENT = RUNTIME_TOKEN="$$(bash -c '. localdev/lib/auth.sh && lib_runtime_token local')" && \
  $(call cluster_agent_install,local,http://host.containers.internal:8081,http://host.containers.internal:8084,$(LOCAL_REGISTRY),false)
k3s-up reload: KUBE_CONTEXT := k3s-local
k3s-up: render-settings local-images
	bash localdev/k3s-macos/install.sh
	$(LOCAL_CLUSTER_AGENT)
	$(MAKE) k3s-dev-nodes-up

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
# registry and upgrades the runtime on the local cluster to the new TAG, which rolls its pods.
# Needed after a change
# under runtime/k8s, tests/workloads/generic or a package those binaries compile in, and before
# the first `make e2e`. A control plane change needs `make dev-api` and no image.
reload: stack-not-held render-settings local-images
	$(LOCAL_CLUSTER_AGENT)
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
TT_CONTEXT ?= k3s-tt
TT_OPERATOR_NAMESPACE ?= tt-operator-system
tt-up:
	bash localdev/k3s-tenstorrent/install.sh
	$(MAKE) tt-status

# Destructive the same way k3s-down is (see its comment): tears down the whole k3s cluster on
# this Tenstorrent host. The tt-operator release lives in k3s's own datastore, which the uninstall
# deletes; the host's tt-kmd install is left alone. rename-context in install.sh leaves the
# kubeconfig's cluster and user named default. Use `make tt-stop`/`tt-start` to pause/resume
# without losing cluster state.
tt-down:
	$(MAKE) tt-dev-nodes-down DESTROYING_CLUSTER=1
	sudo /usr/local/bin/k3s-uninstall.sh
	kubectl config delete-context $(TT_CONTEXT) 2>/dev/null || true
	kubectl config delete-cluster default 2>/dev/null || true
	kubectl config delete-user default 2>/dev/null || true

# Provisions fake-accelerator container nodes. Idempotent.
tt-dev-nodes-up:
	bash localdev/k3s-tenstorrent/dev-nodes-up.sh

# Removes the fake nodes; the host's own node is left as it is.
tt-dev-nodes-down:
	bash localdev/k3s-tenstorrent/dev-nodes-down.sh

# Node readiness, NFD device labels, operator components, the DRA-published devices and the
# host's own tt-smi list. Read-only.
tt-status:
	@kubectl --context $(TT_CONTEXT) get nodes >/dev/null 2>&1 || { echo "make tt-status: context $(TT_CONTEXT) is not reachable, run make tt-up first" >&2; exit 1; }
	kubectl --context $(TT_CONTEXT) get nodes -L feature.node.kubernetes.io/pci-1200_1e52.present,driver.tenstorrent.com/kmd-version,driver.tenstorrent.com/install-mode
	kubectl --context $(TT_CONTEXT) -n $(TT_OPERATOR_NAMESPACE) get deploy,ds
	kubectl --context $(TT_CONTEXT) get resourceslices -o custom-columns='NAME:.metadata.name,POOL:.spec.pool.generation,DEVICES:.spec.devices[*].name'
	$$(command -v tt-smi || echo $$HOME/.tenstorrent-venv/bin/tt-smi) -ls

# Stops k3s and with it every pod; cluster state, the Helm release and CRDs stay on disk.
tt-stop:
	sudo systemctl stop k3s

tt-start:
	sudo systemctl start k3s
	$(call await,the $(TT_CONTEXT) API,kubectl --context $(TT_CONTEXT) get nodes)
	kubectl --context $(TT_CONTEXT) wait node --all --for=condition=Ready --timeout=120s
	kubectl config use-context $(TT_CONTEXT) >/dev/null
	$(MAKE) tt-status

# ---- Real NVIDIA hardware: k3s + device plugin and GPU Feature Discovery ----
# Run on a Linux host with an NVIDIA GPU and its driver (install.sh); the control plane's host
# then attaches it with cluster-agent-up CLUSTER=k3s-nvidia.
NVIDIA_CONTEXT := k3s-nvidia
nvidia-up:
	bash localdev/k3s-nvidia/install.sh
	$(MAKE) nvidia-status

# Destructive the same way k3s-down is: uninstalls k3s and every object in it. The host's NVIDIA
# driver, nvidia-container-toolkit and docker's default runtime stay as they are.
nvidia-down:
	sudo /usr/local/bin/k3s-uninstall.sh
	kubectl config delete-context $(NVIDIA_CONTEXT) 2>/dev/null || true
	kubectl config delete-cluster default 2>/dev/null || true
	kubectl config delete-user default 2>/dev/null || true

# Node readiness, GPU labels, the nvidia.com/gpu capacity advertised, the device plugin and GFD
# pods, the cluster agent and the host's own nvidia-smi. Read-only.
nvidia-status:
	@kubectl --context $(NVIDIA_CONTEXT) get nodes >/dev/null 2>&1 || { echo "make nvidia-status: context $(NVIDIA_CONTEXT) is not reachable, run make nvidia-up first" >&2; exit 1; }
	kubectl --context $(NVIDIA_CONTEXT) get nodes -L nvidia.com/gpu.product,nvidia.com/gpu.count,nvidia.com/gpu.memory
	kubectl --context $(NVIDIA_CONTEXT) get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}'
	kubectl --context $(NVIDIA_CONTEXT) -n kube-system get pods -l name=nvidia-device-plugin-ds
	kubectl --context $(NVIDIA_CONTEXT) -n kube-system get pods -l app.kubernetes.io/name=gpu-feature-discovery
	kubectl --context $(NVIDIA_CONTEXT) -n hypothesisloop get deploy,ds
	nvidia-smi -L

# ---- Local git host: a stack whose GIT_HOST_PROVIDER is local keeps its code on the git server make dev runs ----
# One bare repository on it, which a platform experiment then names in `repository` as NAME.git:
# the platform creates none on a plain git host. Usage: make git-repository NAME=my-experiment
git-repository:
	@test -n "$(NAME)" || { echo "make git-repository: NAME is required" >&2; exit 1; }
	@bash -c '. localdev/lib/auth.sh && \
	  git_host="$$(lib_git_host_provider)" && \
	  { test "$$git_host" = local || { echo "make git-repository: this stack keeps its code on GitHub (GIT_HOST_PROVIDER=$$git_host in .env)" >&2; exit 1; }; } && \
	  git init -q --bare "$$HL_GIT_ROOT/$(NAME).git" && echo "$(NAME).git"'

# make publish run on BUILDER (an ssh host with docker buildx, linux/amd64), pushing to the
# registry of .env, so no image is uploaded from this machine (localdev/images-remote.sh). Publishes
# HEAD and prints the IMAGE_TAG to set. IMPORT=true also loads every image into BUILDER's k3s.
# Usage: make images-remote BUILDER=<ssh host> [IMPORT=true] [SSH_OPTIONS=...]
images-remote:
	@test -n "$(BUILDER)" || { echo "usage: make images-remote BUILDER=<ssh host> [IMPORT=true] [SSH_OPTIONS=...]" >&2; exit 1; }
	@SSH_OPTIONS="$(SSH_OPTIONS)" bash localdev/images-remote.sh "$(BUILDER)" $(if $(filter true,$(IMPORT)),--import)
