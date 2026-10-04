package scheduler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// declared is a repository whose commits declare the environment files held by commit, and none
// otherwise. bases records the base each was asked to be built on, and ensured the bases a file
// was built on besides.
type declared struct {
	files   map[string]string
	err     error
	bases   *[]string
	ensured *[]string
}

func (d declared) Ensure(_ context.Context, dockerfile, base string) (*domain.Environment, error) {
	*d.ensured = append(*d.ensured, base)
	return &domain.Environment{Key: domain.EnvironmentKey(dockerfile, base), Base: base, Dockerfile: dockerfile, State: domain.EnvironmentBuilding}, nil
}

func (d declared) Declared(_ context.Context, _, _, ref, base string) (*domain.Environment, error) {
	if d.err != nil {
		return nil, d.err
	}
	file, ok := d.files[ref]
	if !ok {
		return nil, nil
	}
	if d.bases != nil {
		*d.bases = append(*d.bases, base)
	}
	if err := domain.ValidateEnvironmentFile(file); err != nil {
		return nil, err
	}
	return &domain.Environment{Key: domain.EnvironmentKey(file, base), Base: base, Dockerfile: file, State: domain.EnvironmentBuilding}, nil
}

const rustEnvironment = "ARG BASE\nFROM ${BASE}\nRUN apt-get install -y rustc\n"

// baselineDeclaredStore is a platform experiment whose baseline is declared.
type baselineDeclaredStore struct{ admittingStore }

func (s baselineDeclaredStore) GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
	pe, err := s.admittingStore.GetPlatformExperiment(ctx, id)
	pe.BaselineJobID = "baseline-job"
	return pe, err
}

// A job runs in the environment its own commit declares, built on the image it names, and a
// commit that declares none leaves the job as it was. Until the baseline is declared the file is
// built on the agents' image too, so the declaration does not wait for that build.
func TestASubmitKeepsTheEnvironmentItsCommitDeclaresOnItsImage(t *testing.T) {
	commit := strings.Repeat("a", 40)
	var bases, ensured []string
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: admittingStore{}, hypotheses: humanHypothesis("pe-1"), novelty: novel{},
		quota: insertingQuota{}, loop: wokenLoop{new(int)}, environments: declared{files: map[string]string{commit: rustEnvironment}, bases: &bases, ensured: &ensured},
		agentImage: "registry.test/agent:1"}
	exp := humanIdeaSubmission()
	if err := s.Submit(context.Background(), exp); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if want := domain.EnvironmentKey(rustEnvironment, "registry.test/job:1"); exp.EnvironmentKey != want || len(bases) != 1 || bases[0] != "registry.test/job:1" {
		t.Errorf("environment %q built on %v, want %q on the job's image", exp.EnvironmentKey, bases, want)
	}
	if len(ensured) != 1 || ensured[0] != "registry.test/agent:1" {
		t.Errorf("before the baseline is declared the file is built besides on %v, want the agents' image", ensured)
	}

	s.store = baselineDeclaredStore{}
	if err := s.Submit(context.Background(), humanIdeaSubmission()); err != nil || len(ensured) != 1 {
		t.Errorf("after the baseline is declared: got = %v, built besides on %v, want only the job's environment", err, ensured)
	}

	s.store, s.environments = admittingStore{}, declared{}
	none := humanIdeaSubmission()
	if err := s.Submit(context.Background(), none); err != nil || none.EnvironmentKey != "" {
		t.Errorf("a commit with no environment file: got = %v and environment %q, want the job taken with none", err, none.EnvironmentKey)
	}
}

func TestASubmitIsRefusedAnEnvironmentFileNotBuiltOnBaseOrUnreadable(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for name, c := range map[string]struct {
		environments declared
		reason       string
	}{
		"its own base":        {declared{files: map[string]string{commit: "FROM python:3.12\nRUN true\n"}}, domain.ReasonInvalidEnvironmentFile},
		"BASE with a default": {declared{files: map[string]string{commit: "ARG BASE=python\nFROM ${BASE}\n"}}, domain.ReasonInvalidEnvironmentFile},
		"unreadable":          {declared{err: api.Refuse(http.StatusServiceUnavailable, "unavailable", "GitHub is rate limiting the platform")}, ReasonEnvironmentUnreadable},
	} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: admittingStore{}, hypotheses: humanHypothesis("pe-1"), novelty: novel{},
			quota: insertingQuota{}, loop: wokenLoop{new(int)}, environments: c.environments}
		var refused *AdmissionError
		if err := s.Submit(context.Background(), humanIdeaSubmission()); !errors.As(err, &refused) || refused.Reason != c.reason || !strings.Contains(refused.Message, domain.EnvironmentFile) {
			t.Errorf("%s: got = %v, want %s naming the file", name, err, c.reason)
		}
	}
}

