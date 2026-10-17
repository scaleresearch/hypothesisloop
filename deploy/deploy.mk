.PHONY: cluster-agent-up cluster-agent-down check-ui-build-args locked-publish product-images \
	helm-prepare helm-lint

# ---- Cluster agent: one per target Kubernetes cluster running jobs ----------
# Installs/removes the deploy/charts/hypothesisloop-cluster-agent helm release (node-agent DaemonSet + cluster-agent
# Deployment, the only component with real k8s credentials) on the cluster KUBE_CONTEXT/
# KUBECONFIG_PATH point at (left empty: the current context of ~/.kube/config), pulling the images
# make publish or make local-images pushed to REGISTRY under TAG.
# RUNTIME_TOKEN comes from the environment and is passed on by name, so neither this recipe's
# echo nor a process list shows it.
# BUILDER=true also builds environments there (install.sh's BUILDER).
# BUILDER_NODE_IMAGES, the builder node's containerd socket, also loads each build into that node.
# INSECURE_REGISTRY=true has every node pull from REGISTRY over plain HTTP (install.sh's), the
# default while REGISTRY_TLS_VERIFY=false, as with the dev registry. For a cluster on another
# machine, name the dev registry as that machine reaches it and say it serves plain HTTP:
# REGISTRY=<LOCAL_REACHABLE_ADDRESS>:5000 INSECURE_REGISTRY=true.
# INSTALL_FLAGS=--dry-run runs install.sh --dry-run: the plan and the checks from inside the
# cluster, changing nothing.
# Usage: RUNTIME_TOKEN="$$(make -s runtime-token CLUSTER=<name>)" make cluster-agent-up CLUSTER=<name> API_URL=... METRICS_URL=... [REGISTRY=...] [TAG=...] [KUBECONFIG_PATH=...] [KUBE_CONTEXT=...] [BUILDER=true [BUILDER_NODE_IMAGES=<containerd socket>]] [INSECURE_REGISTRY=true|false] [INSTALL_FLAGS=--dry-run]
INSECURE_REGISTRY ?= $(if $(filter false,$(REGISTRY_TLS_VERIFY)),true,false)
cluster-agent-up:
	@test -n "$(CLUSTER)" || { echo "make cluster-agent-up: CLUSTER=<name> is required, the name the cluster was registered under" >&2; exit 1; }
	@test -n "$(API_URL)" || { echo "make cluster-agent-up: API_URL is required, the control-plane API as a pod in that cluster reaches it" >&2; exit 1; }
	@test -n "$(METRICS_URL)" || { echo "make cluster-agent-up: METRICS_URL is required, the metrics service as every node in that cluster reaches it" >&2; exit 1; }
	@test -n "$(TAG)" || { echo "make cluster-agent-up: TAG is required, the image tag make publish or make local-images pushed" >&2; exit 1; }
	@test -n "$$RUNTIME_TOKEN" || { echo "make cluster-agent-up: RUNTIME_TOKEN must be in the environment, the token make -s runtime-token CLUSTER=$(CLUSTER) printed" >&2; exit 1; }
	CLUSTER_NAME="$(CLUSTER)" API_URL="$(API_URL)" METRICS_URL="$(METRICS_URL)" REGISTRY_URL="$(REGISTRY)" TAG="$(TAG)" \
	  RUNTIME_TOKEN="$$RUNTIME_TOKEN" KUBECONFIG_PATH="$(KUBECONFIG_PATH)" KUBE_CONTEXT="$(KUBE_CONTEXT)" \
	  BUILDER="$(BUILDER)" BUILDER_NODE_IMAGES="$(BUILDER_NODE_IMAGES)" INSECURE_REGISTRY="$(INSECURE_REGISTRY)" bash deploy/charts/hypothesisloop-cluster-agent/install.sh $(INSTALL_FLAGS)

cluster-agent-down:
	bash deploy/charts/hypothesisloop-cluster-agent/destroy.sh

