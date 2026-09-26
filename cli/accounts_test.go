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
		{[]string{"org", "members", "list", "--org", "org-1"}, "GET", "/orgs/org-1/members", nil},
		{[]string{"org", "members", "list", "--org", "org-1", "--limit", "50", "--offset", "20"}, "GET", "/orgs/org-1/members?limit=50&offset=20", nil},
		{[]string{"org", "members", "set-role", "u-1", "owner", "--org", "org-1"}, "PATCH", "/orgs/org-1/members/u-1", map[string]any{"role": "owner"}},
		{[]string{"org", "members", "remove", "u-1", "--org", "org-1"}, "DELETE", "/orgs/org-1/members/u-1", nil},
		{[]string{"org", "invites", "create", "--org", "org-1"}, "POST", "/orgs/org-1/invites", map[string]any{"role": "member"}},
		{[]string{"org", "invites", "create", "--role", "owner", "--org", "org-1"}, "POST", "/orgs/org-1/invites", map[string]any{"role": "owner"}},
		{[]string{"org", "invites", "list", "--org", "org-1", "--limit", "5"}, "GET", "/orgs/org-1/invites?limit=5", nil},
		{[]string{"org", "invites", "revoke", "inv-1"}, "DELETE", "/invites/inv-1", nil},
		{[]string{"org", "invites", "accept", "hli_secret"}, "POST", "/invites/accept", map[string]any{"token": "hli_secret"}},
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
		{[]string{"cluster", "remove", "gpu-east", "--org", "org-1"}, "DELETE", "/orgs/org-1/clusters/gpu-east", nil},
		{[]string{"cluster", "forget-identity", "gpu-east", "--org", "org-1"}, "DELETE", "/orgs/org-1/clusters/gpu-east/identity", nil},
		{[]string{"cluster", "disable", "gpu-east", "--org", "org-1"}, "POST", "/orgs/org-1/clusters/gpu-east/disable", nil},
		{[]string{"cluster", "enable", "gpu-east", "--org", "org-1"}, "POST", "/orgs/org-1/clusters/gpu-east/enable", nil},
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
			{"cluster", "token", "x"}, {"cluster", "remove", "x"}, {"cluster", "forget-identity", "x"},
			{"cluster", "disable", "x"}, {"cluster", "enable", "x"},
			{"org", "members", "list"}, {"org", "members", "set-role", "u-1", "member"}, {"org", "members", "remove", "u-1"},
			{"org", "invites", "create"}, {"org", "invites", "list"},
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

// The printed command lands in scrollback and, once run, in shell history, so it carries no
// token: it reads the token when it runs. What the platform answered is quoted as one word each.
func TestClusterAddPrintsACommandThatHoldsNoToken(t *testing.T) {
	accountsServer(t, nil, `{"name":"gpu-east","org_id":"org-1","shared":false,"runtime_token":"hls_cnVudGltZQ.c2ln","api_url":"https://api.example/a?b=1&c=$(id)","metrics_url":"https://metrics.example/it's","registry":"registry.example/images","image_tag":"abc1234"}`)
	code, stdout, stderr := hl(t, "cluster", "add", "gpu-east", "--org", "org-1")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if strings.Contains(stdout+stderr, "hls_cnVudGltZQ") {
		t.Fatalf("the runtime token is printed: stdout = %q, stderr = %q", stdout, stderr)
	}
	want := `RUNTIME_TOKEN="$(hl cluster token 'gpu-east' --org 'org-1')" make cluster-agent-up CLUSTER='gpu-east' ` +
		`API_URL='https://api.example/a?b=1&c=$(id)' METRICS_URL='https://metrics.example/it'\''s' ` +
		`REGISTRY='registry.example/images' TAG='abc1234'` + "\n"
	if stdout != want {
		t.Fatalf("stdout: got = %q, want = %q, with the addresses the platform answered, each quoted", stdout, want)
	}
}

// A bare-metal node gets one command that needs docker and nothing else on the node, and holds
// no token either.
func TestClusterAddPrintsTheContainerThatAttachesABareMetalNode(t *testing.T) {
	accountsServer(t, nil, `{"name":"gpu-box","org_id":"org-1","shared":false,"runtime_token":"hls_cnVudGltZQ.c2ln","api_url":"https://api.example","metrics_url":"https://metrics.example","registry":"registry.example/images","image_tag":"abc1234"}`)
	code, stdout, stderr := hl(t, "cluster", "add", "gpu-box", "--bare-metal", "--org", "org-1")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if strings.Contains(stdout+stderr, "hls_cnVudGltZQ") {
		t.Fatalf("the runtime token is printed: stdout = %q, stderr = %q", stdout, stderr)
	}
	want := `RUNTIME_TOKEN="$(hl cluster token 'gpu-box' --org 'org-1')" docker run -d --name hypothesisloop-agent --restart unless-stopped --hostname "$(hostname)" ` +
		`-v /var/run/docker.sock:/var/run/docker.sock -v /etc/machine-id:/etc/machine-id:ro -v /var/lib/hypothesisloop:/var/lib/hypothesisloop ` +
		`-e CLUSTER_NAME='gpu-box' -e API_URL='https://api.example' -e RUNTIME_TOKEN 'registry.example/images/hypothesisloop-bare-agent:abc1234'` + "\n"
	if stdout != want {
		t.Fatalf("stdout: got = %q, want = %q", stdout, want)
	}
}