// environmentStore holds the one environment the queued jobs declare and records what the loop
// wrote of them.
type environmentStore struct {
	LoopStore
	env        *domain.Environment
	reasons    map[string]string
	terminated map[string]string
}

func (s *environmentStore) GetEnvironment(_ context.Context, key string) (*domain.Environment, error) {
	if s.env.Key != key {
		return nil, nil
	}
	return s.env, nil
}

func (s *environmentStore) UpdateNotAdmittedReason(_ context.Context, id, reason string) error {
	s.reasons[id] = reason
	return nil
}

func (s *environmentStore) ResolveTermination(_ context.Context, id string, from, to domain.ExperimentStatus, reason string) (domain.Termination, error) {
	if from != domain.StatusQueued {
		return domain.TerminationSkipped, errors.New("only a queued job is ended for its environment")
	}
	s.terminated[id] = string(to) + " " + reason
	return domain.TerminationWritten, nil
}

// A job waits, saying so, while its environment builds, ends with the end of the build's output
// once the file failed, ends as the platform's fault once the platform abandoned it, and goes on
// to placement once it is ready.
func TestAJobWaitsForItsEnvironmentAndEndsWithTheBuildOutputWhenItFails(t *testing.T) {
	for state, want := range map[domain.EnvironmentState]string{
		domain.EnvironmentBuilding:  "held",
		domain.EnvironmentFailed:    "ended",
		domain.EnvironmentAbandoned: "platform",
		domain.EnvironmentReady:     "placed",
	} {
		env := &domain.Environment{Key: "k1", Base: "registry.test/job:1", State: state, LogTail: "E: Unable to locate package rustc"}
		store := &environmentStore{env: env, reasons: map[string]string{}, terminated: map[string]string{}}
		settler := &recordingSettler{}
		l := NewLoop(store, nil, nil, testGraceLimits, zap.NewNop()).WithQuotaSettler(settler)
		exp := &domain.Experiment{ID: "job-1", Status: domain.StatusQueued, EnvironmentKey: "k1"}
		held, err := l.heldForEnvironment(context.Background(), map[string]*domain.Environment{}, exp)
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		switch want {
		case "held":
			if !held || !strings.HasPrefix(store.reasons["job-1"], domain.NotAdmittedEnvironmentBuilding+": ") || len(store.terminated) > 0 {
				t.Errorf("building: held %v, reason %q, want held with environment_building", held, store.reasons["job-1"])
			}
		case "ended":
			reason := store.terminated["job-1"]
			if !held || !strings.HasPrefix(reason, "REJECTED "+string(domain.EvictionEnvironmentFailed)+": ") || !strings.HasSuffix(reason, env.LogTail) || len(settler.settled) != 1 {
				t.Errorf("failed: held %v, ended with %q, settled %v, want it rejected with the build's output and settled", held, reason, settler.settled)
			}
		case "platform":
			reason := store.terminated["job-1"]
			if !held || !strings.HasPrefix(reason, "EVICTED "+string(domain.EvictionCodeUnreachable)+": ") || strings.Contains(reason, env.LogTail) ||
				!domain.IsInfrastructureFault(domain.EvictionReason(strings.TrimPrefix(reason, "EVICTED "))) || len(settler.settled) != 1 {
				t.Errorf("abandoned: held %v, ended with %q, settled %v, want it evicted as the platform's fault and settled", held, reason, settler.settled)
			}
		case "placed":
			if held || len(store.reasons)+len(store.terminated) > 0 {
				t.Errorf("ready: held %v, want it left to placement", held)
			}
		}
	}
}
