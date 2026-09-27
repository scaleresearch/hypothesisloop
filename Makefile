.PHONY: $(addprefix locked-,images ui-image tt-hardware-image sparse-sdpa-workload-image)
.PHONY: dev reset build test agent-test lint check images job-images sparse-sdpa-workload-image \
	experimentator-base-image experimentator-image check-tag ui-image check-ui-build-args registry-up prune-images \
	helm-prepare helm-lint chart-push \
	controlplane-up controlplane-down controlplane-destroy render-settings \
	cluster-agent-up cluster-agent-down staff-token token runtime-token \
	k3s-up k3s-down full-up k3s-dev-nodes-up k3s-dev-nodes-down e2e \
	full-stop full-start reload stack-not-held \
	tt-up tt-down tt-status tt-stop tt-start tt-dev-nodes-up tt-dev-nodes-down tt-hardware-image tt-e2e tt-reload \
	git-daemon-start git-daemon-stop git-daemon-status git-daemon-destroy

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
export HOST_ADDRESS REGISTRY_PUBLISH_IP REGISTRY TAG
# The settings file names this variable and never holds the secret. Local development's value
# lives in localdev/lib/auth.sh and reaches compose, the scripts and the tests from here.
export HYPOTHESISLOOP_SIGNING_SECRET ?= $(shell bash -c '. localdev/lib/auth.sh && printf %s "$$HYPOTHESISLOOP_SIGNING_SECRET"')
REGISTRY_TLS_VERIFY ?= false
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
# and Linux hosts. This renders HOST_ADDRESS in at start-up; the control plane rejects a
# loopback endpoint outright, so a wrong address fails here, not hours into a run with nothing saved.
# HOST_ADDRESS_FILE hands the same address to the scripts and tests that run outside make.
# Everything that runs hl-admin depends on this: it reads the rendered settings, since the
# committed file is refused at load for its placeholders. The recipe prints nothing, so a
# target whose output is a token stays just the token.
render-settings:
	@test -n "$(HOST_ADDRESS)" || { echo "make: could not detect this host's address" >&2; exit 1; }
	@bash -c '. localdev/lib/auth.sh && lib_render_settings "$(HOST_ADDRESS)" $(RENDERED_SETTINGS)'
	@echo "$(HOST_ADDRESS)" > $(HOST_ADDRESS_FILE)

registry-up:
	podman compose -f $(COMPOSE_FILE) up -d registry
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

# compose is started under auth.sh, which exports the GitHub App's secrets when .env has an App:
# a PEM does not survive being a make variable.
controlplane-up: registry-up render-settings images
	bash -c '. localdev/lib/auth.sh && podman compose -f $(COMPOSE_FILE) up -d'
	@until curl -fsS -o /dev/null http://localhost:$${API_HOST_PORT:-8081}/health; do sleep 1; done
	@until curl -fsS -o /dev/null http://localhost:8084/health; do sleep 1; done
	bash -c '. localdev/lib/auth.sh && lib_register_local_clusters'

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

# Cloudflare quick tunnels to this machine's API, metrics-service and object store, which is how
# a hosted agent and a rented instance reach them. They keep running across restarts of the dev
# stack, and their addresses change only when they are started again.
tunnels:
	bash localdev/local-tunnels/tunnels.sh up

tunnels-down:
	bash localdev/local-tunnels/tunnels.sh down

# make dev, plus agent-launcher (Fly.io) and cloud-agent (vast.ai) as host processes, with the
# settings naming the running tunnels and the images `make -C infra publish` pushed. Needs
# `make tunnels` first. From here hosted agents and jobs placed on the cloud cluster rent
# machines, which cost money.
dev-hosted: render-settings
	bash localdev/dev.sh hosted

# Every request to the control plane carries a token. These two print the ones local work needs,
# derived from auth.signing_secret in the settings file (localdev/lib/auth.sh).
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
	podman compose -f $(COMPOSE_FILE) down

# Irreversible: deletes the postgres + GreptimeDB + Memgraph + MinIO volumes (every platform
# experiment, experiment record, metric, hypothesis and stored checkpoint). Prompts unless CONFIRM=--yes.
controlplane-destroy:
	@if [ "$(CONFIRM)" != "--yes" ]; then \
		echo "make controlplane-destroy: this DELETES ALL control-plane data. There is no backup and no undo." >&2; \
		echo "  Re-run with CONFIRM=--yes to proceed." >&2; \
		exit 1; \
	fi
	podman compose -f $(COMPOSE_FILE) down --volumes

