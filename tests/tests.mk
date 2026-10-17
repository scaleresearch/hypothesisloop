.PHONY: e2e tt-e2e test agent-test test-github-live

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

# The workloads the tests' jobs run, built and pushed as the product's images are (push_image in
# deploy/deploy.mk) and never part of a publish. make local-images builds workload for make e2e.
TEST_IMAGES := workload tenstorrent-workload
IMAGE_DOCKERFILE_workload := tests/workloads/generic/Dockerfile.train
IMAGE_CONTEXT_workload := tests/workloads/generic
IMAGE_DOCKERFILE_tenstorrent-workload := tests/workloads/tenstorrent/Dockerfile.train
IMAGE_CONTEXT_tenstorrent-workload := tests/workloads/tenstorrent
.PHONY: $(addprefix image-,$(TEST_IMAGES))
$(addprefix image-,$(TEST_IMAGES)): image-%: check-tag
	$(call push_image,$*)

# Run on the box with the REGISTRY and TAG it was attached with, so its nodes pull the image just pushed.
tt-e2e: render-settings image-tenstorrent-workload
	bash -c '$(E2E_ENV) TT_WORKLOAD_IMAGE="$(REGISTRY)/hypothesisloop-tenstorrent-workload:$(TAG)" bash localdev/k3s-tenstorrent/run-e2e.sh'

# The graph tests get a throwaway Memgraph: community has one database, so they would otherwise
# write next to the stack's own graph.
TEST_MEMGRAPH := hypothesisloop-test-memgraph
test:
	podman run -d --rm --name $(TEST_MEMGRAPH) -p 127.0.0.1:7688:7687 docker.io/memgraph/memgraph:3.13.1 --telemetry-enabled=false >/dev/null
	$(call await,the test Memgraph,podman exec $(TEST_MEMGRAPH) sh -c "echo 'RETURN 1;' | mgconsole") || { podman rm -f $(TEST_MEMGRAPH) >/dev/null; exit 1; }
	go test ./... -timeout 60s; rc=$$?; podman rm -f $(TEST_MEMGRAPH) >/dev/null; exit $$rc

# pytest, not unittest discovery: the conftest.py fixtures every test relies on only load under pytest.
# The tests that shell out to hl find the one built from this tree.
AGENT_TEST_BIN := $(CURDIR)/bin/agent-test
agent-test:
	go build -o $(AGENT_TEST_BIN)/hl ./cli
	cd agents/harness && PATH="$(AGENT_TEST_BIN):$$PATH" uv run pytest tests

# The GitHub client's checks against github.com itself, as the App .env names
# (controlplane/clients/githost/github/live_test.go). The token checks and the push check use the
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
	  printf "%s\n" "$$(lib_env_value GIT_HOST_GITHUB_APP_PRIVATE_KEY)" > "$$key_file"; \
	  TEST_GIT_HOST_GITHUB_APP_ID="$$HL_GIT_HOST_GITHUB_APP_ID" TEST_GIT_HOST_GITHUB_KEY_FILE="$$key_file" \
	  TEST_GIT_HOST_GITHUB_INSTALLATION="$$(lib_env_value TEST_GIT_HOST_GITHUB_INSTALLATION)" \
	  TEST_GIT_HOST_GITHUB_REPOSITORY="$$(lib_env_value TEST_GIT_HOST_GITHUB_REPOSITORY)" \
	  TEST_GIT_HOST_GITHUB_CREATE_INSTALLATION="$$(lib_env_value TEST_GIT_HOST_GITHUB_CREATE_INSTALLATION)" \
	  go test ./controlplane/clients/githost/github -run Live -count=1 -v -timeout 180s'
