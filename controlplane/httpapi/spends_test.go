package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
)

// An agent whose run is over keeps its token through the wind-down to file what it leaves, and
// every route that spends or starts work refuses it there, before any service is reached: the
// services are left out here, and reaching one would panic.
func TestARunOverTokenSpendsAndStartsNothing(t *testing.T) {
	over := agentOne
	over.RunOver = true
	r := authtest.Router(authtest.As(over))
	placement := sessionPlacement
	placement.Agents = map[string]string{"ag-1": "org-a"}
	doc := NewDoc(r, "test", "1.0.0", "", auth.NewAccess(placement), zap.NewNop())
	registerJobs(doc, nil, zap.NewNop())
	registerExperimentData(doc, nil, zap.NewNop())
	registerExperimentImage(doc, nil, zap.NewNop())
	for _, call := range []struct{ method, path, body string }{
		{"POST", "/experiments", `{"metadata":{"platform_experiment_id":"pe-1","agent_id":"ag-1","code_ref":"abc1234"},"job":{"command":["true"]}}`},
		{"POST", "/platform-experiments/pe-1/data-key", `{"agent_id":"ag-1"}`},
		{"PUT", "/platform-experiments/pe-1/image", `{"image":"docker.io/library/python:3.12"}`},
		{"POST", "/platform-experiments/pe-1/registry-credentials", ``},
	} {
		req := httptest.NewRequest(call.method, call.path, bytes.NewReader([]byte(call.body)))
		if call.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, authtest.Authorize(req))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "run over") {
			t.Errorf("%s %s: %d %s, want 403 run over", call.method, call.path, rec.Code, rec.Body.String())
		}
	}
}