reset:
	$(MAKE) controlplane-down
	$(MAKE) controlplane-up

# ---- Cluster agent: one per target Kubernetes cluster running jobs ----------
# Installs/removes the runtime/k8s/infra/chart helm release (node-agent DaemonSet + cluster-agent
# Deployment, the only component with real k8s credentials) on the cluster KUBE_CONTEXT/
# KUBECONFIG_PATH point at (or current kubectl context).
# RUNTIME_TOKEN comes from the environment and is passed on through it, so neither this recipe's
# echo nor a process list shows it.
# Usage: RUNTIME_TOKEN="$$(make -s runtime-token CLUSTER=<name>)" make cluster-agent-up CLUSTER=<name> API_URL=... METRICS_URL=... [KUBECONFIG_PATH=...] [KUBE_CONTEXT=...]
cluster-agent-up:
	CLUSTER_NAME="$(CLUSTER)" API_URL="$(API_URL)" METRICS_URL="$(METRICS_URL)" REGISTRY_URL="$(REGISTRY)" \
	  bash runtime/k8s/infra/install.sh

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
# fake-accelerator nodes. Also control-plane-only by default (tainted no-workload) — see
# tt-dev-nodes-up below.
tt-up: render-settings images
	bash localdev/k3s-tenstorrent/install.sh

# Destructive the same way k3s-down is (see its comment) — tears down the whole k3s cluster on
# this Tenstorrent host, not just this repo's containers. Use `make tt-stop`/`tt-start` to
# pause/resume without losing cluster state.
tt-down:
	bash localdev/k3s-tenstorrent/destroy.sh

# Provisions fake-accelerator container nodes. box-a itself stays detached; the hardware
# e2e test attaches it while it runs. Idempotent.
tt-dev-nodes-up:
	bash localdev/k3s-tenstorrent/dev-nodes-up.sh

# Builds and pushes the Tenstorrent hardware workload image to $(REGISTRY) -- the box-a
# k3s node pulls it normally (registries.yaml, see localdev/lib/node.sh), same as every other
# image; no more local save/import.
locked-tt-hardware-image: check-tag
	podman build --platform $(IMAGE_PLATFORM) -f tests/workloads/tenstorrent/Dockerfile.train -t $(REGISTRY)/hypothesisloop-tenstorrent-workload:$(TAG) tests/workloads/tenstorrent/
	podman tag $(REGISTRY)/hypothesisloop-tenstorrent-workload:$(TAG) $(REGISTRY)/hypothesisloop-tenstorrent-workload:latest
	podman push --tls-verify=$(REGISTRY_TLS_VERIFY) $(REGISTRY)/hypothesisloop-tenstorrent-workload:$(TAG)
	podman push --tls-verify=$(REGISTRY_TLS_VERIFY) $(REGISTRY)/hypothesisloop-tenstorrent-workload:latest

tt-e2e: render-settings tt-hardware-image
	bash -c '$(E2E_ENV) bash localdev/k3s-tenstorrent/run-e2e.sh'

# Counterpart to `reload` for the real Tenstorrent host.
tt-reload: render-settings images
	bash localdev/k3s-tenstorrent/reload.sh

# Removes the fake nodes and detaches box-a, returning the cluster to control-plane-only.
tt-dev-nodes-down:
	bash localdev/k3s-tenstorrent/dev-nodes-down.sh

tt-status:
	bash localdev/k3s-tenstorrent/status.sh

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
	bash localdev/k3s-tenstorrent/stop.sh

tt-start:
	bash localdev/k3s-tenstorrent/start.sh

