package scheduler

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// declared is a repository whose commits declare the environment files held by commit, and none
// otherwise. asked records the org and base each was asked to be built for.
type declared struct {
	files     map[string]string
	err       error
	asked     *[]string
	noBuilder bool
	failed    bool
}

func (d declared) Declared(_ context.Context, orgID, _, _, ref, base string) (*domain.Environment, error) {
	if d.err != nil {
		return nil, d.err
	}
	file, ok := d.files[ref]
	if !ok {
		return nil, nil
	}
	if d.asked != nil {
		*d.asked = append(*d.asked, orgID+" "+base)
	}
	if err := domain.ValidateEnvironmentFile(file); err != nil {
		return nil, err
	}
	if d.noBuilder {
		return nil, domain.ErrNoEnvironmentBuilder
	}
	if d.failed {
		return &domain.Environment{Key: domain.EnvironmentKey(orgID, file, base), Base: base, State: domain.EnvironmentFailed, Fault: domain.EnvironmentFaultFile, LogTail: "E: Unable to locate package rustc"}, nil
	}
	return &domain.Environment{Key: domain.EnvironmentKey(orgID, file, base), OrgID: orgID, Base: base, Dockerfile: file, State: domain.EnvironmentBuilding}, nil
}

const rustEnvironment = "ARG BASE\nFROM ${BASE}\nRUN apt-get install -y rustc\n"

// A job runs in the environment its own commit declares, built for its org on the image it names,
// and a commit that declares none leaves the job as it was.
func TestASubmitKeepsTheEnvironmentItsCommitDeclaresForItsOrgOnItsImage(t *testing.T) {
	commit := strings.Repeat("a", 40)
	var asked []string
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: admittingStore{}, hypotheses: humanHypothesis("pe-1"), novelty: novel{},
		quota: insertingQuota{}, loop: wokenLoop{new(int)}, environments: declared{files: map[string]string{commit: rustEnvironment}, asked: &asked}}
	exp := humanIdeaSubmission()
	if err := s.Submit(context.Background(), exp); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if want := domain.EnvironmentKey("org-a", rustEnvironment, "registry.test/job:1"); exp.EnvironmentKey != want || !slices.Equal(asked, []string{"org-a registry.test/job:1"}) {
		t.Errorf("environment %q asked as %v, want %q for org-a on the job's image", exp.EnvironmentKey, asked, want)
	}

	s.environments = declared{}
	none := humanIdeaSubmission()
	if err := s.Submit(context.Background(), none); err != nil || none.EnvironmentKey != "" {
		t.Errorf("a commit with no environment file: got = %v and environment %q, want the job taken with none", err, none.EnvironmentKey)
	}
}

func TestASubmitIsRefusedAnEnvironmentFileNotBuiltOnBaseUnreadableOrWithNoBuilder(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for name, c := range map[string]struct {
		environments declared
		reason       string
	}{
		"its own base":        {declared{files: map[string]string{commit: "FROM python:3.12\nRUN true\n"}}, domain.ReasonInvalidEnvironmentFile},
		"BASE with a default": {declared{files: map[string]string{commit: "ARG BASE=python\nFROM ${BASE}\n"}}, domain.ReasonInvalidEnvironmentFile},
		"unreadable":          {declared{err: api.Refuse(http.StatusServiceUnavailable, "unavailable", "GitHub is rate limiting the platform")}, ReasonEnvironmentUnreadable},
		"no builder":          {declared{files: map[string]string{commit: rustEnvironment}, noBuilder: true}, domain.ReasonNoEnvironmentBuilder},
		"a build context":     {declared{files: map[string]string{commit: "ARG BASE\nFROM ${BASE}\nCOPY vendor/codec /opt/codec\n"}}, domain.ReasonInvalidEnvironmentFile},
		"the file failed":     {declared{files: map[string]string{commit: rustEnvironment}, failed: true}, string(domain.EvictionEnvironmentFailed)},
	} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: admittingStore{}, hypotheses: humanHypothesis("pe-1"), novelty: novel{},
			quota: insertingQuota{}, loop: wokenLoop{new(int)}, environments: c.environments}
		var refused *AdmissionError
		if err := s.Submit(context.Background(), humanIdeaSubmission()); !errors.As(err, &refused) || refused.Reason != c.reason || !strings.Contains(refused.Message, domain.EnvironmentFile) {
			t.Errorf("%s: got = %v, want %s naming the file", name, err, c.reason)
		}
		if c.environments.failed && !strings.HasSuffix(refused.Message, "E: Unable to locate package rustc") {
			t.Errorf("%s: %q, want the end of the build's output", name, refused.Message)
		}
	}
}

