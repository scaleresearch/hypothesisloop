package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

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
		{[]string{"cluster", "order", "gpu-east", "gpu-west", "--org", "org-1"}, "PUT", "/orgs/org-1/clusters/order", map[string]any{"names": []string{"gpu-east", "gpu-west"}}},
		{[]string{"cluster", "settings", "gpu-east", "--stuck-pending-timeout", "900", "--speculative-cap", "0", "--org", "org-1"}, "PATCH", "/orgs/org-1/clusters/gpu-east", map[string]any{"max_speculative_accelerators": 0, "stuck_pending_timeout_seconds": 900}},
		{[]string{"cluster", "settings", "tt-box", "--status-silence-ceiling", "21600", "--org", "org-1"}, "PATCH", "/orgs/org-1/clusters/tt-box", map[string]any{"status_silence_ceiling_seconds": 21600}},
		{[]string{"cluster", "disable", "gpu-east", "--org", "org-1"}, "POST", "/orgs/org-1/clusters/gpu-east/disable", nil},
		{[]string{"cluster", "enable", "gpu-east", "--org", "org-1"}, "POST", "/orgs/org-1/clusters/gpu-east/enable", nil},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			calls := clitest.AccountsServer(t, nil, `{"id":"x","projects":[],"api_keys":[],"clusters":[],"name":"gpu-east","runtime_token":"hls_t"}`)
			code, _, stderr := hl(t, tc.args...)
			if code != 0 {
				t.Fatalf("exit code = %d, stderr = %s", code, stderr)
			}
			if len(*calls) != 1 {
				t.Fatalf("made %d calls %+v, want one: --org was given, so /me is not asked", len(*calls), *calls)
			}
			got := (*calls)[0]
			if got.Method != tc.method || got.Path != tc.path {
				t.Fatalf("got = %s %s, want = %s %s", got.Method, got.Path, tc.method, tc.path)
			}
			gotBody, _ := json.Marshal(got.Body)
			wantBody, _ := json.Marshal(tc.body)
			if string(gotBody) != string(wantBody) {
				t.Fatalf("body: got = %s, want = %s", gotBody, wantBody)
			}
		})
	}
}

func TestACommandNeedingAnOrgNamesNoneWhenTheTokenReachesSeveral(t *testing.T) {
	for _, orgs := range [][]string{{"org-1", "org-2"}, nil} {
		calls := clitest.AccountsServer(t, orgs, `{}`)
		for _, args := range [][]string{
			{"project", "list"}, {"project", "create", "x"}, {"org", "rename", "x"}, {"api-key", "list"},
			{"api-key", "create", "x"}, {"cluster", "list"}, {"cluster", "add", "x"},
			{"cluster", "token", "x"}, {"cluster", "remove", "x"}, {"cluster", "forget-identity", "x"},
			{"cluster", "disable", "x"}, {"cluster", "enable", "x"}, {"cluster", "order", "x"},
			{"org", "members", "list"}, {"org", "members", "set-role", "u-1", "member"}, {"org", "members", "remove", "u-1"},
			{"org", "invites", "create"}, {"org", "invites", "list"},
		} {
			before := len(*calls)
			code, stdout, stderr := hl(t, args...)
			if code == 0 || !strings.Contains(stderr, "--org") || stdout != "" {
				t.Fatalf("hl %v with %d orgs: exit code = %d, stdout = %q, stderr = %q, want a failure naming --org", args, len(orgs), code, stdout, stderr)
			}
			if made := (*calls)[before:]; len(made) != 1 || made[0].Path != "/me" {
				t.Fatalf("hl %v with %d orgs made %+v, want only GET /me", args, len(orgs), made)
			}
		}
	}
}

