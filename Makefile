.PHONY: $(addprefix locked-,images ui-image tt-hardware-image sparse-sdpa-workload-image)
.PHONY: dev reset build test agent-test lint check images job-images sparse-sdpa-workload-image sparse-sdpa-experiment \
	experimentator-base-image experimentator-image check-tag ui-image check-ui-build-args registry-up prune-images \
	helm-prepare helm-lint chart-push \
	controlplane-up controlplane-down controlplane-destroy render-settings \
	cluster-agent-up cluster-agent-down staff-token token runtime-token \
	k3s-up k3s-down full-up k3s-dev-nodes-up k3s-dev-nodes-down e2e \
	full-stop full-start reload stack-not-held \
	tt-up tt-down tt-status tt-stop tt-start tt-dev-nodes-up tt-dev-nodes-down tt-hardware-image tt-e2e \
	git-repository db-migration

# ---- Control plane: one instance, runs anywhere (the brain) -----------------
COMPOSE_FILE := localdev/controlplane/docker-compose.yml
RENDERED_SETTINGS := controlplane/settings/.hypothesisloop.rendered.yaml
HOST_ADDRESS_FILE := localdev/.host-address
IMAGE_TAG_FILE := localdev/.image-tag

# HOST_ADDRESS is how every k3s node, job pod and container reaches this host's published ports
# (localdev/lib/node.sh's lib_host_address): host.containers.internal on macOS, which survives
# the Mac changing networks, and the LAN address on a native Linux k3s. REGISTRY_PUBLISH_IP is
# where the registry publishes: the Mac's loopback, which host.containers.internal leads to, or
# the LAN address on Linux. Never *:5000, which macOS AirPlay holds.
HOST_ADDRESS := $(shell bash -c '. localdev/lib/node.sh && lib_host_address')
REGISTRY_PUBLISH_IP := $(shell bash -c '. localdev/lib/node.sh && lib_registry_publish_ip')
# LOCAL_REACHABLE_ADDRESS in .env (.env.example) is the address clusters on other machines reach
# this host at. Set, the rendered settings hand it out in place of HOST_ADDRESS, and the registry
# also publishes on REGISTRY_REACHABLE_IP, what it resolves to (docker-compose.reachable.yml).
# Local nodes keep reaching everything through HOST_ADDRESS.
REGISTRY_REACHABLE_IP := $(shell bash -c '. localdev/lib/node.sh && lib_registry_reachable_ip')
COMPOSE := podman compose -f $(COMPOSE_FILE) $(if $(REGISTRY_REACHABLE_IP),-f localdev/controlplane/docker-compose.reachable.yml)

# In production REGISTRY names a real registry the operator provisioned and every cluster
# node already has network access to (see runtime/k8s/infra/install.sh's REGISTRY_URL) --
# this default is the local dev registry. TAG names the tree content that was built (localdev/lib/image-tag.sh),
# so a manifest that pins one tag can never silently start running different bytes under
# imagePullPolicy: IfNotPresent. Computed once here and exported: every script that deploys an
# image reads it from make. REGISTRY_TLS_VERIFY=false only because the dev registry
# in docker-compose.yml serves plain HTTP; a prod REGISTRY with real TLS overrides it to true.
LOCAL_REGISTRY := $(HOST_ADDRESS):5000
REGISTRY ?= $(LOCAL_REGISTRY)
ifndef TAG
TAG := $(shell bash localdev/lib/image-tag.sh)
endif
export HOST_ADDRESS REGISTRY_PUBLISH_IP REGISTRY_REACHABLE_IP REGISTRY TAG
# The settings file names this variable and never holds the secret. Local development's value is
# made by the first render in localdev/.run/signing-secret (localdev/lib/auth.sh reads it) and
# reaches compose, the scripts and the tests from here.
export HYPOTHESISLOOP_SIGNING_SECRET ?= $(shell bash -c '. localdev/lib/auth.sh && printf %s "$$HYPOTHESISLOOP_SIGNING_SECRET"')
REGISTRY_TLS_VERIFY ?= false
# Run before every push. A registry whose login expires logs in here (set by the deployment tooling); the
# dev registry takes none.
REGISTRY_LOGIN ?= true
# The platform every image is built for: the container engine's own unless set (linux/amd64 for
# nodes of another architecture). It is always stated to the build. Left unstated, a base image
# the engine already holds for another platform is used as it is, and the image built on it
# starts nowhere on this machine's nodes (exec format error).
IMAGE_PLATFORM ?= $(shell podman info --format '{{.Host.OS}}/{{.Host.Arch}}')

