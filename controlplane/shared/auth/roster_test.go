package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func (d *fakeDirectory) IsSignedUp(_ context.Context, pe, agent string) (bool, error) {
	return !d.notSignedUp[pe+"/"+agent], d.fail
}

func (d *fakeDirectory) AssignedEnvironmentBuild(_ context.Context, cluster, key string, attempt int) (*domain.Environment, error) {
	if !d.builds[BuildScope(cluster, key, attempt)] {
		return nil, d.fail
	}
	return &domain.Environment{Key: key, ClusterName: cluster, Attempt: attempt}, d.fail
}

// A build token reads its repository only while that attempt of the build is assigned to that
// cluster and building: a later attempt, another cluster, or a build that ended refuse it.
func TestABuildTokenWorksOnlyWhileItsBuildIsBuilding(t *testing.T) {
	a, directory := newTestAuthenticator(t)
	token := mustMint(t, testSecret, BuildScope("cluster-a", "key-1", 1))

	p, err := a.Authenticate(context.Background(), token)
	assertRefused(t, "build not assigned", p, err)

	directory.builds = map[string]bool{BuildScope("cluster-a", "key-1", 1): true}
	p, err = a.Authenticate(context.Background(), token)
	if err != nil || p.Kind != KindBuild || p.Build != (BuildRef{Cluster: "cluster-a", Key: "key-1", Attempt: 1}) {
		t.Fatalf("build building: got = %+v, %v, want the build principal", p, err)
	}
	if len(p.OrgIDs) != 0 || p.ProjectID != "" || p.ClusterName != "" || p.Scope().Admits(tenancyA) {
		t.Errorf("build principal: got = %+v, want one that names no org, project or cluster", p)
	}

	directory.builds = map[string]bool{BuildScope("cluster-a", "key-1", 2): true, BuildScope("cluster-b", "key-1", 1): true}
	p, err = a.Authenticate(context.Background(), token)
	assertRefused(t, "build moved on to another attempt or cluster", p, err)

	directory.builds = nil
	directory.fail = errors.New("db down")
	if _, err := a.Authenticate(context.Background(), token); !errors.Is(err, directory.fail) {
		t.Errorf("directory down: got = %v, want its error", err)
	}
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

// A failing lookup is the platform's failure and is never read as a refusal or an admission.
func TestALookupFailureIsNeitherAdmissionNorRefusal(t *testing.T) {
	for name, scope := range map[string]string{"agent": AgentScope("pe-a", "agent-1")} {
		a, directory := newTestAuthenticator(t)
		directory.fail = context.DeadlineExceeded
		_, err := a.Authenticate(context.Background(), mustMint(t, testSecret, scope))
		var refusal *Error
		if err == nil || errors.As(err, &refusal) {
			t.Errorf("%s: got = %v, want an error that is not a refusal", name, err)
		}
	}
}
