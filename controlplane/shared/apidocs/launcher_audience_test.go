package apidocs

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
)

func TestTheLauncherAudienceAdmitsALauncherOnItsOwnNameAndStaff(t *testing.T) {
	everyone := map[string]auth.Principal{
		"staff":           {Kind: auth.KindStaff},
		"own launcher":    {Kind: auth.KindLauncher, LauncherName: "main"},
		"other launcher":  {Kind: auth.KindLauncher, LauncherName: "other"},
		"unnamed":         {Kind: auth.KindLauncher},
		"runtime of name": {Kind: auth.KindRuntime, ClusterName: "main"},
		"user":            {Kind: auth.KindUser, OrgIDs: []string{"org"}},
		"key":             {Kind: auth.KindKey, OrgIDs: []string{"org"}},
		"coordinator":     {Kind: auth.KindCoordinator, PlatformExperimentID: "main"},
		"agent":           {Kind: auth.KindAgent, PlatformExperimentID: "main", AgentID: "main"},
		"job":             {Kind: auth.KindJob, ExperimentID: "main"},
		"no kind":         {},
	}
	for name, p := range everyone {
		want := name == "staff" || name == "own launcher"
		if got := AudienceLauncher.Admits(p, "main"); got != want {
			t.Errorf("launcher audience, %s: got = %v, want = %v", name, got, want)
		}
		// A launcher operation always names its launcher. One that did not admits staff alone.
		if got := AudienceLauncher.Admits(p, ""); got != (name == "staff") {
			t.Errorf("launcher audience with no name, %s: got = %v", name, got)
		}
	}
}

func TestALauncherTokenIsAdmittedNowhereElse(t *testing.T) {
	launcher := auth.Principal{Kind: auth.KindLauncher, LauncherName: "main"}
	for _, audience := range []Audience{AudienceAgent, AudienceJob, AudienceCoordinator, AudienceInternal} {
		for _, name := range []string{"", "main"} {
			if audience.Admits(launcher, name) {
				t.Errorf("a launcher token is admitted to the %s audience (name %q)", audience, name)
			}
		}
	}
}
