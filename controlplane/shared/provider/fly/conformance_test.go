package fly

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/provider/providertest"
)

// The conformance tests rent real Machines and cost real money, under a cent a run. They run
// when FLY_API_TOKEN holds a token for the organization, in an app of their own that they
// create and delete, and rent with a deploy token they make for that app:
//
//	FLY_API_TOKEN=... FLY_CONFORMANCE_ORG=<org slug> FLY_CONFORMANCE_APP=<app name> FLY_CONFORMANCE_REGION=ams \
//	  go test ./controlplane/shared/provider/fly -run Conformance -v -timeout 45m
//
// FLY_CONFORMANCE_HOLD=1 adds the thirty minute hold. One run per app at a time: a run starts by
// destroying what an aborted earlier run left under the suite's prefix. The app name is the
// caller's to choose and must be the same on every run, or an aborted run's app is never found
// again.
func conformanceOptions() providertest.Options {
	return providertest.Options{
		Template: provider.Spec{
			Image:       "alpine/curl:8.14.1",
			CPUMillis:   1000,
			MemoryBytes: 1 << 30,
			DiskBytes:   8 << 30,
		},
		OutboundURL:   "https://example.com/",
		StartTimeout:  5 * time.Minute,
		SettleTimeout: 5 * time.Minute,
	}
}

func conformanceProvider(t *testing.T) *Provider {
	t.Helper()
	if os.Getenv("FLY_API_TOKEN") == "" {
		t.Skip("FLY_API_TOKEN is not set")
	}
	org, app, region := os.Getenv("FLY_CONFORMANCE_ORG"), os.Getenv("FLY_CONFORMANCE_APP"), os.Getenv("FLY_CONFORMANCE_REGION")
	if org == "" || app == "" || region == "" {
		t.Fatal("FLY_CONFORMANCE_ORG, FLY_CONFORMANCE_APP and FLY_CONFORMANCE_REGION must be set")
	}
	settings := Settings{APITokenEnv: "FLY_API_TOKEN", App: app, Region: region, CPUKind: "shared"}
	owner, err := New(settings, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ownApp(t, owner, org)

	// The suite itself runs on a deploy token for the one app, which is what the settings ask
	// for, so a call the adapter made above the app would fail here.
	var deploy struct {
		Token string `json:"token"`
	}
	if _, err := owner.call(t.Context(), http.MethodPost, owner.machinesURL+"/v1/apps/"+app+"/deploy_token", map[string]string{"expiry": "2h"}, &deploy); err != nil {
		t.Fatalf("create a deploy token for app %s: %v", app, err)
	}
	t.Setenv("FLY_CONFORMANCE_DEPLOY_TOKEN", deploy.Token)
	settings.APITokenEnv = "FLY_CONFORMANCE_DEPLOY_TOKEN"
	p, err := New(settings, nil)
	if err != nil {
		t.Fatalf("New with the app's deploy token: %v", err)
	}
	return p
}

// ownApp creates the suite's app unless an aborted run left it, and deletes it when the test ends.
func ownApp(t *testing.T, p *Provider, org string) {
	t.Helper()
	appURL := p.machinesURL + "/v1/apps/" + p.cfg.App
	var shown struct {
		Organization struct {
			Slug string `json:"slug"`
		} `json:"organization"`
	}
	_, err := p.call(t.Context(), http.MethodGet, appURL, nil, &shown)
	switch {
	case hasStatus(err, http.StatusNotFound):
		if _, err := p.call(t.Context(), http.MethodPost, p.machinesURL+"/v1/apps", map[string]string{"name": p.cfg.App, "org_slug": org}, nil); err != nil {
			t.Fatalf("create app %s: %v", p.cfg.App, err)
		}
	case err != nil:
		t.Fatalf("read app %s: %v", p.cfg.App, err)
	case shown.Organization.Slug != org:
		t.Fatalf("app %s belongs to organization %q, want %q: it is not the suite's to use", p.cfg.App, shown.Organization.Slug, org)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := p.call(ctx, http.MethodDelete, appURL, nil, nil); err != nil {
			t.Errorf("app %s is still there: %v", p.cfg.App, err)
		}
	})
}

func TestConformance(t *testing.T) {
	providertest.Run(t, conformanceProvider(t), conformanceOptions())
}

func TestConformancePause(t *testing.T) {
	providertest.RunPause(t, conformanceProvider(t), conformanceOptions(), providertest.SameProcessOrRestart)
}

func TestConformanceWarm(t *testing.T) {
	providertest.RunWarm(t, conformanceProvider(t), conformanceOptions())
}

func TestConformanceHold(t *testing.T) {
	if os.Getenv("FLY_CONFORMANCE_HOLD") == "" {
		t.Skip("FLY_CONFORMANCE_HOLD is not set")
	}
	providertest.RunHold(t, conformanceProvider(t), conformanceOptions(), 30*time.Minute)
}
