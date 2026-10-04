package scheduler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// assigning records the keys it was asked to assign and answers each building.
type assigning struct{ keys *[]string }

func (a assigning) AssignBuild(_ context.Context, key string) (*domain.Environment, error) {
	*a.keys = append(*a.keys, key)
	return &domain.Environment{Key: key, Base: "registry.test/job:1", State: domain.EnvironmentBuilding, ClusterName: "builder-a", Attempt: 1}, nil
}

// awaiting records who was to be told the end of which build.
type awaiting struct {
	poolOnlyStore
	awaited *[]string
}

func (a awaiting) AwaitEnvironmentBuild(_ context.Context, key, peID, agentID, codeRef, image string) error {
	*a.awaited = append(*a.awaited, strings.Join([]string{key, peID, agentID, codeRef, image}, " "))
	return nil
}

// An environment asked for ahead of its job is the one a job of that commit on that image waits
// for, under the same key, and its build is assigned at once. The agent that asked is told when
// it ends.
func TestAnEnvironmentBuiltAheadIsTheOneItsJobWaitsFor(t *testing.T) {
	commit := strings.Repeat("a", 40)
	codeRef := "https://example.com/repo.git@" + commit
	var asked, assigned, awaited []string
	s := &Service{store: awaiting{awaited: &awaited}, codeHost: testCodeHost{}, environments: declared{files: map[string]string{commit: rustEnvironment}, asked: &asked}, builds: assigning{&assigned}}
	env, err := s.BuildEnvironment(context.Background(), "pe-1", "agent-a", codeRef, "registry.test/job:1")
	if err != nil {
		t.Fatal(err)
	}
	want := domain.EnvironmentKey("org-a", "", rustEnvironment, "registry.test/job:1", "")
	if env.Key != want || !slices.Equal(assigned, []string{want}) || !slices.Equal(asked, []string{"org-a registry.test/job:1"}) {
		t.Errorf("built %q, assigned %v, asked %v, want %q for org-a on the image", env.Key, assigned, asked, want)
	}
	if told := []string{want + " pe-1 agent-a " + codeRef + " registry.test/job:1"}; !slices.Equal(awaited, told) {
		t.Errorf("awaited %v, want %v", awaited, told)
	}

	job := humanIdeaSubmission()
	if err := (&Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: admittingStore{}, hypotheses: humanHypothesis("pe-1"), novelty: novel{},
		quota: insertingQuota{}, loop: wokenLoop{new(int)}, environments: declared{files: map[string]string{commit: rustEnvironment}}}).Submit(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if job.EnvironmentKey != env.Key {
		t.Errorf("the job of that commit waits for %q, want the one built ahead, %q", job.EnvironmentKey, env.Key)
	}
	if got := answerEnvironment(env); got.State != domain.EnvironmentBuilding || !strings.Contains(got.Message, "It builds on cluster builder-a, attempt 1") {
		t.Errorf("answered %+v, want it building on builder-a", got)
	}
}

func TestAnEnvironmentIsNotBuiltAheadForACommitWithoutOneOrOutsideTheRepository(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for name, c := range map[string]struct {
		codeRef, image string
		reason         string
	}{
		"no file":              {"https://example.com/repo.git@" + strings.Repeat("b", 40), "registry.test/job:1", ReasonNoEnvironmentFile},
		"another repository":   {"https://example.com/other.git@" + commit, "registry.test/job:1", ReasonCodeRefNotInRepository},
		"a branch":             {"https://example.com/repo.git@main", "registry.test/job:1", ReasonMalformed},
		"an image of no place": {"https://example.com/repo.git@" + commit, "python", domain.ReasonUnknownJobImage},
	} {
		var assigned []string
		s := &Service{store: poolOnlyStore{}, codeHost: testCodeHost{}, environments: declared{files: map[string]string{commit: rustEnvironment}}, builds: assigning{&assigned}}
		var refused *AdmissionError
		if _, err := s.BuildEnvironment(context.Background(), "pe-1", "", c.codeRef, c.image); !errors.As(err, &refused) || refused.Reason != c.reason || len(assigned) != 0 {
			t.Errorf("%s: got = %v with %v assigned, want %s and nothing assigned", name, err, assigned, c.reason)
		}
	}
}