check-tag:
	@test -n "$(IMAGE_PLATFORM)" || { echo "make: could not ask podman for its platform; set IMAGE_PLATFORM" >&2; exit 1; }
	@test -n "$(TAG)" || { echo "make: could not compute an image TAG (localdev/lib/image-tag.sh)" >&2; exit 1; }

# data_store.endpoint in controlplane/settings/hypothesisloop.yaml carries a
# REPLACE-WITH-HOST-ADDRESS placeholder rather than an address, because a k3s job pod has its own
# network namespace and can't reach the host's loopback, and the address differs between macOS
# and Linux hosts. This renders HOST_ADDRESS in at start-up, or LOCAL_REACHABLE_ADDRESS when .env
# sets it, the one address remote clusters are handed for everything; the control plane rejects a
# loopback endpoint outright, so a wrong address fails here, not hours into a run with nothing saved.
# HOST_ADDRESS_FILE hands the same address to the scripts and tests that run outside make.
# Everything that runs hl-admin depends on this: it reads the rendered settings, since the
# committed file is refused at load for its placeholders. The recipe prints nothing, so a
# target whose output is a token stays just the token.
render-settings:
	@test -n "$(HOST_ADDRESS)" || { echo "make: could not detect this host's address" >&2; exit 1; }
	@bash -c '. localdev/lib/auth.sh && lib_render_settings "$(HOST_ADDRESS)" $(RENDERED_SETTINGS) && lib_registry_reachable_ip >/dev/null'
	@echo "$(HOST_ADDRESS)" > $(HOST_ADDRESS_FILE)

registry-up:
	$(COMPOSE) up -d registry
	@until curl -fsS -o /dev/null http://$(REGISTRY_PUBLISH_IP):5000/v2/; do sleep 1; done

# One build of this project's images stays on this machine: every other tag leaves podman's
# image store and the dev registry (localdev/prune-images.py says what counts as in use). Each
# target that builds ends here. TAGS names tags to keep besides those.
prune-images:
	python3 localdev/images-lock.py python3 localdev/prune-images.py $(TAGS)

# A build's tag is not in use until it is pushed and installed, so a prune running beside it
# would take it away. Every target that builds therefore runs its recipe (locked-<target>) under
# the one lock the prune takes too (localdev/images-lock.py).
BUILDS := images ui-image tt-hardware-image sparse-sdpa-workload-image
$(BUILDS):
	@python3 localdev/images-lock.py $(MAKE) locked-$@ TAG="$(TAG)"

# compose is started under auth.sh, which exports the git host's secrets GIT_HOST_PROVIDER names:
# a PEM does not survive being a make variable.
controlplane-up: registry-up render-settings images
	bash -c '. localdev/lib/auth.sh && $(COMPOSE) up -d'
	@until curl -fsS -o /dev/null http://localhost:$${API_HOST_PORT:-8081}/health; do sleep 1; done
	@until curl -fsS -o /dev/null http://localhost:8084/health; do sleep 1; done
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

# make dev, plus agent-launcher and cloud-agent, on the providers .env names, as host processes, with the
# settings naming the running tunnels and the images under IMAGE_TAG, which the maintainers'
# deployment tooling (not in this repository) pushes after a change to them. Needs
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

reset:
	$(MAKE) controlplane-down
	$(MAKE) controlplane-up

