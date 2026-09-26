package quota

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
)

type memoryCode struct{ stored map[string][]byte }

func (m *memoryCode) PutCode(_ context.Context, pe, agent, sha string, bundle []byte) error {
	m.stored[pe+"/"+agent+"/"+sha] = bytes.Clone(bundle)
	return nil
}

func (m *memoryCode) GetCode(_ context.Context, pe, agent, sha string, _ int64) ([]byte, error) {
	bundle, found := m.stored[pe+"/"+agent+"/"+sha]
	if !found {
		return nil, objectstore.ErrNoCode
	}
	return bundle, nil
}

// codeRoster is who is in pe-1, and whether it is closed or an agent cut.
type codeRoster struct {
	joined map[string]bool
	closed bool
}

func (r codeRoster) Joined(_ context.Context, _, agentID string) (bool, error) {
	return r.joined[agentID], nil
}

func (r codeRoster) RequireActive(_ context.Context, platformExpID, _ string) error {
	if r.closed {
		return &reasonError{status: http.StatusConflict, Reason: "experiment_closed", Message: "platform experiment " + platformExpID + " is closed"}
	}
	return nil
}

const codeSHA = "0123456789abcdef0123456789abcdef01234567"

var (
	codePath   = "/platform-experiments/pe-1/agents/ag-1/code/" + codeSHA
	codeBundle = []byte("# v2 git bundle\n" + codeSHA + " HEAD\n\nPACK\x00\x00\x00\x02")
	everyone   = codeRoster{joined: map[string]bool{"ag-1": true, "ag-2": true}}
	codePlaces = authtest.Placement{
		PlatformExperiments: sessionPlacement.PlatformExperiments,
		Agents:              map[string]string{"ag-1": "org-a", "ag-2": "org-a", "ag-out": "org-a"},
	}
	jobOfAgentTwo = auth.Principal{Kind: auth.KindJob, OrgIDs: []string{"org-a"}, ProjectID: "prj-a", PlatformExperimentID: "pe-1", AgentID: "ag-2", ExperimentID: "job-9"}
)

func codeCall(t *testing.T, store CodeStore, roster CodeRoster, caller auth.Principal, method, path string, body []byte) (int, []byte) {
	t.Helper()
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(caller), auth.NewAccess(codePlaces), zap.NewNop())
	RegisterCodeHuma(doc, store, roster, zap.NewNop())
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(req))
	return rec.Code, rec.Body.Bytes()
}

// A platform experiment with no repository has nowhere else to keep the code a job ran.
func TestAnAgentStoresItsCodeAndEveryTokenOfTheExperimentReadsItBack(t *testing.T) {
	store := &memoryCode{stored: map[string][]byte{}}
	if code, body := codeCall(t, store, everyone, agentOne, "PUT", codePath, codeBundle); code != http.StatusOK || !strings.Contains(string(body), `"sha":"`+codeSHA+`"`) {
		t.Fatalf("PUT: %d %s", code, body)
	}
	readers := map[string]auth.Principal{
		"the agent": agentOne, "another agent": agentTwo, "another agent's job": jobOfAgentTwo, "the coordinator": coordinator,
		"a person of the org": {Kind: auth.KindUser, UserID: "u-1", OrgIDs: []string{"org-a"}},
	}
	for name, reader := range readers {
		if code, body := codeCall(t, store, everyone, reader, "GET", codePath, nil); code != http.StatusOK || !bytes.Equal(body, codeBundle) {
			t.Errorf("%s: GET %d, want the bundle byte for byte", name, code)
		}
	}
	if code, _ := codeCall(t, store, everyone, agentTwo, "GET", strings.Replace(codePath, "0123", "ffff", 1), nil); code != http.StatusNotFound {
		t.Errorf("a commit nothing is stored under: %d, want 404", code)
	}
}

