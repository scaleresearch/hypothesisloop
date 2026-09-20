package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type recordedCall struct {
	method, path string
	body         map[string]any
}

// accountsServer answers every account path with reply and records what it was asked. GET /me
// answers with the given orgs.
func accountsServer(t *testing.T, orgs []string, reply string) *[]recordedCall {
	t.Helper()
	calls := &[]recordedCall{}
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		call := recordedCall{method: r.Method, path: r.URL.RequestURI()}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &call.body)
		}
		*calls = append(*calls, call)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/me" {
			me := map[string]any{"kind": "key", "orgs": []map[string]string{}, "projects": []any{}}
			for _, id := range orgs {
				me["orgs"] = append(me["orgs"].([]map[string]string), map[string]string{"id": id, "name": "name of " + id})
			}
			_ = json.NewEncoder(w).Encode(me)
			return
		}
		_, _ = w.Write([]byte(reply))
	})
	setAPIURL(t, url)
	t.Setenv("HL_ORG", "")
	t.Setenv("HL_PROJECT", "")
	return calls
}

func hl(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestAccountCommandsCallTheirEndpoints(t *testing.T) {
	for _, tc := range []struct {
		args         []string
		method, path string
		body         map[string]any
	}{
		{[]string{"org", "create", "research"}, "POST", "/orgs", map[string]any{"name": "research"}},
		{[]string{"org", "create", "research", "--owner", "u-1"}, "POST", "/orgs", map[string]any{"name": "research", "owner_user_id": "u-1"}},
		{[]string{"org", "rename", "new name", "--org", "org-1"}, "PATCH", "/orgs/org-1", map[string]any{"name": "new name"}},
		{[]string{"project", "list", "--org", "org-1"}, "GET", "/orgs/org-1/projects", nil},
		{[]string{"project", "create", "vision", "--org", "org-1"}, "POST", "/orgs/org-1/projects", map[string]any{"name": "vision"}},
		{[]string{"project", "rename", "--project", "prj-1", "vision"}, "PATCH", "/projects/prj-1", map[string]any{"name": "vision"}},
		{[]string{"api-key", "create", "ci", "--org", "org-1"}, "POST", "/orgs/org-1/api-keys", map[string]any{"name": "ci"}},
		{[]string{"api-key", "create", "ci", "--org", "org-1", "--project", "prj-1"}, "POST", "/orgs/org-1/api-keys", map[string]any{"name": "ci", "project_id": "prj-1"}},
		{[]string{"api-key", "list", "--org", "org-1"}, "GET", "/orgs/org-1/api-keys", nil},
		{[]string{"api-key", "revoke", "abc123"}, "DELETE", "/api-keys/abc123", nil},
		{[]string{"api-key", "revoke", "--id", "abc123"}, "DELETE", "/api-keys/abc123", nil},
		{[]string{"cluster", "list", "--org", "org-1"}, "GET", "/orgs/org-1/clusters", nil},
		{[]string{"cluster", "add", "gpu-east", "--org", "org-1"}, "POST", "/orgs/org-1/clusters", map[string]any{"name": "gpu-east"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			calls := accountsServer(t, nil, `{"id":"x","projects":[],"api_keys":[],"clusters":[],"name":"gpu-east","runtime_token":"hls_t"}`)
			code, _, stderr := hl(t, tc.args...)
			if code != 0 {
				t.Fatalf("exit code = %d, stderr = %s", code, stderr)
			}
			if len(*calls) != 1 {
				t.Fatalf("made %d calls %+v, want one: --org was given, so /me is not asked", len(*calls), *calls)
			}
			got := (*calls)[0]
			if got.method != tc.method || got.path != tc.path {
				t.Fatalf("got = %s %s, want = %s %s", got.method, got.path, tc.method, tc.path)
			}
			gotBody, _ := json.Marshal(got.body)
			wantBody, _ := json.Marshal(tc.body)
			if string(gotBody) != string(wantBody) {
				t.Fatalf("body: got = %s, want = %s", gotBody, wantBody)
			}
		})
	}
}

func TestTheOrgIsTheFlagThenTheEnvironmentThenTheOnlyOneTheTokenReaches(t *testing.T) {
	calls := accountsServer(t, []string{"org-only"}, `{"projects":[{"id":"prj-1","name":"default"}]}`)
	code, stdout, stderr := hl(t, "project", "list")
	if code != 0 || (*calls)[len(*calls)-1].path != "/orgs/org-only/projects" {
		t.Fatalf("with one reachable org: exit code = %d, calls = %+v, stderr = %s", code, *calls, stderr)
	}
	if stdout != `{"id":"prj-1","name":"default"}`+"\n" {
		t.Fatalf("stdout: got = %q, want the project on one line", stdout)
	}

	t.Setenv("HL_ORG", "org-env")
	if code, _, _ := hl(t, "project", "list"); code != 0 || (*calls)[len(*calls)-1].path != "/orgs/org-env/projects" {
		t.Fatalf("with $HL_ORG: exit code = %d, last call = %+v", code, (*calls)[len(*calls)-1])
	}
	if code, _, _ := hl(t, "project", "list", "--org", "org-flag"); code != 0 || (*calls)[len(*calls)-1].path != "/orgs/org-flag/projects" {
		t.Fatalf("with --org: exit code = %d, last call = %+v", code, (*calls)[len(*calls)-1])
	}
}

func TestACommandNeedingAnOrgNamesNoneWhenTheTokenReachesSeveral(t *testing.T) {
	for _, orgs := range [][]string{{"org-1", "org-2"}, nil} {
		calls := accountsServer(t, orgs, `{}`)
		for _, args := range [][]string{
			{"project", "list"}, {"project", "create", "x"}, {"org", "rename", "x"}, {"api-key", "list"},
			{"api-key", "create", "x"}, {"cluster", "list"}, {"cluster", "add", "x"},
		} {
			before := len(*calls)
			code, stdout, stderr := hl(t, args...)
			if code == 0 || !strings.Contains(stderr, "--org") || stdout != "" {
				t.Fatalf("hl %v with %d orgs: exit code = %d, stdout = %q, stderr = %q, want a failure naming --org", args, len(orgs), code, stdout, stderr)
			}
			if made := (*calls)[before:]; len(made) != 1 || made[0].path != "/me" {
				t.Fatalf("hl %v with %d orgs made %+v, want only GET /me", args, len(orgs), made)
			}
		}
	}
}

func TestOrgListPrintsTheOrgsOfMe(t *testing.T) {
	accountsServer(t, []string{"org-1", "org-2"}, `{}`)
	code, stdout, stderr := hl(t, "org", "list")
	want := `{"id":"org-1","name":"name of org-1"}` + "\n" + `{"id":"org-2","name":"name of org-2"}` + "\n"
	if code != 0 || stdout != want {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q, want = %q", code, stdout, stderr, want)
	}
}

func TestProjectRenameTakesTheProjectFromTheEnvironment(t *testing.T) {
	calls := accountsServer(t, nil, `{"id":"prj-env","name":"vision"}`)
	if code, _, stderr := hl(t, "project", "rename", "vision"); code != 2 || !strings.Contains(stderr, "HL_PROJECT") || len(*calls) != 0 {
		t.Fatalf("with no project: exit code = %d, stderr = %q, calls = %+v, want a usage error naming $HL_PROJECT", code, stderr, *calls)
	}
	t.Setenv("HL_PROJECT", "prj-env")
	if code, _, stderr := hl(t, "project", "rename", "vision"); code != 0 || (*calls)[0].path != "/projects/prj-env" {
		t.Fatalf("with $HL_PROJECT: exit code = %d, calls = %+v, stderr = %s", code, *calls, stderr)
	}
}

func TestAPIKeyCreateNeverNarrowsTheKeyFromTheEnvironment(t *testing.T) {
	calls := accountsServer(t, nil, `{"id":"abc","name":"ci","token":"hlk_abc_secret"}`)
	t.Setenv("HL_PROJECT", "prj-env")
	code, stdout, stderr := hl(t, "api-key", "create", "ci", "--org", "org-1")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if _, narrowed := (*calls)[0].body["project_id"]; narrowed {
		t.Fatalf("the key was bound to $HL_PROJECT: %+v", (*calls)[0].body)
	}
	if !strings.Contains(stdout, `"token":"hlk_abc_secret"`) {
		t.Fatalf("stdout does not show the token it will never see again: %q", stdout)
	}
}

func TestClusterAddPrintsTheCommandThatStartsItsAgent(t *testing.T) {
	accountsServer(t, nil, `{"name":"gpu-east","org_id":"org-1","shared":false,"runtime_token":"hls_cnVudGltZQ.c2ln","api_url":"https://api.example","metrics_url":"https://metrics.example"}`)
	code, stdout, stderr := hl(t, "cluster", "add", "gpu-east", "--org", "org-1")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	want := "RUNTIME_TOKEN=hls_cnVudGltZQ.c2ln make cluster-agent-up CLUSTER=gpu-east API_URL=https://api.example METRICS_URL=https://metrics.example\n"
	if stdout != want {
		t.Fatalf("stdout: got = %q, want = %q, with the addresses the platform answered and not the one hl itself called", stdout, want)
	}
}

func TestARefusalFromTheAccountsAPIIsPrintedAndFails(t *testing.T) {
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"cluster name is registered to another org"}`))
	})
	setAPIURL(t, url)
	code, stdout, stderr := hl(t, "cluster", "add", "taken", "--org", "org-1")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "registered to another org") {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q, want the refusal and no command printed", code, stdout, stderr)
	}
}

func TestAnEmptyListSaysSoOnStderr(t *testing.T) {
	accountsServer(t, nil, `{"clusters":[],"api_keys":[],"projects":[]}`)
	for _, group := range []string{"cluster", "api-key", "project"} {
		code, stdout, stderr := hl(t, group, "list", "--org", "org-1")
		if code != 0 || stdout != "" || !strings.Contains(stderr, group+" list: none") {
			t.Fatalf("hl %s list: exit code = %d, stdout = %q, stderr = %q", group, code, stdout, stderr)
		}
	}
}

func TestAccountGroupsListTheirCommands(t *testing.T) {
	setAPIURL(t, "http://unused")
	for group, commands := range map[string][]string{
		"org": {"list", "create NAME", "rename NAME"}, "project": {"list", "create NAME", "rename NAME"},
		"api-key": {"create NAME", "list", "revoke ID"}, "cluster": {"add NAME", "list"},
	} {
		code, stdout, _ := hl(t, group, "--help")
		if code != 0 {
			t.Fatalf("hl %s --help: exit code = %d", group, code)
		}
		for _, command := range commands {
			if !strings.Contains(stdout, "\n  "+command) {
				t.Errorf("hl %s --help does not list %q:\n%s", group, command, stdout)
			}
		}
		if code, _, stderr := hl(t, group, "nonsense"); code != 2 || !strings.Contains(stderr, "unknown subcommand") {
			t.Errorf("hl %s nonsense: exit code = %d, stderr = %q", group, code, stderr)
		}
	}
}

func TestPlatformExperimentsListFiltersByProject(t *testing.T) {
	calls := accountsServer(t, nil, `[]`)
	if code, _, stderr := hl(t, "platform-experiments", "list"); code != 0 || (*calls)[0].path != "/platform-experiments?limit=20" {
		t.Fatalf("with no project: exit code = %d, calls = %+v, stderr = %s", code, *calls, stderr)
	}
	t.Setenv("HL_PROJECT", "prj-env")
	if code, _, _ := hl(t, "platform-experiments"); code != 0 || (*calls)[1].path != "/platform-experiments?limit=20&project_id=prj-env" {
		t.Fatalf("with $HL_PROJECT: exit code = %d, call = %+v", code, (*calls)[1])
	}
	if code, _, _ := hl(t, "platform-experiments", "list", "--project", "prj-flag"); code != 0 || (*calls)[2].path != "/platform-experiments?limit=20&project_id=prj-flag" {
		t.Fatalf("with --project: exit code = %d, call = %+v", code, (*calls)[2])
	}
}