func TestClusterTokenPrintsTheTokenOfAClusterTheOrgRegisteredAndRegistersNothing(t *testing.T) {
	var posted []string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
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
	setAPIURL(t, url)

	code, stdout, stderr := hl(t, "cluster", "token", "gpu-east", "--org", "org-1")
	if code != 0 || stdout != "hls_token_of_gpu-east\n" {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q, want the token alone on stdout", code, stdout, stderr)
	}
	for _, name := range []string{"platform", "gpu-esat"} {
		code, stdout, stderr := hl(t, "cluster", "token", name, "--org", "org-1")
		if code == 0 || stdout != "" || !strings.Contains(stderr, "not a cluster org org-1 registered") {
			t.Fatalf("token of %s: exit code = %d, stdout = %q, stderr = %q, want a refusal", name, code, stdout, stderr)
		}
	}
	if len(posted) != 1 || posted[0] != "gpu-east" {
		t.Fatalf("registered %v, want only the cluster the org already had: a mistyped name must not become a cluster", posted)
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

func TestClusterListShowsWhichClustersTheOrgTurnedOff(t *testing.T) {
	accountsServer(t, nil, `{"clusters":[{"name":"gpu-east","owned":true,"enabled":true},{"name":"platform","shared":true,"enabled":false}]}`)
	code, stdout, stderr := hl(t, "cluster", "list", "--org", "org-1")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"name":"gpu-east"`) || !strings.Contains(lines[0], `"enabled":true`) ||
		!strings.Contains(lines[1], `"name":"platform"`) || !strings.Contains(lines[1], `"enabled":false`) {
		t.Fatalf("listed = %s, want gpu-east enabled and platform not, one a line", stdout)
	}
}

func TestClusterDisablePrintsTheClusterAndARefusalFails(t *testing.T) {
	accountsServer(t, nil, `{"name":"platform","shared":true,"enabled":false}`)
	code, stdout, stderr := hl(t, "cluster", "disable", "platform", "--org", "org-1")
	if code != 0 || !strings.Contains(stdout, `"enabled":false`) {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q, want the cluster printed as turned off", code, stdout, stderr)
	}

	setAPIURL(t, withServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"reason":"not_found","message":"cluster not found"}`))
	}))
	code, stdout, stderr = hl(t, "cluster", "enable", "of-another-org", "--org", "org-1")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "cluster not found") {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q, want the refusal", code, stdout, stderr)
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
		"org": {"list", "create NAME", "rename NAME", "members <command>", "invites <command>"}, "project": {"list", "create NAME", "rename NAME"},
		"api-key": {"create NAME", "list", "revoke ID"}, "cluster": {"add NAME", "list", "token NAME", "disable NAME", "enable NAME", "forget-identity NAME", "remove NAME"},
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

func TestPlatformExperimentsListPassesSharedOnAsTheFilterItIs(t *testing.T) {
	calls := accountsServer(t, nil, `[{"id":"pe-own","shared":false},{"id":"pe-show","shared":true}]`)
	t.Setenv("HL_PROJECT", "prj-env")
	code, stdout, stderr := hl(t, "platform-experiments", "list")
	if code != 0 || (*calls)[0].path != "/platform-experiments?limit=20&project_id=prj-env" {
		t.Fatalf("no flag: exit code = %d, calls = %+v, stderr = %s, want no shared filter", code, *calls, stderr)
	}
	if !strings.Contains(stdout, `"shared": true`) || !strings.Contains(stdout, `"shared": false`) {
		t.Fatalf("no flag: stdout = %s, want each platform experiment with its shared mark", stdout)
	}
	if code, _, _ := hl(t, "platform-experiments", "list", "--shared"); code != 0 || (*calls)[1].path != "/platform-experiments?limit=20&project_id=prj-env&shared=true" {
		t.Fatalf("--shared: exit code = %d, call = %+v", code, (*calls)[1])
	}
	if code, _, _ := hl(t, "platform-experiments", "--shared=false", "--project", "prj-flag", "--limit", "5"); code != 0 || (*calls)[2].path != "/platform-experiments?limit=5&project_id=prj-flag&shared=false" {
		t.Fatalf("--shared=false with --project: exit code = %d, call = %+v", code, (*calls)[2])
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

func TestOrgMembersAndInvitesListTheirCommands(t *testing.T) {
	setAPIURL(t, "http://unused")
	for group, commands := range map[string][]string{
		"members": {"list", "set-role USER owner|member", "remove USER"},
		"invites": {"create", "list", "revoke ID", "accept TOKEN"},
	} {
		code, stdout, _ := hl(t, "org", group, "--help")
		if code != 0 {
			t.Fatalf("hl org %s --help: exit code = %d", group, code)
		}
		for _, command := range commands {
			if !strings.Contains(stdout, "\n  "+command) {
				t.Errorf("hl org %s --help does not list %q:\n%s", group, command, stdout)
			}
		}
		if code, _, stderr := hl(t, "org", group, "nonsense"); code != 2 || !strings.Contains(stderr, "unknown subcommand") {
			t.Errorf("hl org %s nonsense: exit code = %d, stderr = %q", group, code, stderr)
		}
	}
}

func TestOrgMembersAndInvitesPrintOneLineEach(t *testing.T) {
	accountsServer(t, nil, `[{"org_id":"org-1","user_id":"u-1","role":"owner"},{"org_id":"org-1","user_id":"u-2","role":"member"}]`)
	code, stdout, stderr := hl(t, "org", "members", "list", "--org", "org-1")
	want := `{"org_id":"org-1","user_id":"u-1","role":"owner"}` + "\n" + `{"org_id":"org-1","user_id":"u-2","role":"member"}` + "\n"
	if code != 0 || stdout != want {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q, want = %q", code, stdout, stderr, want)
	}
	accountsServer(t, nil, `[]`)
	for _, group := range []string{"members", "invites"} {
		code, stdout, stderr := hl(t, "org", group, "list", "--org", "org-1")
		if code != 0 || stdout != "" || !strings.Contains(stderr, "org "+group+" list: none") {
			t.Fatalf("hl org %s list: exit code = %d, stdout = %q, stderr = %q", group, code, stdout, stderr)
		}
	}
}

func TestInviteCreatePrintsTheTokenItWillNeverSeeAgain(t *testing.T) {
	accountsServer(t, nil, `{"id":"inv-1","org_id":"org-1","role":"member","token":"hli_secret"}`)
	code, stdout, stderr := hl(t, "org", "invites", "create", "--org", "org-1")
	if code != 0 || !strings.Contains(stdout, `"token":"hli_secret"`) {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q, want the invite with its token", code, stdout, stderr)
	}
}

// Removing and revoking answer with no body, so the command says what it did.
func TestACommandAnsweredWithNoBodySaysWhatItDid(t *testing.T) {
	var asked []string
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})
	setAPIURL(t, url)
	for args, said := range map[string]string{
		"org members remove u-1 --org org-1": "removed u-1 from org org-1",
		"org invites revoke inv-1":           "revoked invite inv-1",
	} {
		code, stdout, stderr := hl(t, strings.Fields(args)...)
		if code != 0 || stdout != "" || !strings.Contains(stderr, said) {
			t.Errorf("hl %s: exit code = %d, stdout = %q, stderr = %q, want %q on stderr", args, code, stdout, stderr, said)
		}
	}
	if len(asked) != 2 {
		t.Fatalf("asked %v, want one call each", asked)
	}
}

func TestARoleNobodyDefinedIsRefusedBeforeAnyCall(t *testing.T) {
	calls := accountsServer(t, nil, `{}`)
	for _, args := range [][]string{
		{"org", "members", "set-role", "u-1", "admin", "--org", "org-1"},
		{"org", "members", "set-role", "u-1", "--org", "org-1"},
		{"org", "invites", "create", "--role", "admin", "--org", "org-1"},
		{"org", "invites", "accept"},
		{"org", "invites", "revoke"},
	} {
		if code, stdout, stderr := hl(t, args...); code != 2 || stdout != "" || !strings.Contains(stderr, "usage: hl org") {
			t.Errorf("hl %v: exit code = %d, stdout = %q, stderr = %q, want a usage error", args, code, stdout, stderr)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("made %+v, want no call", *calls)
	}
}

func TestARefusalWithAReasonIsPrinted(t *testing.T) {
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"reason":"last_owner","message":"an org keeps at least one owner"}`))
	})
	setAPIURL(t, url)
	code, stdout, stderr := hl(t, "org", "members", "remove", "u-1", "--org", "org-1")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "last_owner") || strings.Contains(stderr, "removed") {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q, want the refusal and no claim of a removal", code, stdout, stderr)
	}
}

// A create past what one account may hold is refused by the platform, and the refusal is what
// the person reads.
func TestACreatePastAnAccountLimitPrintsTheReason(t *testing.T) {
	for args, refusal := range map[string]string{
		"org create one-more":                 `{"reason":"max_orgs_reached","message":"a person owns at most 3 orgs, and the owner of this one already does"}`,
		"project create one-more --org org-1": `{"reason":"max_projects_reached","message":"an org holds at most 10 projects, and this one already does"}`,
	} {
		url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(refusal))
		})
		setAPIURL(t, url)
		code, stdout, stderr := hl(t, strings.Fields(args)...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "409") || !strings.Contains(stderr, refusal) {
			t.Errorf("hl %s: exit code = %d, stdout = %q, stderr = %q, want the 409 with %s", args, code, stdout, stderr, refusal)
		}
	}
}