func TestCodeIsWrittenOnlyByItsOwnAgentAndReadOnlyWithinTheExperiment(t *testing.T) {
	store := &memoryCode{stored: map[string][]byte{"pe-1/ag-1/" + codeSHA: []byte("the agent's")}}
	elsewhere := auth.Principal{Kind: auth.KindAgent, OrgIDs: []string{"org-a"}, ProjectID: "prj-a", PlatformExperimentID: "pe-2", AgentID: "ag-1"}
	stranger := auth.Principal{Kind: auth.KindUser, UserID: "u-9", OrgIDs: []string{"org-b"}}
	for name, c := range map[string]struct {
		caller auth.Principal
		method string
		want   int
	}{
		"another agent writing":                   {agentTwo, "PUT", http.StatusForbidden},
		"the coordinator writing":                 {coordinator, "PUT", http.StatusForbidden},
		"a job writing":                           {jobOfAgentTwo, "PUT", http.StatusForbidden},
		"the same agent id of another experiment": {elsewhere, "PUT", http.StatusNotFound},
		"another experiment's token reading":      {elsewhere, "GET", http.StatusNotFound},
		"another org reading":                     {stranger, "GET", http.StatusNotFound},
	} {
		var body []byte
		if c.method == "PUT" {
			body = codeBundle
		}
		if code, out := codeCall(t, store, everyone, c.caller, c.method, codePath, body); code != c.want {
			t.Errorf("%s: %d %s, want %d", name, code, out, c.want)
		}
	}
	if string(store.stored["pe-1/ag-1/"+codeSHA]) != "the agent's" {
		t.Fatalf("a refused write changed what is stored: %q", store.stored)
	}
}

func TestCodeIsRefusedWhenItIsNoBundleOfThatCommitTooLargeOrLate(t *testing.T) {
	store := &memoryCode{stored: map[string][]byte{}}
	other := strings.Repeat("9", 40)
	for name, body := range map[string][]byte{
		"a tarball":                  []byte("\x1f\x8b\x08 not a bundle"),
		"a bundle of another commit": []byte("# v2 git bundle\n" + other + " HEAD\n\nPACK"),
		"the commit as prerequisite": []byte("# v2 git bundle\n-" + codeSHA + " earlier\n" + other + " HEAD\n\nPACK"),
		"a header with no pack":      []byte("# v2 git bundle\n" + codeSHA + " HEAD\n"),
	} {
		if code, out := codeCall(t, store, everyone, agentOne, "PUT", codePath, body); code != http.StatusUnprocessableEntity || !strings.Contains(string(out), "not_a_bundle") {
			t.Errorf("%s: %d %s, want 422 not_a_bundle", name, code, out)
		}
	}
	if code, _ := codeCall(t, store, everyone, agentOne, "PUT", codePath, append(bytes.Clone(codeBundle), make([]byte, domain.MaxAgentCodeBytes)...)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("a bundle past the limit: %d, want 413", code)
	}
	if code, _ := codeCall(t, store, everyone, agentOne, "PUT", "/platform-experiments/pe-1/agents/ag-1/code/main", codeBundle); code != http.StatusNotFound {
		t.Errorf("a branch name for a commit: %d, want 404", code)
	}
	closed := codeRoster{joined: everyone.joined, closed: true}
	if code, out := codeCall(t, store, closed, agentOne, "PUT", codePath, codeBundle); code != http.StatusConflict {
		t.Errorf("after the close: %d %s, want 409", code, out)
	}
	person := auth.Principal{Kind: auth.KindUser, UserID: "u-1", OrgIDs: []string{"org-a"}}
	if code, out := codeCall(t, store, everyone, person, "PUT", "/platform-experiments/pe-1/agents/ag-out/code/"+codeSHA, codeBundle); code != http.StatusNotFound {
		t.Errorf("for an agent that is not in the experiment: %d %s, want 404", code, out)
	}
	if len(store.stored) != 0 {
		t.Fatalf("a refused write stored %v", store.stored)
	}
	// A v3 bundle carries capability lines before its tips.
	v3 := []byte("# v3 git bundle\n@object-format=sha1\n" + codeSHA + " refs/heads/main\n\nPACK")
	if code, out := codeCall(t, store, everyone, agentOne, "PUT", codePath, v3); code != http.StatusOK {
		t.Errorf("a v3 bundle: %d %s", code, out)
	}
}
