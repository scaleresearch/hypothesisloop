package people

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

func TestTheOrgIsTheFlagThenTheEnvironmentThenTheOnlyOneTheTokenReaches(t *testing.T) {
	calls := clitest.AccountsServer(t, []string{"org-only"}, `{"projects":[{"id":"prj-1","name":"default"}]}`)
	code, stdout, stderr := clitest.Output(t, ProjectList)
	if code != 0 || (*calls)[len(*calls)-1].Path != "/orgs/org-only/projects" {
		t.Fatalf("with one reachable org: exit code = %d, calls = %+v, stderr = %s", code, *calls, stderr)
	}
	if stdout != `{"id":"prj-1","name":"default"}`+"\n" {
		t.Fatalf("stdout: got = %q, want the project on one line", stdout)
	}

	t.Setenv("HL_ORG", "org-env")
	if code, _, _ := clitest.Output(t, ProjectList); code != 0 || (*calls)[len(*calls)-1].Path != "/orgs/org-env/projects" {
		t.Fatalf("with $HL_ORG: exit code = %d, last call = %+v", code, (*calls)[len(*calls)-1])
	}
	if code, _, _ := clitest.Output(t, ProjectList, "--org", "org-flag"); code != 0 || (*calls)[len(*calls)-1].Path != "/orgs/org-flag/projects" {
		t.Fatalf("with --org: exit code = %d, last call = %+v", code, (*calls)[len(*calls)-1])
	}
}

func TestOrgListPrintsTheOrgsOfMe(t *testing.T) {
	clitest.AccountsServer(t, []string{"org-1", "org-2"}, `{}`)
	code, stdout, stderr := clitest.Output(t, OrgList)
	want := `{"id":"org-1","name":"name of org-1"}` + "\n" + `{"id":"org-2","name":"name of org-2"}` + "\n"
	if code != 0 || stdout != want {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q, want = %q", code, stdout, stderr, want)
	}
}

func TestProjectRenameTakesTheProjectFromTheEnvironment(t *testing.T) {
	calls := clitest.AccountsServer(t, nil, `{"id":"prj-env","name":"vision"}`)
	if code, _, stderr := clitest.Output(t, ProjectRename, "vision"); code != 2 || !strings.Contains(stderr, "HL_PROJECT") || len(*calls) != 0 {
		t.Fatalf("with no project: exit code = %d, stderr = %q, calls = %+v, want a usage error naming $HL_PROJECT", code, stderr, *calls)
	}
	t.Setenv("HL_PROJECT", "prj-env")
	if code, _, stderr := clitest.Output(t, ProjectRename, "vision"); code != 0 || (*calls)[0].Path != "/projects/prj-env" {
		t.Fatalf("with $HL_PROJECT: exit code = %d, calls = %+v, stderr = %s", code, *calls, stderr)
	}
}

func TestAPIKeyCreateNeverNarrowsTheKeyFromTheEnvironment(t *testing.T) {
	calls := clitest.AccountsServer(t, nil, `{"id":"abc","name":"ci","token":"hlk_abc_secret"}`)
	t.Setenv("HL_PROJECT", "prj-env")
	code, stdout, stderr := clitest.Output(t, ApiKeyCreate, "ci", "--org", "org-1")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if _, narrowed := (*calls)[0].Body["project_id"]; narrowed {
		t.Fatalf("the key was bound to $HL_PROJECT: %+v", (*calls)[0].Body)
	}
	if !strings.Contains(stdout, `"token":"hlk_abc_secret"`) {
		t.Fatalf("stdout does not show the token it will never see again: %q", stdout)
	}
}

func TestClusterTokenPrintsTheTokenOfAClusterTheOrgRegisteredAndRegistersNothing(t *testing.T) {
	var posted []string
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`{"clusters":[{"name":"gpu-east","owned":true},{"name":"platform","owned":false,"shared":true}]}`))
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		posted = append(posted, body["name"])
		_, _ = w.Write([]byte(`{"name":"` + body["name"] + `","runtime_token":"hls_token_of_` + body["name"] + `"}`))
	})
	clitest.SetAPIURL(t, url)

	code, stdout, stderr := clitest.Output(t, ClusterToken, "gpu-east", "--org", "org-1")
	if code != 0 || stdout != "hls_token_of_gpu-east\n" {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q, want the token alone on stdout", code, stdout, stderr)
	}
	for _, name := range []string{"platform", "gpu-esat"} {
		code, stdout, stderr := clitest.Output(t, ClusterToken, name, "--org", "org-1")
		if code == 0 || stdout != "" || !strings.Contains(stderr, "not a cluster org org-1 registered") {
			t.Fatalf("token of %s: exit code = %d, stdout = %q, stderr = %q, want a refusal", name, code, stdout, stderr)
		}
	}
	if len(posted) != 1 || posted[0] != "gpu-east" {
		t.Fatalf("registered %v, want only the cluster the org already had: a mistyped name must not become a cluster", posted)
	}
}

func TestARefusalWithAReasonIsPrinted(t *testing.T) {
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"reason":"last_owner","message":"an org keeps at least one owner"}`))
	})
	clitest.SetAPIURL(t, url)
	code, stdout, stderr := clitest.Output(t, OrgMembersRemove, "u-1", "--org", "org-1")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "last_owner") || strings.Contains(stderr, "removed") {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q, want the refusal and no claim of a removal", code, stdout, stderr)
	}
}