# Builds every platform image and pushes it to $(REGISTRY) under two tags: $(TAG) (the tree
# content that produced it -- the one every manifest pins) and latest (a mutable convenience pointer for
# interactive dev, never referenced by a manifest). Every consumer (k3s nodes via
# registries.yaml, the Helm chart's images.registry) pulls normally instead of the old
# podman-save-pipe-ctr-import sideload, which is what left an unpinned image ID for kubelet's
# own image GC to reclaim mid-run (see the fix-later.md incident this replaced).
# IMAGE_TAG_FILE hands TAG to the e2e tests, whose jobs must pin it: nodes pull IfNotPresent, so
# a job naming latest keeps running whatever latest was when that node first pulled it. It is
# written only by a push to the dev registry: the tests pull from there, and a publish to another
# registry would otherwise pin them to a tag the dev registry may not hold.
# job is the bare image the deployment offers a job to run in (job_images) and the one a cloud
# runtime boots, at the tag of the agents' images, so it is pushed wherever they are.
IMAGE_TARGETS := control-service:controlplane/build/Dockerfile.control-service:. \
	metrics-service:controlplane/build/Dockerfile.metrics-service:. \
	node-agent:runtime/k8s/build/Dockerfile.node-agent:. \
	cluster-agent:runtime/k8s/build/Dockerfile.cluster-agent:. \
	bare-agent:runtime/bare-metal/build/Dockerfile.bare-agent:. \
	cloud-agent:runtime/cloud/build/Dockerfile.cloud-agent:. \
	agent-launcher:runtime/cloud/build/Dockerfile.agent-launcher:. \
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
		podman build --platform $(IMAGE_PLATFORM) -f "$$dockerfile" -t "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" "$$ctx" || exit 1; \
		podman tag "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" "$(REGISTRY)/hypothesisloop-$$name:latest"; \
		podman push --tls-verify=$(REGISTRY_TLS_VERIFY) "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" || exit 1; \
		podman push --tls-verify=$(REGISTRY_TLS_VERIFY) "$(REGISTRY)/hypothesisloop-$$name:latest" || exit 1; \
	done
	$(if $(filter $(LOCAL_REGISTRY),$(REGISTRY)),echo "$(TAG)" > $(IMAGE_TAG_FILE))
	$(MAKE) chart-push
	$(MAKE) prune-images TAGS="$(TAG)"

# The job images that carry a framework (job_images in the settings file): the upstream image
# with the job image's own tools added. Apart from `images` because each pulls gigabytes.
JOB_IMAGE_BASES := job-pytorch-cuda=docker.io/pytorch/pytorch@sha256:68c022c2f4627943a6f3e574cfd2c8ae4256210d5f66ae2b117942e0a8d4fa9d \
	job-tenstorrent=ghcr.io/tenstorrent/tt-metal/tt-metalium-ubuntu-22.04-release-amd64:v0.77.0

job-images: check-tag
	@for entry in $(JOB_IMAGE_BASES); do \
		name="$${entry%%=*}"; base="$${entry#*=}"; \
		echo "==> building hypothesisloop-$$name:$(TAG) from $$base"; \
		podman build --platform $(IMAGE_PLATFORM) --build-arg BASE="$$base" -f infra/images/job/Dockerfile -t "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" . || exit 1; \
		podman push --tls-verify=$(REGISTRY_TLS_VERIFY) "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" || exit 1; \
	done

# The runtime chart goes to the registry its images are in, as an OCI artifact under charts/, so
# a cluster is attached with one helm command and no checkout (hl cluster add prints it). Its
# version carries TAG as build metadata, which is how the printed command names the chart that
# matches the images.
chart-push: check-tag
	@package="$$(mktemp -d)" && trap 'rm -rf "$$package"' EXIT && \
	  helm package runtime/k8s/infra/chart --version "0.1.0+$(TAG)" --app-version "$(TAG)" --destination "$$package" >/dev/null && \
	  helm push "$$package"/hypothesisloop-cluster-agent-*.tgz "oci://$(REGISTRY)/charts" $(if $(filter false,$(REGISTRY_TLS_VERIFY)),--plain-http)

# The UI image is no part of `images`: nothing the local stack runs uses it (local development
# serves the UI with `npm run dev`), and it cannot be built without a Supabase project. Next.js
# compiles into the browser bundle the API address as a browser reaches it and the project that
# signs people in (controlplane/build/Dockerfile.ui), so all three are required here and have no
# default. All three are public values. The helm chart's UI Deployment runs this image.
# Usage: make ui-image REGISTRY=... NEXT_PUBLIC_API_URL=... NEXT_PUBLIC_SUPABASE_URL=... NEXT_PUBLIC_SUPABASE_PUBLISHABLE_KEY=...
UI_BUILD_ARGS := NEXT_PUBLIC_API_URL NEXT_PUBLIC_SUPABASE_URL NEXT_PUBLIC_SUPABASE_PUBLISHABLE_KEY
check-ui-build-args:
	@$(foreach arg,$(UI_BUILD_ARGS),test -n "$($(arg))" || { echo "make ui-image: $(arg) is required (controlplane/ui/.env.example says what it is)" >&2; exit 1; };)