# ---- Cluster agent: one per target Kubernetes cluster running jobs ----------
# Installs/removes the runtime/k8s/infra/chart helm release (node-agent DaemonSet + cluster-agent
# Deployment, the only component with real k8s credentials) on the cluster KUBE_CONTEXT/
# KUBECONFIG_PATH point at (left empty: the current context of ~/.kube/config), pulling the images
# make images pushed to REGISTRY under TAG.
# RUNTIME_TOKEN comes from the environment and is passed on by name, so neither this recipe's
# echo nor a process list shows it.
# BUILDER=true also builds environments there (install.sh's BUILDER).
# BUILDER_NODE_IMAGES, the builder node's containerd socket, also loads each build into that node.
# INSECURE_REGISTRY=true has every node pull from REGISTRY over plain HTTP (install.sh's), the
# default while REGISTRY_TLS_VERIFY=false, as with the dev registry. For a cluster on another
# machine, name the dev registry as that machine reaches it: REGISTRY=<LOCAL_REACHABLE_ADDRESS>:5000.
# INSTALL_FLAGS=--dry-run runs install.sh --dry-run: the plan and the checks from inside the
# cluster, changing nothing.
# Usage: RUNTIME_TOKEN="$$(make -s runtime-token CLUSTER=<name>)" make cluster-agent-up CLUSTER=<name> API_URL=... METRICS_URL=... [REGISTRY=...] [TAG=...] [KUBECONFIG_PATH=...] [KUBE_CONTEXT=...] [BUILDER=true [BUILDER_NODE_IMAGES=<containerd socket>]] [INSECURE_REGISTRY=true|false] [INSTALL_FLAGS=--dry-run]
INSECURE_REGISTRY ?= $(if $(filter false,$(REGISTRY_TLS_VERIFY)),true,false)
cluster-agent-up:
	@test -n "$(CLUSTER)" || { echo "make cluster-agent-up: CLUSTER=<name> is required, the name the cluster was registered under" >&2; exit 1; }
	@test -n "$(API_URL)" || { echo "make cluster-agent-up: API_URL is required, the control-plane API as a pod in that cluster reaches it" >&2; exit 1; }
	@test -n "$(METRICS_URL)" || { echo "make cluster-agent-up: METRICS_URL is required, the metrics service as every node in that cluster reaches it" >&2; exit 1; }
	@test -n "$(TAG)" || { echo "make cluster-agent-up: TAG is required, the image tag make images pushed" >&2; exit 1; }
	@test -n "$$RUNTIME_TOKEN" || { echo "make cluster-agent-up: RUNTIME_TOKEN must be in the environment, the token make -s runtime-token CLUSTER=$(CLUSTER) printed" >&2; exit 1; }
	CLUSTER_NAME="$(CLUSTER)" API_URL="$(API_URL)" METRICS_URL="$(METRICS_URL)" REGISTRY_URL="$(REGISTRY)" TAG="$(TAG)" \
	  RUNTIME_TOKEN="$$RUNTIME_TOKEN" KUBECONFIG_PATH="$(KUBECONFIG_PATH)" KUBE_CONTEXT="$(KUBE_CONTEXT)" \
	  BUILDER="$(BUILDER)" BUILDER_NODE_IMAGES="$(BUILDER_NODE_IMAGES)" INSECURE_REGISTRY="$(INSECURE_REGISTRY)" bash runtime/k8s/infra/install.sh $(INSTALL_FLAGS)

cluster-agent-down:
	bash runtime/k8s/infra/destroy.sh

# ---- Local dev cluster bootstrap --------------------------------------------
# Spins up a local k3s cluster (control-plane only, tainted no-workload — see install.sh), then
# installs the cluster-agent bundle onto it (localdev/k3s-macos/install.sh calls
# runtime/k8s/infra/install.sh itself) and provisions the dev nodes (see k3s-dev-nodes-up below),
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

