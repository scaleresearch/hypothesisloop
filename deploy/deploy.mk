.PHONY: cluster-agent-up cluster-agent-down job-images chart-push check-ui-build-args \
	harness-base-image helm-prepare helm-lint

# ---- Cluster agent: one per target Kubernetes cluster running jobs ----------
# Installs/removes the deploy/charts/hypothesisloop-cluster-agent helm release (node-agent DaemonSet + cluster-agent
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
	  BUILDER="$(BUILDER)" BUILDER_NODE_IMAGES="$(BUILDER_NODE_IMAGES)" INSECURE_REGISTRY="$(INSECURE_REGISTRY)" bash deploy/charts/hypothesisloop-cluster-agent/install.sh $(INSTALL_FLAGS)

cluster-agent-down:
	bash deploy/charts/hypothesisloop-cluster-agent/destroy.sh

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
	job-base:runtime/job/build/Dockerfile.job-base:. \
	job:runtime/job/build/Dockerfile.job:. \
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
		args=""; case "$$name" in \
			job) args="--build-arg BASE=$(REGISTRY)/hypothesisloop-job-base:$(TAG)";; \
			*-agent) args="--build-arg VERSION=$(TAG)";; \
		esac; \
		podman build --platform $(IMAGE_PLATFORM) $$args -f "$$dockerfile" -t "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" "$$ctx" || exit 1; \
		podman tag "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" "$(REGISTRY)/hypothesisloop-$$name:latest"; \
		$(REGISTRY_LOGIN) || exit 1; \
		podman push --tls-verify=$(REGISTRY_TLS_VERIFY) "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" || exit 1; \
		podman push --tls-verify=$(REGISTRY_TLS_VERIFY) "$(REGISTRY)/hypothesisloop-$$name:latest" || exit 1; \
	done
	$(if $(filter $(LOCAL_REGISTRY),$(REGISTRY)),echo "$(TAG)" > $(IMAGE_TAG_FILE))
	$(MAKE) chart-push
	$(MAKE) prune-images TAGS="$(TAG)"

# The job images that carry a framework, built and pushed to $(REGISTRY), which a local cluster
# pulls them from. Apart from `images` because each pulls gigabytes.
include runtime/job/build/bases.mk

job-images: check-tag
	@for entry in $(JOB_IMAGE_BASES); do \
		name="$${entry%%=*}"; rest="$${entry#*=}"; base="$${rest%%=*}"; torch=""; \
		case "$$rest" in *=*) torch="$${rest#*=}";; esac; \
		echo "==> building hypothesisloop-$$name:$(TAG) from $$base$${torch:+ with CPU torch $$torch}"; \
		podman build --platform $(IMAGE_PLATFORM) --build-arg BASE="$$base" --build-arg TORCH_CPU="$$torch" -f runtime/job/build/Dockerfile.job-base -t "$(REGISTRY)/hypothesisloop-$$name-base:$(TAG)" . || exit 1; \
		podman build --platform $(IMAGE_PLATFORM) --build-arg BASE="$(REGISTRY)/hypothesisloop-$$name-base:$(TAG)" -f runtime/job/build/Dockerfile.job -t "$(REGISTRY)/hypothesisloop-$$name:$(TAG)" . || exit 1; \
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
	  helm package deploy/charts/hypothesisloop-cluster-agent --version "0.1.0+$(TAG)" --app-version "$(TAG)" --destination "$$package" >/dev/null && \
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

# The image every hosted agent runs (agents/harness/Dockerfile.base's header): the agent
# harness, git/Claude Code plumbing, and cli/reference as HL_REFERENCE. Root build context so it
# can bake in cli/reference and hl-clone.
harness-base-image:
	podman build -f agents/harness/Dockerfile.base -t localhost/hypothesisloop-harness-base .

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
