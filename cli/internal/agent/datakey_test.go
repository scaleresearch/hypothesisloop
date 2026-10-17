package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

func TestDataKeyPrintsTheJobVariablesAsExportLines(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]string
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"uri": "s3://hl-data/pe-1/bot-1/", "shared": "s3://hl-data/pe-1/",
			"endpoint": "http://store:9000", "region": "us-east-1",
			"access_key_id": "AK", "secret_access_key": "S'K",
		})
	})
	clitest.SetAPIURL(t, url)
	t.Setenv("HL_TOKEN", clitest.DerivedToken("agent:pe-1/bot-1"))

	var stdout, stderr bytes.Buffer
	if code := clitest.Run(DataKey, []string{}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotMethod != "POST" || gotPath != "/platform-experiments/pe-1/data-key" || gotBody["agent_id"] != "bot-1" {
		t.Fatalf("got %s %s %+v", gotMethod, gotPath, gotBody)
	}
	want := `export AWS_ACCESS_KEY_ID='AK'
export AWS_SECRET_ACCESS_KEY='S'\''K'
export AWS_ENDPOINT_URL='http://store:9000'
export AWS_ENDPOINT_URL_S3='http://store:9000'
export AWS_REGION='us-east-1'
export HYPOTHESISLOOP_DATA_URI='s3://hl-data/pe-1/bot-1/'
export HYPOTHESISLOOP_DATA_SHARED='s3://hl-data/pe-1/'
`
	if stdout.String() != want {
		t.Fatalf("stdout =\n%s\nwant\n%s", stdout.String(), want)
	}
}

func TestDataKeyNeedsAnAgentAndAPlatformExperiment(t *testing.T) {
	t.Setenv("HL_TOKEN", "hlk_x")
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(DataKey, []string{}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage: hl data-key") {
		t.Fatalf("exit %d, stderr %q, want 2 and the usage", code, stderr.String())
	}
}