# The e2e suite against the stack `make dev` runs and the local k3s cluster, in one command
# (localdev/e2e.sh). It refuses a stack that rents compute (make dev-hosted) and one somebody
# else holds, brings the dev nodes up, and leaves the nodes, the API and the UI as it found them.
# The cluster's runtime and the test workload are images: `make reload` first when they changed.
# Usage: make e2e                                        every test that needs no real accelerator
#        make e2e E2E_ARGS='e2e/test_accounts.py -x'      what pytest is given instead
e2e:
	bash localdev/e2e.sh $(E2E_ARGS)

# What a suite run on another host's stack (tt-e2e) is started with: the API's address, a staff
# token, and what the suite's hl-admin calls read. Refused on a stack that rents, as make e2e is.
E2E_API_URL = $${API_URL:-http://localhost:$${API_HOST_PORT:-8081}}
E2E_ENV = bash $(CURDIR)/localdev/dev.sh require-local && . $(CURDIR)/localdev/lib/auth.sh && export API_URL="$(E2E_API_URL)" && export HL_TOKEN="$${HL_TOKEN:-$$(lib_hl_admin token staff)}" &&

# ---- Convenience: local cluster + control plane in one command -------------
full-up: k3s-up controlplane-up

# Pause/resume everything (podman machine, k3s, control plane) without destroying
# cluster state — much faster than full-up/k3s-down for a daily on/off cycle.
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

# Run on the box with the REGISTRY and TAG it was attached with, so its nodes pull the image just pushed.
tt-e2e: render-settings tt-hardware-image
	bash -c '$(E2E_ENV) TT_WORKLOAD_IMAGE="$(REGISTRY)/hypothesisloop-tenstorrent-workload:$(TAG)" bash localdev/k3s-tenstorrent/run-e2e.sh'

# Removes the fake nodes; the host's own node is left as it is.
tt-dev-nodes-down:
	bash localdev/k3s-tenstorrent/dev-nodes-down.sh

tt-status:
	bash localdev/k3s-tenstorrent/status.sh

# ---- Local git host: a stack whose GIT_HOST_PROVIDER is local keeps its code on the git server make dev runs ----
# One bare repository on it, which a platform experiment then names in `repository` as NAME.git:
# the platform creates none on a plain git host. Usage: make git-repository NAME=my-experiment
git-repository:
	@test -n "$(NAME)" || { echo "make git-repository: NAME is required" >&2; exit 1; }
	@bash -c '. localdev/lib/auth.sh && \
	  git_host="$$(lib_git_host_provider)" && \
	  { test "$$git_host" = local || { echo "make git-repository: this stack keeps its code on GitHub (GIT_HOST_PROVIDER=$$git_host in .env)" >&2; exit 1; }; } && \
	  git init -q --bare "$$HL_GIT_ROOT/$(NAME).git" && echo "$(NAME).git"'

tt-stop:
	bash localdev/k3s-tenstorrent/stop.sh

tt-start:
	bash localdev/k3s-tenstorrent/start.sh

# Builds every platform image and pushes it to $(REGISTRY) under two tags: $(TAG) (the tree
# content that produced it -- the one every manifest pins) and latest (a mutable convenience pointer for
# interactive dev, never referenced by a manifest). Every consumer (k3s nodes via
# registries.yaml, the Helm chart's images.registry) pulls normally instead of the old
# podman-save-pipe-ctr-import sideload, which is what left an unpinned image ID for kubelet's
# own image GC to reclaim mid-run (the incident this replaced).
# IMAGE_TAG_FILE hands TAG to the e2e tests, whose jobs must pin it: nodes pull IfNotPresent, so
# a job naming latest keeps running whatever latest was when that node first pulled it. It is
# written only by a push to the dev registry: the tests pull from there, and a publish to another
# registry would otherwise pin them to a tag the dev registry may not hold.
# job is the bare image the deployment offers a job to run in (job_images) and the one a cloud
# runtime boots, at the tag of the agents' images, so it is pushed wherever they are. It is
# job-base, which environments are built on, with the platform's tools, so job-base comes first.
IMAGE_TARGETS := control-service:controlplane/build/Dockerfile.control-service:. \
	metrics-service:controlplane/build/Dockerfile.metrics-service:. \
	node-agent:runtime/k8s/build/Dockerfile.node-agent:. \
	cluster-agent:runtime/k8s/build/Dockerfile.cluster-agent:. \
	bare-agent:runtime/bare-metal/build/Dockerfile.bare-agent:. \
	cloud-agent:runtime/cloud/build/Dockerfile.cloud-agent:. \
	agent-launcher:runtime/cloud/build/Dockerfile.agent-launcher:. \
	job-base:infra/images/job/Dockerfile.base:. \
	job:infra/images/job/Dockerfile:. \
	workload:tests/workloads/generic/Dockerfile.train:tests/workloads/generic

# The Go images build with -mod=vendor from a vendor/ that is not tracked, so it is made here
# and removed when the builds end: left behind, it goes stale with the next change to go.mod and
# plain `go build` then fails on it.
# The dev registry is started only when it is the one pushed to.
locked-images: check-tag $(if $(filter $(LOCAL_REGISTRY),$(REGISTRY)),registry-up)
	@trap 'rm -rf vendor' EXIT; go mod vendor || exit 1; \
	for entry in $(IMAGE_TARGETS); do \
		name="$${entry%%:*}"; rest="$${entry#*:}"; dockerfile="$${rest%%:*}"; ctx="$${rest#*:}"; \
		echo "==> building hypothesisloop-$$name:$(TAG)"; \
		base=""; [ "$$name" = job ] && base="--build-arg BASE=$(REGISTRY)/hypothesisloop-job-base:$(TAG)"; \
		podman build --platform $(IMAGE_PLATFORM) $$base -f "$$dockerfile" -t "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" "$$ctx" || exit 1; \
		podman tag "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" "$(REGISTRY)/hypothesisloop-$$name:latest"; \
		$(REGISTRY_LOGIN) || exit 1; \
		podman push --tls-verify=$(REGISTRY_TLS_VERIFY) "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" || exit 1; \
		podman push --tls-verify=$(REGISTRY_TLS_VERIFY) "$(REGISTRY)/hypothesisloop-$$name:latest" || exit 1; \
	done
	$(if $(filter $(LOCAL_REGISTRY),$(REGISTRY)),echo "$(TAG)" > $(IMAGE_TAG_FILE))
	$(MAKE) chart-push
	$(MAKE) prune-images TAGS="$(TAG)"

# The job images that carry a framework, built and pushed to $(REGISTRY), which a local cluster
# pulls them from. Apart from `images` because each pulls gigabytes. The deployment's registry
# gets them from the maintainers' deployment tooling.
include infra/images/job/bases.mk

job-images: check-tag
	@for entry in $(JOB_IMAGE_BASES); do \
		name="$${entry%%=*}"; rest="$${entry#*=}"; base="$${rest%%=*}"; torch=""; \
		case "$$rest" in *=*) torch="$${rest#*=}";; esac; \
		echo "==> building hypothesisloop-$$name:$(TAG) from $$base$${torch:+ with CPU torch $$torch}"; \
		podman build --platform $(IMAGE_PLATFORM) --build-arg BASE="$$base" --build-arg TORCH_CPU="$$torch" -f infra/images/job/Dockerfile.base -t "$(REGISTRY)/hypothesisloop-$$name-base:$(TAG)" . || exit 1; \
		podman build --platform $(IMAGE_PLATFORM) --build-arg BASE="$(REGISTRY)/hypothesisloop-$$name-base:$(TAG)" -f infra/images/job/Dockerfile -t "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" . || exit 1; \
		$(REGISTRY_LOGIN) || exit 1; \
		podman push --tls-verify=$(REGISTRY_TLS_VERIFY) "$(REGISTRY)/hypothesisloop-$$name-base:$(TAG)" || exit 1; \
		podman push --tls-verify=$(REGISTRY_TLS_VERIFY) "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" || exit 1; \
	done