locked-ui-image: check-tag check-ui-build-args $(if $(filter $(LOCAL_REGISTRY),$(REGISTRY)),registry-up)
	podman build --platform $(IMAGE_PLATFORM) $(foreach arg,$(UI_BUILD_ARGS),--build-arg $(arg)="$($(arg))") \
		-f controlplane/build/Dockerfile.ui -t "$(REGISTRY)/hypothesisloop-ui:$(TAG)" controlplane/ui
	podman tag "$(REGISTRY)/hypothesisloop-ui:$(TAG)" "$(REGISTRY)/hypothesisloop-ui:latest"
	podman push --tls-verify=$(REGISTRY_TLS_VERIFY) "$(REGISTRY)/hypothesisloop-ui:$(TAG)"
	podman push --tls-verify=$(REGISTRY_TLS_VERIFY) "$(REGISTRY)/hypothesisloop-ui:latest"
	$(MAKE) prune-images TAGS="$(TAG)"

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
locked-sparse-sdpa-workload-image: check-tag registry-up
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
	mkdir -p $(HELM_CHART)/files
	cp controlplane/settings/hypothesisloop.yaml $(HELM_CHART)/files/hypothesisloop.yaml

# The two ci values files are the two ways the chart is installed: every store in the cluster, and
# a database run elsewhere with the runtime processes on. `make test` renders the same two and
# loads what they render (controlplane/infra/helm/chart_test.go).
helm-lint: helm-prepare
	helm lint $(HELM_CHART) --values $(HELM_CHART)/ci/in-cluster-values.yaml
	helm lint $(HELM_CHART) --values $(HELM_CHART)/ci/external-values.yaml
	helm lint runtime/k8s/infra/chart --set-string clusterName=lint,apiURL=http://lint,metricsURL=http://lint,runtimeToken=lint \
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
# (controlplane/shared/github/github_live_test.go). Both the token checks and the push check use
# the sandbox repository of HL_E2E_GITHUB_REPO; the push check pushes a branch there and deletes
# it. The App's key reaches the test in a file that exists only for the run.
test-github-live:
	@bash -c 'set -euo pipefail; . localdev/lib/auth.sh; \
	  test -n "$$HL_GITHUB_APP_ID" || { echo "make test-github-live: .env does not set GITHUB_APP_ID" >&2; exit 1; }; \
	  for key in INSTALLATION REPO; do \
	    test -n "$$(lib_env_value "HL_E2E_GITHUB_$$key")" || { echo "make test-github-live: .env does not set HL_E2E_GITHUB_$$key" >&2; exit 1; }; \
	  done; \
	  key_file="$$(umask 077 && mktemp)"; trap "rm -f \"$$key_file\"" EXIT; \
	  printf "%s\n" "$$HYPOTHESISLOOP_GITHUB_APP_PRIVATE_KEY" > "$$key_file"; \
	  HYPOTHESISLOOP_GITHUB_LIVE_APP_ID="$$HL_GITHUB_APP_ID" HYPOTHESISLOOP_GITHUB_LIVE_KEY_FILE="$$key_file" \
	  HYPOTHESISLOOP_GITHUB_LIVE_INSTALLATION="$$(lib_env_value HL_E2E_GITHUB_INSTALLATION)" \
	  HYPOTHESISLOOP_GITHUB_LIVE_REPOSITORY="$$(lib_env_value HL_E2E_GITHUB_REPO)" \
	  HYPOTHESISLOOP_GITHUB_LIVE_PUSH_REPOSITORY="$$(lib_env_value HL_E2E_GITHUB_REPO)" \
	  go test ./controlplane/shared/github -run Live -count=1 -v -timeout 180s'

lint:
	golangci-lint run ./...

check: build test agent-test helm-lint
	go vet ./...
	cd controlplane/ui && npm test && npm run build