# One image: hypothesisloop-NAME built from IMAGE_DOCKERFILE_NAME in IMAGE_CONTEXT_NAME (the
# root when unset) with IMAGE_ARGS_NAME, and pushed to $(REGISTRY) under $(TAG) alone, the tree
# content that produced it, so a pinned tag never starts running other bytes under IfNotPresent.
define push_image
$(CONTAINER_ENGINE) build --platform $(IMAGE_PLATFORM) $(IMAGE_ARGS_$(1)) -f $(IMAGE_DOCKERFILE_$(1)) -t "$(REGISTRY)/hypothesisloop-$(1):$(TAG)" $(or $(IMAGE_CONTEXT_$(1)),.)
$(REGISTRY_LOGIN)
$(CONTAINER_ENGINE) push $(if $(filter podman,$(CONTAINER_ENGINE)),--tls-verify=$(REGISTRY_TLS_VERIFY)) "$(REGISTRY)/hypothesisloop-$(1):$(TAG)"
endef

# Every image the product runs, in build order: job is job-base with the platform's tools, and
# each framework job image of bases.mk is its own -base the same way.
include runtime/job/build/bases.mk
define framework_job
IMAGE_DOCKERFILE_$(word 1,$(1))-base := runtime/job/build/Dockerfile.job-base
IMAGE_ARGS_$(word 1,$(1))-base := --build-arg BASE=$(word 2,$(1)) --build-arg TORCH_CPU=$(word 3,$(1))
IMAGE_DOCKERFILE_$(word 1,$(1)) := runtime/job/build/Dockerfile.job
IMAGE_ARGS_$(word 1,$(1)) = --build-arg BASE=$$(REGISTRY)/hypothesisloop-$(word 1,$(1))-base:$$(TAG)
FRAMEWORK_JOB_IMAGES += $(word 1,$(1))-base $(word 1,$(1))
endef
$(foreach entry,$(JOB_IMAGE_BASES),$(eval $(call framework_job,$(subst =, ,$(entry)))))

PRODUCT_IMAGES := control-service metrics-service ui harness-base node-agent cluster-agent bare-agent \
	job-base job $(FRAMEWORK_JOB_IMAGES)
IMAGE_DOCKERFILE_control-service := controlplane/build/Dockerfile.control-service
IMAGE_DOCKERFILE_metrics-service := controlplane/build/Dockerfile.metrics-service
IMAGE_DOCKERFILE_harness-base := agents/harness/Dockerfile.base
IMAGE_DOCKERFILE_node-agent := runtime/k8s/build/Dockerfile.node-agent
IMAGE_ARGS_node-agent = --build-arg VERSION=$(TAG)
IMAGE_DOCKERFILE_cluster-agent := runtime/k8s/build/Dockerfile.cluster-agent
IMAGE_ARGS_cluster-agent = --build-arg VERSION=$(TAG)
IMAGE_DOCKERFILE_bare-agent := runtime/bare-metal/build/Dockerfile.bare-agent
IMAGE_ARGS_bare-agent = --build-arg VERSION=$(TAG)
IMAGE_DOCKERFILE_job-base := runtime/job/build/Dockerfile.job-base
IMAGE_DOCKERFILE_job := runtime/job/build/Dockerfile.job
IMAGE_ARGS_job = --build-arg BASE=$(REGISTRY)/hypothesisloop-job-base:$(TAG)
# Next.js compiles into the browser bundle the API address as a browser reaches it, the service
# that signs people in and the sign-in providers it offers (controlplane/build/Dockerfile.ui), so
# each is required and has no default. All are public values.
UI_BUILD_ARGS := NEXT_PUBLIC_API_URL NEXT_PUBLIC_IDENTITY_PROVIDER NEXT_PUBLIC_IDENTITY_SUPABASE_URL NEXT_PUBLIC_IDENTITY_SUPABASE_PUBLISHABLE_KEY NEXT_PUBLIC_SIGN_IN_PROVIDERS
IMAGE_DOCKERFILE_ui := controlplane/build/Dockerfile.ui
IMAGE_CONTEXT_ui := controlplane/ui
IMAGE_ARGS_ui = $(foreach arg,$(UI_BUILD_ARGS),--build-arg $(arg)="$($(arg))")