# The runtime chart goes to the registry its images are in, as an OCI artifact under charts/, so
# a cluster is attached with one helm command and no checkout (hl cluster add prints it). Its
# version carries TAG as build metadata, which is how the printed command names the chart that
# matches the images.
chart-push: check-tag
	@package="$$(mktemp -d)" && trap 'rm -rf "$$package"' EXIT && \
	  helm package runtime/k8s/infra/chart --version "0.1.0+$(TAG)" --app-version "$(TAG)" --destination "$$package" >/dev/null && \
	  $(REGISTRY_LOGIN) && \
	  helm push "$$package"/hypothesisloop-cluster-agent-*.tgz "oci://$(REGISTRY)/charts" $(if $(filter false,$(REGISTRY_TLS_VERIFY)),--plain-http)

# The UI image is no part of `images`: nothing the local stack runs uses it (local development
# serves the UI with `npm run dev`), and it cannot be built without a sign-in service. Next.js
# compiles into the browser bundle the API address as a browser reaches it, the service that
# signs people in and the sign-in providers it offers (controlplane/build/Dockerfile.ui), so each
# is required here and has no default. All are public values. The helm chart's UI Deployment runs
# this image.
# Usage: make ui-image REGISTRY=... NEXT_PUBLIC_API_URL=... NEXT_PUBLIC_IDENTITY_PROVIDER=supabase NEXT_PUBLIC_IDENTITY_SUPABASE_URL=... NEXT_PUBLIC_IDENTITY_SUPABASE_PUBLISHABLE_KEY=... NEXT_PUBLIC_SIGN_IN_PROVIDERS=...
UI_BUILD_ARGS := NEXT_PUBLIC_API_URL NEXT_PUBLIC_IDENTITY_PROVIDER NEXT_PUBLIC_IDENTITY_SUPABASE_URL NEXT_PUBLIC_IDENTITY_SUPABASE_PUBLISHABLE_KEY NEXT_PUBLIC_SIGN_IN_PROVIDERS
check-ui-build-args:
	@$(foreach arg,$(UI_BUILD_ARGS),test -n "$($(arg))" || { echo "make ui-image: $(arg) is required (controlplane/ui/.env.example says what it is)" >&2; exit 1; };)