// environmentStore holds the one environment the queued jobs declare, assigns it as the store
// would have, and records what the loop asked and wrote.
type environmentStore struct {
	LoopStore
	env        *domain.Environment
	assigned   [][]string
	reasons    map[string]string
	terminated map[string]string
	settler    *recordingSettler
}

func (s *environmentStore) AssignEnvironment(_ context.Context, key string, live []string, lease time.Duration) (*domain.Environment, error) {
	if s.env.Key != key {
		return nil, nil
	}
	if lease != domain.EnvironmentBuildLease {
		return nil, errors.New("assigned with another lease")
	}
	s.assigned = append(s.assigned, slices.Sorted(slices.Values(live)))
	return s.env, nil
}

func (s *environmentStore) UpdateNotAdmittedReason(_ context.Context, id, reason string) error {
	s.reasons[id] = reason
	return nil
}

func (s *environmentStore) EndQueuedSettled(_ context.Context, id string, to domain.ExperimentStatus, reason string) (bool, error) {
	if !slices.Contains(s.settler.settled, id) {
		return false, errors.New("ended before its usage was settled")
	}
	s.terminated[id] = string(to) + " " + reason
	return true, nil
}

// buildersLive reports the clusters that are live.
type buildersLive struct {
	LoopWorkloadClient
	live  map[string]bool
	reads *int
}

func (w buildersLive) GetLiveClusters(context.Context) (map[string]bool, error) {
	*w.reads++
	return w.live, nil
}