// The command is the platform's: hl prints the one it answered for the kind asked for, untouched,
// and the token it asks for below it. The command holds no token, so none lands in shell history.
func TestClusterAddPrintsTheCommandThePlatformAnsweredAndTheTokenItAsksFor(t *testing.T) {
	clitest.AccountsServer(t, nil, `{"name":"gpu-east","org_id":"org-1","runtime_token":"hls_cnVudGltZQ.c2ln","attach_commands":{"kubernetes":"sh -c 'helm it'\\''s' sh","bare_metal":"sudo sh -c 'install' sh"}}`)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"cluster", "add", "gpu-east", "--org", "org-1"}, `sh -c 'helm it'\''s' sh`},
		{[]string{"cluster", "add", "gpu-east", "--bare-metal", "--org", "org-1"}, `sudo sh -c 'install' sh`},
	} {
		code, stdout, stderr := hl(t, tc.args...)
		if want := tc.want + "\n\nIt asks for the runtime token of gpu-east:\nhls_cnVudGltZQ.c2ln\n"; code != 0 || stdout != want {
			t.Fatalf("hl %s: exit code = %d, stderr = %s, stdout = %q, want = %q", strings.Join(tc.args, " "), code, stderr, stdout, want)
		}
	}
}

func TestClusterOrderAndSettingsAskForWhatTheyChangeAndPrintARefusal(t *testing.T) {
	calls := clitest.AccountsServer(t, nil, `{"clusters":[{"name":"gpu-east"}]}`)
	for _, args := range [][]string{
		{"cluster", "order", "--org", "org-1"},
		{"cluster", "settings", "gpu-east", "--org", "org-1"},
		{"cluster", "settings", "gpu-east", "--scale-up-timeout", "soon", "--org", "org-1"},
		{"cluster", "settings", "--speculative-cap", "4", "--org", "org-1"},
	} {
		if code, stdout, _ := hl(t, args...); code != 2 || stdout != "" {
			t.Errorf("hl %s: exit code = %d, stdout = %q, want a usage error", strings.Join(args, " "), code, stdout)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("a command that does not parse reached the API: %+v", *calls)
	}
	code, stdout, stderr := hl(t, "cluster", "order", "gpu-east", "--org", "org-1")
	if code != 0 || stdout != `{"name":"gpu-east"}`+"\n" {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q, want the clusters in their order, one per line", code, stdout, stderr)
	}

	// Which clusters an order must name is the API's to refuse, and it says why.
	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"reason":"invalid_request","message":"gpu-west is missing"}`))
	}))
	code, stdout, stderr = hl(t, "cluster", "order", "gpu-east", "--org", "org-1")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "gpu-west is missing") {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q, want the refusal", code, stdout, stderr)
	}
}

func TestAnEmptyListSaysSoOnStderr(t *testing.T) {
	clitest.AccountsServer(t, nil, `{"clusters":[],"api_keys":[],"projects":[]}`)
	for _, group := range []string{"cluster", "api-key", "project"} {
		code, stdout, stderr := hl(t, group, "list", "--org", "org-1")
		if code != 0 || stdout != "" || !strings.Contains(stderr, group+" list: none") {
			t.Fatalf("hl %s list: exit code = %d, stdout = %q, stderr = %q", group, code, stdout, stderr)
		}
	}
}

func TestPlatformExperimentsListPassesSharedOnAsTheFilterItIs(t *testing.T) {
	calls := clitest.AccountsServer(t, nil, `[{"id":"pe-own","shared":false},{"id":"pe-show","shared":true}]`)
	t.Setenv("HL_PROJECT", "prj-env")
	code, stdout, stderr := hl(t, "platform-experiments", "list")
	if code != 0 || (*calls)[0].Path != "/platform-experiments?limit=20&project_id=prj-env" {
		t.Fatalf("no flag: exit code = %d, calls = %+v, stderr = %s, want no shared filter", code, *calls, stderr)
	}
	if !strings.Contains(stdout, `"shared":true`) || !strings.Contains(stdout, `"shared":false`) {
		t.Fatalf("no flag: stdout = %s, want each platform experiment with its shared mark", stdout)
	}
	if code, _, _ := hl(t, "platform-experiments", "list", "--shared"); code != 0 || (*calls)[1].Path != "/platform-experiments?limit=20&project_id=prj-env&shared=true" {
		t.Fatalf("--shared: exit code = %d, call = %+v", code, (*calls)[1])
	}
	if code, _, _ := hl(t, "platform-experiments", "--shared=false", "--project", "prj-flag", "--limit", "5"); code != 0 || (*calls)[2].Path != "/platform-experiments?limit=5&project_id=prj-flag&shared=false" {
		t.Fatalf("--shared=false with --project: exit code = %d, call = %+v", code, (*calls)[2])
	}
}

func TestPlatformExperimentsListFiltersByProject(t *testing.T) {
	calls := clitest.AccountsServer(t, nil, `[]`)
	if code, _, stderr := hl(t, "platform-experiments", "list"); code != 0 || (*calls)[0].Path != "/platform-experiments?limit=20" {
		t.Fatalf("with no project: exit code = %d, calls = %+v, stderr = %s", code, *calls, stderr)
	}
	t.Setenv("HL_PROJECT", "prj-env")
	if code, _, _ := hl(t, "platform-experiments"); code != 0 || (*calls)[1].Path != "/platform-experiments?limit=20&project_id=prj-env" {
		t.Fatalf("with $HL_PROJECT: exit code = %d, call = %+v", code, (*calls)[1])
	}
	if code, _, _ := hl(t, "platform-experiments", "list", "--project", "prj-flag"); code != 0 || (*calls)[2].Path != "/platform-experiments?limit=20&project_id=prj-flag" {
		t.Fatalf("with --project: exit code = %d, call = %+v", code, (*calls)[2])
	}
}

// Removing and revoking answer with no body, so the command says what it did. Three deletes
// still answer 200 with the resource where the conventions say 204, and hl had one helper for
// each kind of answer: a delete that moves to 204 would have printed "response is not JSON".
// One helper takes both, so every delete here is answered 204 and then with a body.
func TestACommandAnsweredWithNoBodySaysWhatItDid(t *testing.T) {
	var asked []string
	body := ""
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path)
		if body == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, body)
	})
	clitest.SetAPIURL(t, url)
	deletes := map[string]string{
		"org members remove u-1 --org org-1":                                 "removed u-1 from org org-1",
		"org invites revoke inv-1":                                           "revoked invite inv-1",
		"api-key revoke key-1":                                               "revoked API key key-1",
		"cluster remove main --org org-1":                                    "DELETE /orgs/org-1/clusters/main: done",
		"cluster forget-identity main --org org-1":                           "DELETE /orgs/org-1/clusters/main/identity: done",
		"platform-experiments agents remove ag-1 --platform-experiment pe-1": "removed ag-1 from platform experiment pe-1",
	}
	for args, said := range deletes {
		body = ""
		code, stdout, stderr := hl(t, strings.Fields(args)...)
		if code != 0 || stdout != "" || stderr != "hl: "+said+"\n" {
			t.Errorf("hl %s answered 204: exit code = %d, stdout = %q, stderr = %q, want %q on stderr", args, code, stdout, stderr, said)
		}
		body = `{"id":"the-resource"}`
		code, stdout, stderr = hl(t, strings.Fields(args)...)
		if code != 0 || stdout != body+"\n" || stderr != "" {
			t.Errorf("hl %s answered with the resource: exit code = %d, stdout = %q, stderr = %q, want the resource printed", args, code, stdout, stderr)
		}
	}
	if len(asked) != 2*len(deletes) {
		t.Fatalf("asked %v, want one call each", asked)
	}
}

func TestARoleNobodyDefinedIsRefusedBeforeAnyCall(t *testing.T) {
	calls := clitest.AccountsServer(t, nil, `{}`)
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