locked-ui-image: check-tag check-ui-build-args $(if $(filter $(LOCAL_REGISTRY),$(REGISTRY)),registry-up)
	podman build --platform $(IMAGE_PLATFORM) $(foreach arg,$(UI_BUILD_ARGS),--build-arg $(arg)="$($(arg))") \
		-f controlplane/build/Dockerfile.ui -t "$(REGISTRY)/hypothesisloop-ui:$(TAG)" controlplane/ui
	podman tag "$(REGISTRY)/hypothesisloop-ui:$(TAG)" "$(REGISTRY)/hypothesisloop-ui:latest"
	$(REGISTRY_LOGIN)
	podman push --tls-verify=$(REGISTRY_TLS_VERIFY) "$(REGISTRY)/hypothesisloop-ui:$(TAG)"
	podman push --tls-verify=$(REGISTRY_TLS_VERIFY) "$(REGISTRY)/hypothesisloop-ui:latest"
	$(MAKE) prune-images TAGS="$(TAG)"

# Shared base every experiment's own Dockerfile.experimentator FROMs (see
# agents/experimentator/Dockerfile.base's header) -- the agent harness, git/Claude Code plumbing,
# and WORKLOAD_SAMPLES, none of which is experiment-specific. Root build context (not
# agents/experimentator/) so it can bake in hl-clone and the local experiments EXPERIMENT_SAMPLES
# names (the variable, else .env) as WORKLOAD_SAMPLES.
EXPERIMENT_SAMPLES ?= $(shell test -f .env && bash -c 'set +u; . ./.env; printf %s "$$EXPERIMENT_SAMPLES"')
experimentator-base-image:
	podman build --build-arg EXPERIMENT_SAMPLES="$(EXPERIMENT_SAMPLES)" -f agents/experimentator/Dockerfile.base -t localhost/hypothesisloop-experimentator-base .