// A job waits, saying where its environment builds, ends with the end of the build's output once
// the file failed, ends at no cost and is not queued again once the platform failed every attempt,
// and goes on to placement once it is ready. Each environment is assigned against the live
// clusters once a tick.
func TestAJobWaitsForItsAssignedEnvironmentAndEndsByWhoseFaultItFailed(t *testing.T) {
	assignedAt := time.Now()
	for name, c := range map[string]struct {
		env  domain.Environment
		want string
	}{
		"assigned":   {domain.Environment{State: domain.EnvironmentBuilding, ClusterName: "builder-a", Attempt: 2, AssignedAt: &assignedAt}, "held"},
		"unassigned": {domain.Environment{State: domain.EnvironmentBuilding}, "waiting"},
		"file":       {domain.Environment{State: domain.EnvironmentFailed, Fault: domain.EnvironmentFaultFile}, "rejected"},
		"platform":   {domain.Environment{State: domain.EnvironmentFailed, Fault: domain.EnvironmentFaultPlatform}, "unbuilt"},
		"ready":      {domain.Environment{State: domain.EnvironmentReady, Image: "registry.test/env@sha256:1"}, "placed"},
	} {
		env := c.env
		env.Key, env.Base, env.LogTail = "k1", "registry.test/job:1", "E: Unable to locate package rustc"
		settler := &recordingSettler{}
		store := &environmentStore{env: &env, reasons: map[string]string{}, terminated: map[string]string{}, settler: settler}
		reads := 0
		workload := buildersLive{live: map[string]bool{"builder-a": true, "gone": false, "builder-b": true}, reads: &reads}
		l := NewLoop(store, nil, workload, testGraceLimits, zap.NewNop()).WithQuotaSettler(settler)
		pass := newEnvironmentPass()
		var held bool
		for _, id := range []string{"job-1", "job-2"} {
			var err error
			if held, err = l.heldForEnvironment(context.Background(), pass, &domain.Experiment{ID: id, Status: domain.StatusQueued, EnvironmentKey: "k1"}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if reads != 1 || len(store.assigned) != 1 || !slices.Equal(store.assigned[0], []string{"builder-a", "builder-b"}) {
			t.Errorf("%s: %d live reads and assignments %v, want one of each against the live clusters", name, reads, store.assigned)
		}
		reason, ended := store.reasons["job-1"], store.terminated["job-1"]
		switch c.want {
		case "held":
			if !held || reason != domain.NotAdmittedEnvironmentBuilding+": the platform builds the environment its commit declares in .hypothesisloop/Dockerfile, on registry.test/job:1, on cluster builder-a, attempt 2 of 3 (the platform builds a file again only after a failure of its own). The job runs in it once it is built" || len(store.terminated) > 0 {
				t.Errorf("%s: held %v, reason %q, want held naming the cluster and the attempt", name, held, reason)
			}
		case "waiting":
			if !held || !strings.HasPrefix(reason, domain.NotAdmittedEnvironmentBuilding+": ") || !strings.Contains(reason, "has room") || len(store.terminated) > 0 {
				t.Errorf("%s: held %v, reason %q, want held waiting for a cluster", name, held, reason)
			}
		case "rejected":
			if !held || !strings.HasPrefix(ended, "REJECTED "+string(domain.EvictionEnvironmentFailed)+": ") || !strings.HasSuffix(ended, env.LogTail) || len(settler.settled) != 2 {
				t.Errorf("%s: held %v, ended with %q, settled %v, want both rejected with the build's output and settled", name, held, ended, settler.settled)
			}
		case "unbuilt":
			code := domain.EvictionReason(strings.TrimPrefix(ended, "EVICTED "))
			if !held || code.Code() != domain.EvictionEnvironmentUnbuilt || strings.Contains(ended, env.LogTail) || !domain.IsInfrastructureFault(code) || domain.RequeuedOnFault(code) || len(settler.settled) != 2 {
				t.Errorf("%s: held %v, ended with %q, settled %v, want both evicted as environment_unbuilt, refunded, not requeued", name, held, ended, settler.settled)
			}
		case "placed":
			if held || len(store.reasons)+len(store.terminated) > 0 {
				t.Errorf("%s: held %v, want it left to placement", name, held)
			}
		}
	}
}

// builtStore is a Store holding environments by key.
type builtStore struct {
	Store
	envs map[string]*domain.Environment
}

func (s builtStore) GetEnvironment(_ context.Context, key string) (*domain.Environment, error) {
	return s.envs[key], nil
}

// Run 3's agent read environment_building for 19 minutes while its build pulled, installed and
// pushed. A job waiting for its environment says the step its build last reported, read with the
// job: a step changes no queue reason and wakes nobody.
func TestAJobWaitingForItsEnvironmentSaysTheStepItsBuildReported(t *testing.T) {
	at := time.Date(2026, 10, 8, 9, 14, 10, 0, time.UTC)
	svc := &Service{store: builtStore{envs: map[string]*domain.Environment{
		"k1": {Key: "k1", State: domain.EnvironmentBuilding, ClusterName: "builder-a", Attempt: 1, Progress: "[2/3] RUN apt-get install gcc: started", ProgressAt: &at},
		"k2": {Key: "k2", State: domain.EnvironmentBuilding, Attempt: 1, Progress: "stale"},
	}}}
	waiting := func(id, key string) *domain.Experiment {
		return &domain.Experiment{ID: id, Status: domain.StatusQueued, EnvironmentKey: key, NotAdmittedReason: domain.NotAdmittedEnvironmentBuilding, NotAdmittedMessage: "the platform builds it"}
	}
	building, unassigned, placed := waiting("job-1", "k1"), waiting("job-2", "k2"), &domain.Experiment{ID: "job-3", Status: domain.StatusSubmitted, EnvironmentKey: "k1"}
	if err := svc.mergeBuildProgress(context.Background(), []*domain.Experiment{building, unassigned, placed}); err != nil {
		t.Fatal(err)
	}
	if want := "the platform builds it. Its build reported at 2026-10-08T09:14:10Z: [2/3] RUN apt-get install gcc: started"; building.NotAdmittedMessage != want {
		t.Errorf("building: %q, want %q", building.NotAdmittedMessage, want)
	}
	if unassigned.NotAdmittedMessage != "the platform builds it" || placed.NotAdmittedMessage != "" {
		t.Errorf("a build no cluster runs said %q and a placed job %q, want nothing added", unassigned.NotAdmittedMessage, placed.NotAdmittedMessage)
	}
	if err := svc.mergeBuildProgress(context.Background(), []*domain.Experiment{waiting("job-4", "gone")}); err == nil {
		t.Error("a job naming no environment that exists: no error")
	}
}