.PHONY: $(addprefix image-,$(PRODUCT_IMAGES))
$(addprefix image-,$(PRODUCT_IMAGES)): image-%: check-tag
	$(call push_image,$*)

check-ui-build-args:
	@$(foreach arg,$(UI_BUILD_ARGS),test -n "$($(arg))" || { echo "make: $(arg) is required to build the UI image (controlplane/ui/.env.example says what it is)" >&2; exit 1; };)

# The Go images build with -mod=vendor from a vendor/ that is not tracked, so it is made here
# and removed when the builds end: left behind, it goes stale with the next change to go.mod and
# plain `go build` then fails on it. GO_VENDOR is how it is made.
GO_VENDOR ?= go mod vendor

# The one publish: every product image and both charts, all under TAG. Each chart's version is
# its own with +TAG as build metadata and its appVersion is TAG, which is the image tag it
# installs unless told another, so the chart names the images it was published with. Charts go
# to $(REGISTRY)/charts as OCI artifacts. The charts are linted first, so a chart that does not
# render fails the publish before any image is built.
# Usage: make publish REGISTRY=... TAG=... NEXT_PUBLIC_API_URL=... NEXT_PUBLIC_IDENTITY_PROVIDER=supabase NEXT_PUBLIC_IDENTITY_SUPABASE_URL=... NEXT_PUBLIC_IDENTITY_SUPABASE_PUBLISHABLE_KEY=... NEXT_PUBLIC_SIGN_IN_PROVIDERS=...
CHARTS := deploy/charts/hypothesisloop deploy/charts/hypothesisloop-cluster-agent
locked-publish: check-tag check-ui-build-args helm-lint
	$(GO_VENDOR)
	$(MAKE) -j1 --no-print-directory $(addprefix image-,$(PRODUCT_IMAGES)) || { rm -rf vendor; exit 1; }
	rm -rf vendor
	@charts="$$(mktemp -d)" && trap 'rm -rf "$$charts"' EXIT && \
	for chart in $(CHARTS); do \
		version="$$(awk '$$1 == "version:" { print $$2 }' "$$chart/Chart.yaml")" && \
		helm package "$$chart" --version "$$version+$(TAG)" --app-version "$(TAG)" --destination "$$charts" >/dev/null || exit 1; \
	done && \
	$(REGISTRY_LOGIN) && \
	for archive in "$$charts"/*.tgz; do \
		helm push "$$archive" "oci://$(REGISTRY)/charts" $(if $(filter false,$(REGISTRY_TLS_VERIFY)),--plain-http) || exit 1; \
	done

# The names publish builds, for a caller that handles each image after it.
product-images:
	@echo $(PRODUCT_IMAGES)

# Stages the one file the helm chart renders verbatim (never edited in the chart itself, so
# there is exactly one source of truth for platform config) — run before any helm
# lint/template/install/upgrade. The database schema is not staged: control-service embeds
# schema.sql and builds it at start (db.EnsureSchema).
HELM_CHART := deploy/charts/hypothesisloop
helm-prepare:
	mkdir -p $(HELM_CHART)/files
	cp controlplane/settings/hypothesisloop.yaml $(HELM_CHART)/files/hypothesisloop.yaml

# The two ci values files are the two ways the chart is installed: every store in the cluster, and
# a database run elsewhere with the runtime processes on. `make test` renders the same two and
# loads what they render (deploy/charts/hypothesisloop/chart_test.go).
helm-lint: helm-prepare
	helm lint $(HELM_CHART) --values $(HELM_CHART)/ci/in-cluster-values.yaml
	helm lint $(HELM_CHART) --values $(HELM_CHART)/ci/external-values.yaml
	helm lint deploy/charts/hypothesisloop-cluster-agent --kube-version 1.34.0 --set-string clusterName=lint,apiURL=http://lint,metricsURL=http://lint,runtimeToken=lint \
		--set-string images.registry=$(REGISTRY),images.tag=$(TAG)