# Experiments live only on the machines that have them: agents/coordinator/experiments/ is
# git-ignored apart from hl-clone. $(call require-experiment-file,<path>) fails, naming the file,
# before anything is built when this checkout lacks it.
require-experiment-file = test -f $(1) || { echo "$(1) is missing: the target builds a local experiment that this checkout does not have (agents/coordinator/experiments/ is git-ignored)" >&2; exit 1; }

# Builds one experiment's own experimentator image on top of the base, as
# `make experimentator-image EXPERIMENT=<name>`. Each experiment owns its Dockerfile.experimentator
# (agents/coordinator/experiments/<name>/Dockerfile.experimentator) for whatever runtime source/pins
# its agent needs to read.
experimentator-image:
	@test -n "$(EXPERIMENT)" || { echo "usage: make experimentator-image EXPERIMENT=<name>" >&2; exit 1; }
	@$(call require-experiment-file,agents/coordinator/experiments/$(EXPERIMENT)/Dockerfile.experimentator)
	$(MAKE) experimentator-base-image
	podman build -f agents/coordinator/experiments/$(EXPERIMENT)/Dockerfile.experimentator \
		-t localhost/hypothesisloop-experimentator-$(EXPERIMENT) agents/coordinator/experiments/$(EXPERIMENT)

# Pre-builds a local experiment's own job image from its seed/Dockerfile.workload, so every job
# an agent submits starts with no per-job setup. Run it once before starting a platform
# experiment of it, like experimentator-image.
sparse-sdpa-experiment:
	@$(call require-experiment-file,agents/coordinator/experiments/sparse-sdpa/seed/Dockerfile.workload)

locked-sparse-sdpa-workload-image: sparse-sdpa-experiment check-tag registry-up
	podman build -f agents/coordinator/experiments/sparse-sdpa/seed/Dockerfile.workload \
		-t $(REGISTRY)/hypothesisloop-sparse-sdpa-workload:$(TAG) agents/coordinator/experiments/sparse-sdpa/seed/
	podman tag $(REGISTRY)/hypothesisloop-sparse-sdpa-workload:$(TAG) $(REGISTRY)/hypothesisloop-sparse-sdpa-workload:latest
	podman push --tls-verify=$(REGISTRY_TLS_VERIFY) $(REGISTRY)/hypothesisloop-sparse-sdpa-workload:$(TAG)
	podman push --tls-verify=$(REGISTRY_TLS_VERIFY) $(REGISTRY)/hypothesisloop-sparse-sdpa-workload:latest

# Stages the one file the helm chart renders verbatim (never edited in the chart itself, so
# there is exactly one source of truth for platform config) — run before any helm
# lint/template/install/upgrade. The database schema is not staged: control-service embeds its
# migrations and applies them at start (db.Migrate).
HELM_CHART := controlplane/infra/helm/hypothesisloop
helm-prepare:
	mkdir -p $(HELM_CHART)/files
	cp controlplane/settings/hypothesisloop.yaml $(HELM_CHART)/files/hypothesisloop.yaml

# The two ci values files are the two ways the chart is installed: every store in the cluster, and
# a database run elsewhere with the runtime processes on. `make test` renders the same two and
# loads what they render (controlplane/infra/helm/chart_test.go).
helm-lint: helm-prepare
	helm lint $(HELM_CHART) --values $(HELM_CHART)/ci/in-cluster-values.yaml
	helm lint $(HELM_CHART) --values $(HELM_CHART)/ci/external-values.yaml
	helm lint runtime/k8s/infra/chart --kube-version 1.34.0 --set-string clusterName=lint,apiURL=http://lint,metricsURL=http://lint,runtimeToken=lint \
		--set-string images.registry=$(REGISTRY),images.tag=$(TAG)

