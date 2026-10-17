package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/scope"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

func (d *fakeDirectory) IsSignedUp(_ context.Context, pe, agent string) (bool, error) {
	return !d.notSignedUp[pe+"/"+agent], d.fail
}

// Joining is done for an agent, never by it, so its token has nothing to stand on until the
// signup exists and nothing once the signup is removed.
func TestAnAgentTokenWorksOnlyWhileItsSignupExists(t *testing.T) {
	a, directory := newTestAuthenticator(t)
	token := mustMint(t, testSecret, scope.Agent("pe-a", "agent-1"))

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
	other := mustMint(t, testSecret, scope.Agent("pe-a", "agent-2"))
	if p, err := a.Authenticate(context.Background(), other); err != nil || p.AgentID != "agent-2" {
		t.Fatalf("its neighbour: got = %+v, %v, want admitted", p, err)
	}
}

// A failing lookup is the platform's failure and is never read as a refusal or an admission.
func TestALookupFailureIsNeitherAdmissionNorRefusal(t *testing.T) {
	for name, scope := range map[string]string{"agent": scope.Agent("pe-a", "agent-1")} {
		a, directory := newTestAuthenticator(t)
		directory.fail = context.DeadlineExceeded
		_, err := a.Authenticate(context.Background(), mustMint(t, testSecret, scope))
		var refused *refusal.Refusal
		if err == nil || errors.As(err, &refused) {
			t.Errorf("%s: got = %v, want an error that is not a refusal", name, err)
		}
	}
}