// An environment is read as it is, building nothing: a failed one stays failed, and one never
// asked for, or a commit without the file, is told so.
func TestAnEnvironmentIsReadWithoutBuildingIt(t *testing.T) {
	commit := strings.Repeat("a", 40)
	key := domain.EnvironmentKey("org-a", "", rustEnvironment, "registry.test/job:1", "")
	failed := &domain.Environment{Key: key, Base: "registry.test/job:1", State: domain.EnvironmentFailed, Fault: domain.EnvironmentFaultFile, LogTail: "error: unexpected argument '--locked=false'"}
	var assigned []string
	s := &Service{store: poolOnlyStore{}, codeHost: testCodeHost{}, builds: assigning{&assigned},
		environments: declared{files: map[string]string{commit: rustEnvironment}, known: map[string]*domain.Environment{key: failed}}}
	env, err := s.FindEnvironment(context.Background(), "pe-1", "agent-a", "https://example.com/repo.git@"+commit, "registry.test/job:1")
	if err != nil || env != failed || len(assigned) != 0 {
		t.Fatalf("got %+v, %v with %v assigned, want the failed one as it is and nothing assigned", env, err, assigned)
	}
	if got := answerEnvironment(env); got.State != domain.EnvironmentFailed || !strings.Contains(got.Message, "--locked=false") {
		t.Errorf("answered %+v, want it failed with the end of its output", got)
	}

	for name, c := range map[string]struct {
		commit string
		known  map[string]*domain.Environment
		reason string
	}{
		"never asked": {commit, nil, ReasonEnvironmentNotRequested},
		"no file":     {strings.Repeat("b", 40), nil, ReasonNoEnvironmentFile},
	} {
		s.environments = declared{files: map[string]string{commit: rustEnvironment}, known: c.known}
		var refused *AdmissionError
		if _, err := s.FindEnvironment(context.Background(), "pe-1", "agent-a", "https://example.com/repo.git@"+c.commit, "registry.test/job:1"); !errors.As(err, &refused) || refused.Reason != c.reason {
			t.Errorf("%s: got %v, want %s", name, err, c.reason)
		}
	}
}

// An agent that reads its environment building is told once it ends, as one that asked for it is.
// A person's read, and one of a build already over, waits for nothing.
func TestAnAgentThatReadsABuildingEnvironmentIsToldItEnded(t *testing.T) {
	commit := strings.Repeat("a", 40)
	codeRef := "https://example.com/repo.git@" + commit
	key := domain.EnvironmentKey("org-a", "", rustEnvironment, "registry.test/job:1", "")
	for name, c := range map[string]struct {
		agent string
		state domain.EnvironmentState
		told  []string
	}{
		"an agent, building": {"agent-a", domain.EnvironmentBuilding, []string{key + " pe-1 agent-a " + codeRef + " registry.test/job:1"}},
		"a person, building": {"", domain.EnvironmentBuilding, nil},
		"an agent, ready":    {"agent-a", domain.EnvironmentReady, nil},
	} {
		var awaited []string
		s := &Service{store: awaiting{awaited: &awaited}, codeHost: testCodeHost{},
			environments: declared{files: map[string]string{commit: rustEnvironment}, known: map[string]*domain.Environment{key: {Key: key, State: c.state}}}}
		if _, err := s.FindEnvironment(context.Background(), "pe-1", c.agent, codeRef, "registry.test/job:1"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Equal(awaited, c.told) {
			t.Errorf("%s: awaited %v, want %v", name, awaited, c.told)
		}
	}
}
