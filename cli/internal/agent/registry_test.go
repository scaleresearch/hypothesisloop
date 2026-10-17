package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
	"github.com/scaleresearch/hypothesisloop/contract/agentsandbox"
)

// registryServer answers each push login with the next of answers, a login or a refusal's reason,
// the last one again once they run out, and counts the requests.
func registryServer(t *testing.T, answers ...string) *int {
	t.Helper()
	asked := 0
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/platform-experiments/pe-1/registry-credentials" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		answer := answers[min(asked, len(answers)-1)]
		asked++
		w.Header().Set("Content-Type", "application/json")
		switch answer {
		case "login":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"registry": "europe-docker.pkg.dev", "username": "oauth2accesstoken", "password": "fresh-" + strings.Repeat("x", asked),
				"expires_at": time.Now().Add(30 * time.Minute), "repository": "europe-docker.pkg.dev/hl/org-1/pe-1",
			})
		case "registry_preparing", "registry_unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"reason": answer, "message": "being created"})
		case "no_registry", "experiment_closed":
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"reason": answer, "message": "refused: " + answer})
		}
	})
	clitest.SetAPIURL(t, url)
	t.Setenv(agentsandbox.EnvToken, clitest.DerivedToken("agent:pe-1/bot-1"))
	return &asked
}

func helper(server string, args ...string) (int, string) {
	var stdout bytes.Buffer
	c := apiclient.New(os.Getenv(agentsandbox.EnvAPIURL), os.Getenv(agentsandbox.EnvToken))
	code := CredentialHelper(c, args, strings.NewReader(server+"\n"), &stdout)
	return code, strings.TrimSpace(stdout.String())
}

func TestTheCredentialHelperGivesAFreshLoginOfThePlatformsRegistryOnEveryGet(t *testing.T) {
	asked := registryServer(t, "login")
	var secrets []string
	for range 2 {
		code, out := helper("europe-docker.pkg.dev", "get")
		var got struct{ ServerURL, Username, Secret string }
		if code != 0 || json.Unmarshal([]byte(out), &got) != nil {
			t.Fatalf("exit %d, stdout %q", code, out)
		}
		if got.ServerURL != "europe-docker.pkg.dev" || got.Username != "oauth2accesstoken" {
			t.Fatalf("got %+v", got)
		}
		secrets = append(secrets, got.Secret)
	}
	if *asked != 2 || secrets[0] == secrets[1] {
		t.Fatalf("asked %d times, secrets %v: each get must fetch a login of its own", *asked, secrets)
	}
	if code, _ := helper("https://Europe-Docker.pkg.dev/v2/", "get"); code != 0 {
		t.Fatal("a server written with a scheme and a path is not matched to its host")
	}
}

func fastWaits(t *testing.T) {
	poll, limit := preparingPoll, preparingLimit
	preparingPoll, preparingLimit = time.Millisecond, time.Minute
	t.Cleanup(func() { preparingPoll, preparingLimit = poll, limit })
}

func TestTheCredentialHelperWaitsWhileTheRegistryPreparesOrDoesNotAnswer(t *testing.T) {
	fastWaits(t)
	asked := registryServer(t, "registry_preparing", "registry_unavailable", "login")
	if code, out := helper("europe-docker.pkg.dev", "get"); code != 0 || !strings.Contains(out, `"Secret":"fresh-xxx"`) || *asked != 3 {
		t.Fatalf("exit %d, stdout %q, asked %d", code, out, *asked)
	}
}

func TestTheCredentialHelperHasNoLoginOfAnyOtherRegistry(t *testing.T) {
	for _, answer := range []string{"login", "no_registry"} {
		t.Run(answer, func(t *testing.T) {
			registryServer(t, answer)
			if code, out := helper("docker.io", "get"); code != 1 || out != credentialsNotFound {
				t.Fatalf("exit %d, stdout %q, want the not-found answer a pull goes on without a login after", code, out)
			}
		})
	}
}

func TestTheCredentialHelperSaysWhyItCannotGetALogin(t *testing.T) {
	registryServer(t, "experiment_closed")
	if code, out := helper("europe-docker.pkg.dev", "get"); code != 1 || !strings.Contains(out, "refused: experiment_closed") {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
}

func TestTheCredentialHelperKeepsNothing(t *testing.T) {
	asked := registryServer(t, "login")
	if code, out := helper("", "list"); code != 0 || out != "{}" {
		t.Fatalf("list: exit %d, %q", code, out)
	}
	if code, out := helper("europe-docker.pkg.dev", "erase"); code != 1 || out != credentialsNotFound {
		t.Fatalf("erase: exit %d, %q", code, out)
	}
	if code, _ := helper(`{"ServerURL":"x","Username":"u","Secret":"s"}`, "store"); code != 1 {
		t.Fatalf("store: exit %d, want a refusal", code)
	}
	if *asked != 0 {
		t.Fatalf("asked the platform %d times", *asked)
	}
}

func TestImageRepositoryWaitsWhileTheOrgsRepositoryIsCreated(t *testing.T) {
	fastWaits(t)
	asked := registryServer(t, "registry_preparing", "registry_preparing", "login")
	code, stdout, stderr := clitest.Output(t, ImageRepository)
	if code != 0 || stdout != "europe-docker.pkg.dev/hl/org-1/pe-1\n" || *asked != 3 {
		t.Fatalf("exit %d, stdout %q, stderr %q, asked %d", code, stdout, stderr, *asked)
	}
	if strings.Contains(stdout+stderr, "fresh-") {
		t.Fatal("the login is printed")
	}
}

func TestImageRepositoryFailsOnAnyOtherRefusal(t *testing.T) {
	registryServer(t, "no_registry")
	if code, _, stderr := clitest.Output(t, ImageRepository); code == 0 || !strings.Contains(stderr, "no_registry") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}