build:
	go build ./...

# The graph tests get a throwaway Memgraph: community has one database, so they would otherwise
# write next to the stack's own graph.
TEST_MEMGRAPH := hypothesisloop-test-memgraph
test:
	podman run -d --rm --name $(TEST_MEMGRAPH) -p 127.0.0.1:7688:7687 docker.io/memgraph/memgraph:3.13.1 --telemetry-enabled=false >/dev/null
	@until podman exec $(TEST_MEMGRAPH) sh -c "echo 'RETURN 1;' | mgconsole" >/dev/null 2>&1; do sleep 1; done
	go test ./... -timeout 60s; rc=$$?; podman rm -f $(TEST_MEMGRAPH) >/dev/null; exit $$rc

agent-test:
	cd agents/experimentator && uv run python -m unittest discover -s tests

# The GitHub client's checks against github.com itself, as the App .env names
# (controlplane/shared/githost/github/live_test.go). The token checks and the push check use the
# sandbox repository of TEST_GIT_HOST_GITHUB_REPOSITORY; the push check and the first-commit check
# each make a branch there and delete it. The App's key reaches the test in a file that exists
# only for the run. The creation check runs with TEST_GIT_HOST_GITHUB_CREATE=1 and creates on the
# account of TEST_GIT_HOST_GITHUB_CREATE_INSTALLATION, skipping when .env names none.
test-github-live:
	@bash -c 'set -euo pipefail; . localdev/lib/auth.sh; \
	  test "$$(lib_git_host_provider)" = github || { echo "make test-github-live: .env does not set GIT_HOST_PROVIDER=github" >&2; exit 1; }; \
	  for key in INSTALLATION REPOSITORY; do \
	    test -n "$$(lib_env_value "TEST_GIT_HOST_GITHUB_$$key")" || { echo "make test-github-live: .env does not set TEST_GIT_HOST_GITHUB_$$key" >&2; exit 1; }; \
	  done; \
	  key_file="$$(umask 077 && mktemp)"; trap "rm -f \"$$key_file\"" EXIT; \
	  printf "%s\n" "$$HYPOTHESISLOOP_GIT_HOST_GITHUB_APP_PRIVATE_KEY" > "$$key_file"; \
	  TEST_GIT_HOST_GITHUB_APP_ID="$$HL_GIT_HOST_GITHUB_APP_ID" TEST_GIT_HOST_GITHUB_KEY_FILE="$$key_file" \
	  TEST_GIT_HOST_GITHUB_INSTALLATION="$$(lib_env_value TEST_GIT_HOST_GITHUB_INSTALLATION)" \
	  TEST_GIT_HOST_GITHUB_REPOSITORY="$$(lib_env_value TEST_GIT_HOST_GITHUB_REPOSITORY)" \
	  TEST_GIT_HOST_GITHUB_CREATE_INSTALLATION="$$(lib_env_value TEST_GIT_HOST_GITHUB_CREATE_INSTALLATION)" \
	  go test ./controlplane/shared/githost/github -run Live -count=1 -v -timeout 180s'

lint:
	golangci-lint run ./...

check: build test agent-test helm-lint
	go vet ./...
	cd controlplane/ui && npm test && npm run build

# Creates the next numbered, empty migration: make db-migration NAME=add_widget_color
MIGRATIONS := controlplane/shared/db/migrations
db-migration:
	@echo "$(NAME)" | grep -Eq '^[a-z0-9_]+$$' || { echo "NAME must be lower_snake_case, e.g. make db-migration NAME=add_widget_color" >&2; exit 1; }
	@last=$$(ls $(MIGRATIONS) | sed -n 's/^\([0-9]\{4\}\)_.*\.sql$$/\1/p' | sort | tail -1); \
	file=$(MIGRATIONS)/$$(printf '%04d' $$(expr $$last + 1))_$(NAME).sql; \
	printf -- '-- Keep the previous release working against this schema (README, Database).\n' > $$file; \
	echo $$file
