package people

import (
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// A command that attaches a cluster on another machine to addresses only the platform's machine
// reaches cannot work: each such address is named on stderr, and the command is still printed.
func TestClusterAddWarnsOfEachAddressOnlyThePlatformsMachineReaches(t *testing.T) {
	clitest.AccountsServer(t, nil, `{"name":"tt-box","runtime_token":"hls_t","api_url":"http://host.containers.internal:8081","metrics_url":"http://host.containers.internal:8084",`+
		`"registry":"host.containers.internal:5000","attach_commands":{"kubernetes":"helm it","bare_metal":"install it"}}`)
	code, stdout, stderr := clitest.Output(t, ClusterAdd, "tt-box", "--org", "org-1")
	if code != 0 || !strings.HasPrefix(stdout, "helm it\n") {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
	for _, want := range []string{"the API address http://host.containers.internal:8081", "the metrics address http://host.containers.internal:8084",
		"the image registry host.containers.internal:5000", "other machines cannot reach it", "LOCAL_REACHABLE_ADDRESS"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, stderr)
		}
	}

	clitest.AccountsServer(t, nil, `{"name":"tt-box","runtime_token":"hls_t","api_url":"http://100.64.0.5:8081","metrics_url":"http://100.64.0.5:8084",`+
		`"registry":"100.64.0.5:5000","attach_commands":{"kubernetes":"helm it","bare_metal":"install it"}}`)
	if code, _, stderr := clitest.Output(t, ClusterAdd, "tt-box", "--org", "org-1"); code != 0 || stderr != "" {
		t.Errorf("addresses other machines reach: exit %d, stderr %q, want no warning", code, stderr)
	}
}

func TestClusterListWarnsOfAVersionSkewAndOfARefusedAgent(t *testing.T) {
	clitest.AccountsServer(t, nil, `{"clusters":[{"name":"tt-box","connected":false,"agent_version":"old1234","version_skew":true,"last_error":"cluster tt-box is bound to another cluster_id"},`+
		`{"name":"steady","connected":true,"agent_version":"abc1234"}]}`)
	code, stdout, stderr := clitest.Output(t, ClusterList, "--org", "org-1")
	if code != 0 || strings.Count(stdout, "\n") != 2 || !strings.Contains(stdout, `"version_skew":true`) {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
	for _, want := range []string{"cluster tt-box runs agent version old1234, not the platform's", "cluster tt-box is not connected, and its agent was last refused with: cluster tt-box is bound"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "steady") {
		t.Errorf("a cluster in step is warned about:\n%s", stderr)
	}
}
