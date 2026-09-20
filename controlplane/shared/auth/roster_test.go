package auth

import (
	"context"
	"errors"
	"testing"
)

func (d *fakeDirectory) IsSignedUp(_ context.Context, pe, agent string) (bool, error) {
	return !d.notSignedUp[pe+"/"+agent], d.fail
}

func (d *fakeDirectory) LauncherKnown(_ context.Context, name string) (bool, error) {
	return d.launchers[name], d.fail
}

// Joining is done for an agent, never by it, so its token has nothing to stand on until the
// signup exists and nothing once the signup is removed.
func TestAnAgentTokenWorksOnlyWhileItsSignupExists(t *testing.T) {
	a, directory := newTestAuthenticator(t)
	token := mustMint(t, testSecret, AgentScope("pe-a", "agent-1"))

	directory.notSignedUp = map[string]bool{"pe-a/agent-1": true}
	p, err := a.Authenticate(context.Background(), token)
	assertRefused(t, "agent that never joined", p, err)

	directory.notSignedUp = nil
	p, err = a.Authenticate(context.Background(), token)
	if err != nil || p.Kind != KindAgent || p.AgentID != "agent-1" || p.PlatformExperimentID != "pe-a" {
		t.Fatalf("agent that joined: got = %+v, %v, want the agent principal", p, err)
	}

	directory.notSignedUp = map[string]bool{"pe-a/agent-1": true}
	p, err = a.Authenticate(context.Background(), token)
	assertRefused(t, "agent that was removed", p, err)

	// Another agent's signup in the same platform experiment does not carry this one.
	other := mustMint(t, testSecret, AgentScope("pe-a", "agent-2"))
	if p, err := a.Authenticate(context.Background(), other); err != nil || p.AgentID != "agent-2" {
		t.Fatalf("its neighbour: got = %+v, %v, want admitted", p, err)
	}
}

func TestALauncherTokenWorksWhileTheLauncherIsKnown(t *testing.T) {
	a, directory := newTestAuthenticator(t)
	token := mustMint(t, testSecret, LauncherScope("main"))

	p, err := a.Authenticate(context.Background(), token)
	assertRefused(t, "launcher nothing names", p, err)

	directory.launchers = map[string]bool{"main": true}
	p, err = a.Authenticate(context.Background(), token)
	if err != nil || p.Kind != KindLauncher || p.LauncherName != "main" {
		t.Fatalf("known launcher: got = %+v, %v, want the launcher principal", p, err)
	}
	if len(p.OrgIDs) != 0 || p.Scope().Admits(tenancyA) {
		t.Fatalf("a launcher principal reaches into an org: %+v", p)
	}

	p, err = a.Authenticate(context.Background(), mustMint(t, testSecret, LauncherScope("other")))
	assertRefused(t, "another launcher", p, err)

	p, err = a.Authenticate(context.Background(), mustMint(t, "another-secret-of-the-same-length-x", LauncherScope("main")))
	assertRefused(t, "launcher token of another secret", p, err)
}

// A failing lookup is the platform's failure and is never read as a refusal or an admission.
func TestALookupFailureIsNeitherAdmissionNorRefusal(t *testing.T) {
	for name, scope := range map[string]string{"launcher": LauncherScope("main"), "agent": AgentScope("pe-a", "agent-1")} {
		a, directory := newTestAuthenticator(t)
		directory.launchers = map[string]bool{"main": true}
		directory.fail = context.DeadlineExceeded
		_, err := a.Authenticate(context.Background(), mustMint(t, testSecret, scope))
		var refusal *Error
		if err == nil || errors.As(err, &refusal) {
			t.Errorf("%s: got = %v, want an error that is not a refusal", name, err)
		}
	}
}
