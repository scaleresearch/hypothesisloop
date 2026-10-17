package apitest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/watch"
)

// The callers of the table, none of them staff: a signed-in owner of org-a (user), a signed-in
// member of it who is no owner, an API key of org-a, one bound to its project prj-a1, agent-1's token in pe-a1, pe-a1's coordinator, the
// token of agent-1's job, cluster-a's runtime, and a request with no token.
var principals = []string{"user", "member", "org key", "project key", "agent", "coordinator", "job", "runtime", "build", "none"}

// Whose object a request names. own is agent-1's in pe-a1, peer another agent's in the same
// platform experiment, sibling one in pe-a2 (same org, another project), foreign one in org-b,
// absent an id with no row, and shared one in the platform experiment of org-b that staff shared
// with every account.
var owners = []string{"own", "peer", "sibling", "foreign", "absent", "shared"}

var named = map[string]map[string]string{
	"own":     {"pe": "pe-a1", "job": "job-own", "don": "don-own", "hyp": "hyp-own", "agent": "agent-1", "cluster": "cluster-a", "org": "org-a", "prj": "prj-a1"},
	"peer":    {"pe": "pe-a1", "job": "job-peer", "don": "don-peer", "hyp": "hyp-peer", "agent": "agent-2", "cluster": "cluster-a", "org": "org-a", "prj": "prj-a1"},
	"sibling": {"pe": "pe-a2", "job": "job-sibling", "don": "don-sibling", "hyp": "hyp-sibling", "agent": "agent-2", "cluster": "cluster-a", "org": "org-a", "prj": "prj-a2"},
	"foreign": {"pe": "pe-b", "job": "job-foreign", "don": "don-foreign", "hyp": "hyp-foreign", "agent": "agent-9", "cluster": "cluster-b", "org": "org-b", "prj": "prj-b"},
	"shared":  {"pe": "pe-show", "job": "job-shared", "don": "don-shared", "hyp": "hyp-shared", "agent": "agent-9", "cluster": "cluster-b", "org": "org-b", "prj": "prj-b"},
	"absent":  {"pe": "pe-none", "job": "job-none", "don": "don-none", "hyp": "hyp-none", "agent": "agent-none", "cluster": "cluster-none", "org": "org-none", "prj": "prj-none"},
}

func fill(template, owner string) string {
	for name, value := range named[owner] {
		template = strings.ReplaceAll(template, "<"+name+">", value)
	}
	return template
}

// route is one registered operation: the path it is registered under, the request that reaches
// it for each owner, and the rule its answers follow.
type route struct {
	method, path string
	// request is the path and query to call, with <pe>, <job>, <don>, <hyp>, <agent>, <cluster>,
	// <org> and <prj> standing for the owner's ids. Empty means path, whose {id} is not an owner's.
	request string
	body    string
	rule    rule
	// sharedRead is the route's cell for another org's account on the shared platform
	// experiment: set, an account reads there what it reads of its own; unset, the route answers
	// every caller there exactly as on a platform experiment of another org.
	sharedRead bool
}

// want is what the route answers the caller for an object of the owner.
func (r route) want(principal, owner string) int {
	if owner != "shared" {
		return r.rule(principal, owner)
	}
	if r.sharedRead && among(people, principal) {
		return r.rule(principal, "own")
	}
	return r.rule(principal, "foreign")
}

func (api *worldAPI) call(method, target, body, token string) int {
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, target, nil)
	} else {
		request = httptest.NewRequest(method, target, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
	}
	switch {
	case token == "":
	case target == "/watch-with-header":
		request = httptest.NewRequest(method, "/watch?platform_experiment_id=pe-a1", nil)
		request.Header.Set("Authorization", "Bearer "+token)
	case strings.HasPrefix(target, "/watch?"):
		request.Header.Set("Sec-WebSocket-Protocol", watch.Subprotocol+", "+token)
	default:
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	api.serve.ServeHTTP(recorder, request)
	return recorder.Code
}

// registered lists every operation of the three documents as "METHOD /full/path", plus the two
// handlers mounted beside them.
func (api *worldAPI) registered() []string {
	out := []string{"GET /watch", "GET /metrics"}
	for prefix, doc := range api.docs {
		for path, item := range doc.API.OpenAPI().Paths {
			for method, op := range map[string]any{"GET": item.Get, "POST": item.Post, "PUT": item.Put, "PATCH": item.Patch, "DELETE": item.Delete} {
				if fmt.Sprint(op) != "<nil>" {
					out = append(out, method+" "+strings.TrimSuffix(prefix+path, "/"))
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestEveryRouteAnswersEachCallerAsItsRuleSays(t *testing.T) {
	api := newWorldAPI(t)

	known := map[string]bool{}
	for _, r := range routes {
		key := r.method + " " + r.path
		if known[key] {
			t.Errorf("%s is in the table twice", key)
		}
		known[key] = true
	}
	for _, key := range api.registered() {
		if !known[key] {
			t.Errorf("%s is registered and the table does not know it: add it with the rule its answers follow", key)
		}
		delete(known, key)
	}
	for key := range known {
		t.Errorf("%s is in the table and no longer registered", key)
	}

	discover := os.Getenv("HL_AUTHZ_DISCOVER") != ""
	for _, r := range routes {
		for _, owner := range owners {
			target := r.request
			if target == "" {
				target = r.path
			}
			target, body := fill(target, owner), fill(r.body, owner)
			var line []string
			for _, principal := range principals {
				got := api.call(r.method, target, body, api.tokens[principal])
				line = append(line, fmt.Sprint(got))
				if discover || r.rule == nil {
					continue
				}
				if want := r.want(principal, owner); got != want {
					t.Errorf("%s %s as %s on the %s object: got = %d, want = %d", r.method, target, principal, owner, got, want)
				}
			}
			if discover {
				fmt.Printf("%-7s %-58s %-8s %s\n", r.method, r.path, owner, strings.Join(line, " "))
			}
			if !strings.Contains(r.request+r.body, "<") {
				break
			}
		}
	}
}

func TestTheWaysPastTheTableStayClosed(t *testing.T) {
	api := newWorldAPI(t)
	for _, c := range cases {
		token, ok := api.tokens[c.principal]
		if !ok {
			t.Fatalf("%s: no token for %q", c.why, c.principal)
		}
		if got := api.call(c.method, c.target, c.body, token); got != c.want {
			t.Errorf("%s (%s %s as %s): got = %d, want = %d", c.why, c.method, c.target, c.principal, got, c.want)
		}
	}
}
