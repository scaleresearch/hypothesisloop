.PHONY: check-tag render-settings build lint check publish local-images $(addprefix locked-,publish local-images)

# Shared by every area's targets: where the images go and under which tag, the local stack's
# address and settings, and the build lock. Each area's targets live in its own .mk file.
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
# The ports the control service and metrics-service listen on, which the settings name
# (localdev/lib/env.sh). Exported for the compose file and the scripts make runs.
HL_API_PORT := $(shell bash -c '. localdev/lib/env.sh && echo "$$HL_API_PORT"')
HL_METRICS_PORT := $(shell bash -c '. localdev/lib/env.sh && echo "$$HL_METRICS_PORT"')
export HL_API_PORT HL_METRICS_PORT
COMPOSE := podman compose -f $(COMPOSE_FILE) $(if $(REGISTRY_REACHABLE_IP),-f localdev/controlplane/docker-compose.reachable.yml)

# In production REGISTRY names a real registry the operator provisioned and every cluster
# node already has network access to (the cluster-agent chart's images.registry, make cluster-agent-up);
# this default is the local dev registry. TAG names the tree content that was built (localdev/lib/image-tag.sh),
# so a manifest that pins one tag can never silently start running different bytes under
# imagePullPolicy: IfNotPresent. Computed once here and exported: every script that deploys an
# image reads it from make. REGISTRY_TLS_VERIFY is false only for the dev registry, which
# docker-compose.yml serves over plain HTTP.
LOCAL_REGISTRY := $(HOST_ADDRESS):5000
REGISTRY ?= $(LOCAL_REGISTRY)
ifndef TAG
TAG := $(shell bash localdev/lib/image-tag.sh)
endif
export HOST_ADDRESS REGISTRY_PUBLISH_IP REGISTRY_REACHABLE_IP REGISTRY TAG
REGISTRY_TLS_VERIFY ?= $(if $(filter $(LOCAL_REGISTRY),$(REGISTRY)),false,true)
# Run before every push. A registry whose login expires sets the command that logs in to it; the
# dev registry takes none.
REGISTRY_LOGIN ?= true
# The platform every image is built for: the container engine's own unless set (linux/amd64 for
# nodes of another architecture). It is always stated to the build. Left unstated, a base image
# the engine already holds for another platform is used as it is, and the image built on it
# starts nowhere on this machine's nodes (exec format error).
IMAGE_PLATFORM ?= $(shell podman info --format '{{.Host.OS}}/{{.Host.Arch}}')
# What builds and pushes the images. A builder with docker alone sets CONTAINER_ENGINE=docker and
# IMAGE_PLATFORM.
CONTAINER_ENGINE ?= podman

check-tag:
	@test -n "$(IMAGE_PLATFORM)" || { echo "make: could not ask podman for its platform; set IMAGE_PLATFORM" >&2; exit 1; }
	@test -n "$(TAG)" || { echo "make: could not compute an image TAG (localdev/lib/image-tag.sh)" >&2; exit 1; }

# object_store.endpoint in controlplane/settings/hypothesisloop.yaml carries a
# REPLACE-WITH-HOST-ADDRESS placeholder rather than an address, because a k3s job pod has its own
# network namespace and can't reach the host's loopback, and the address differs between macOS
# and Linux hosts. This renders HOST_ADDRESS in at start-up, or LOCAL_REACHABLE_ADDRESS when .env
# sets it, the one address remote clusters are handed for everything; the control plane rejects a
# loopback endpoint outright, so a wrong address fails here, not hours into a run with nothing saved.
# HOST_ADDRESS_FILE hands the same address to the scripts and tests that run outside make.
# Everything that runs hl-admin depends on this: it reads the rendered settings, since the
# committed file is refused at load for its placeholders. It also writes each program's
# environment file from .env (localdev/lib/env.sh). The recipe prints nothing, so a target whose
# output is a token stays just the token.
render-settings:
	@test -n "$(HOST_ADDRESS)" || { echo "make: could not detect this host's address" >&2; exit 1; }
	@bash -c '. localdev/lib/auth.sh && lib_render_settings "$(HOST_ADDRESS)" $(RENDERED_SETTINGS) && lib_env_files $(RENDERED_SETTINGS) && lib_registry_reachable_ip >/dev/null'
	@echo "$(HOST_ADDRESS)" > $(HOST_ADDRESS_FILE)

# await WHAT,COMMAND runs COMMAND every second until it succeeds, and fails once AWAIT_SECONDS
# have passed without it.
AWAIT_SECONDS ?= 120
define await
@(waited=0; until $(2) >/dev/null 2>&1; do \
	waited=$$((waited + 1)); \
	[ $$waited -lt $(AWAIT_SECONDS) ] || { echo "make: $(1) did not answer within $(AWAIT_SECONDS)s" >&2; exit 1; }; \
	sleep 1; \
done)
endef

# A build's tag is not in use until it is pushed and installed, so a prune running beside it
# would take it away. Every target that builds therefore runs its recipe (locked-<target>) under
# the one lock the prune takes too (localdev/images-lock.py).
BUILDS := publish local-images
$(BUILDS):
	@python3 localdev/images-lock.py $(MAKE) locked-$@ TAG="$(TAG)"

include localdev/local.mk
include deploy/deploy.mk
include tests/tests.mk

build:
	go build ./...

lint:
	golangci-lint run ./...

check: build test agent-test helm-lint
	go vet ./...
	cd controlplane/ui && npm test && npm run build
