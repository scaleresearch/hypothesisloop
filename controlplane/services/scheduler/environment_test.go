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
	// known are the environments asked for, by key, which Find reads.
	known map[string]*domain.Environment
}

func (d declared) Find(_ context.Context, orgID, _, _, ref, base string) (*domain.Environment, bool, error) {
	if d.err != nil {
		return nil, false, d.err
	}
	file, ok := d.files[ref]
	if !ok {
		return nil, false, nil
	}
	if err := domain.ValidateEnvironmentFile(file); err != nil {
		return nil, true, err
	}
	return d.known[domain.EnvironmentKey(orgID, "", file, base, "")], true, nil
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
	return &domain.Environment{Key: domain.EnvironmentKey(orgID, "", file, base, ""), OrgID: orgID, Base: base, Dockerfile: file, State: domain.EnvironmentBuilding}, nil
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
	if want := domain.EnvironmentKey("org-a", "", rustEnvironment, "registry.test/job:1", ""); exp.EnvironmentKey != want || !slices.Equal(asked, []string{"org-a registry.test/job:1"}) {
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
	} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: admittingStore{}, hypotheses: humanHypothesis("pe-1"), novelty: novel{},
			quota: insertingQuota{}, loop: wokenLoop{new(int)}, environments: c.environments}
		var refused *AdmissionError
		if err := s.Submit(context.Background(), humanIdeaSubmission()); !errors.As(err, &refused) || refused.Reason != c.reason || !strings.Contains(refused.Message, domain.EnvironmentFile) {
			t.Errorf("%s: got = %v, want %s naming the file", name, err, c.reason)
		}
	}
}

// environmentStore holds the one environment the queued jobs declare, assigns it as the store
// would have, and records what the loop asked and wrote.
type environmentStore struct {
	LoopStore
	env        *domain.Environment
	assigned   [][]string
	holding    [][]string
	reasons    map[string]string
	terminated map[string]string
	settler    *recordingSettler
}

func (s *environmentStore) AssignEnvironment(_ context.Context, key string, live, holding []string, lease time.Duration) (*domain.Environment, error) {
	if s.env.Key != key {
		return nil, nil
	}
	if lease != domain.EnvironmentBuildLease {
		return nil, errors.New("assigned with another lease")
	}
	s.assigned = append(s.assigned, slices.Sorted(slices.Values(live)))
	s.holding = append(s.holding, slices.Sorted(slices.Values(holding)))
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

// A job waits with a queue reason that changes with none of its build's attempts, ends with the end of the build's output once
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
		"unassigned": {domain.Environment{State: domain.EnvironmentBuilding}, "held"},
		"again":      {domain.Environment{State: domain.EnvironmentBuilding, ClusterName: "builder-a", Attempt: 1, AssignedAt: &assignedAt, FailedLog: "curl: (6) Could not resolve host"}, "again"},
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
			if held, err = l.heldForEnvironment(context.Background(), pass, &domain.Experiment{ID: id, Status: domain.StatusQueued, EnvironmentKey: "k1"}, nil, nil); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if reads != 1 || len(store.assigned) != 1 || !slices.Equal(store.assigned[0], []string{"builder-a", "builder-b"}) ||
			!slices.Equal(store.holding[0], []string{"builder-a", "builder-b", "gone"}) {
			t.Errorf("%s: %d live reads, assignments %v held by %v, want one of each against the live clusters, held by every one heard from", name, reads, store.assigned, store.holding)
		}
		reason, ended := store.reasons["job-1"], store.terminated["job-1"]
		switch c.want {
		case "held":
			if !held || reason != domain.NotAdmittedEnvironmentBuilding+": the platform builds the environment its commit declares in .hypothesisloop/Dockerfile, on registry.test/job:1. The job runs in it once it is built" || len(store.terminated) > 0 {
				t.Errorf("%s: held %v, reason %q, want held naming no cluster, attempt or step", name, held, reason)
			}
		case "again":
			if !held || !strings.Contains(reason, "The job runs in it once it is built. This file failed to build before, and the platform builds it once more") ||
				!strings.HasSuffix(reason, "\ncurl: (6) Could not resolve host") || len(store.terminated) > 0 {
				t.Errorf("%s: held %v, reason %q, want it held saying the file failed before, with that output", name, held, reason)
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

// A build its cluster holds on one node places the job that waits for it there, pinned to that
// node's hostname and run in the image held there, while the push goes on. The job's own spec is
// left as it was submitted.
func TestAJobWhoseEnvironmentIsLoadedOnANodeIsPinnedThereInThatImage(t *testing.T) {
	assignedAt := time.Now()
	loaded := domain.Environment{Key: "k1", Base: "registry.test/job:1", State: domain.EnvironmentBuilding, ClusterName: "builder-a", Attempt: 1, AssignedAt: &assignedAt,
		LoadedImage: "registry.test/env@sha256:1", LoadedNode: "node-1", LoadedCluster: "builder-a"}
	registered := map[string]domain.Cluster{"builder-a": {Name: "builder-a", OrgID: "org-a"}}
	labels := map[string]map[string]map[string]string{"builder-a": {"node-1": {domain.NodeHostnameLabel: "host-1"}, "node-2": {domain.NodeHostnameLabel: "host-2"}}}
	job := func(mutate func(*domain.Experiment)) *domain.Experiment {
		exp := &domain.Experiment{ID: "job-1", OrgID: "org-a", Status: domain.StatusQueued, EnvironmentKey: "k1",
			Job: domain.JobSpec{Image: "registry.test/job:1", NodeSelector: map[string]string{"zone": "a"}}}
		if mutate != nil {
			mutate(exp)
		}
		return exp
	}
	held := func(env domain.Environment, exp *domain.Experiment) (bool, map[string]string) {
		store := &environmentStore{env: &env, reasons: map[string]string{}, terminated: map[string]string{}, settler: &recordingSettler{}}
		l := NewLoop(store, nil, buildersLive{live: map[string]bool{"builder-a": true}, reads: new(int)}, testGraceLimits, zap.NewNop())
		waits, err := l.heldForEnvironment(context.Background(), newEnvironmentPass(), exp, registered, labels)
		if err != nil {
			t.Fatal(err)
		}
		return waits, store.reasons
	}

	submitted := job(nil)
	selector := submitted.Job.NodeSelector
	if waits, reasons := held(loaded, submitted); waits || len(reasons) != 0 {
		t.Fatalf("held %v with %v, want the job left to placement", waits, reasons)
	}
	if submitted.ClusterName != "builder-a" || submitted.EnvironmentImage != loaded.LoadedImage ||
		submitted.Job.NodeSelector[domain.NodeHostnameLabel] != "host-1" || submitted.Job.NodeSelector["zone"] != "a" {
		t.Errorf("pinned to %q in %q with selector %v, want builder-a's node-1 in the loaded image beside its own selector",
			submitted.ClusterName, submitted.EnvironmentImage, submitted.Job.NodeSelector)
	}
	if len(selector) != 1 {
		t.Errorf("the job's own selector became %v", selector)
	}

	pushFailed := loaded
	pushFailed.ClusterName, pushFailed.Attempt = "", 1
	rebuilt := loaded
	rebuilt.ClusterName, rebuilt.Attempt = "builder-b", 2
	for name, env := range map[string]domain.Environment{"its push failed": pushFailed, "built again elsewhere": rebuilt} {
		exp := job(nil)
		if waits, reasons := held(env, exp); waits || len(reasons) != 0 || exp.ClusterName != "builder-a" || exp.Job.NodeSelector[domain.NodeHostnameLabel] != "host-1" {
			t.Errorf("%s: held %v with %v on %q, want it pinned to builder-a's node-1", name, waits, reasons, exp.ClusterName)
		}
	}

	unloaded := loaded
	unloaded.LoadedImage, unloaded.LoadedNode = "", ""
	unlabelled := loaded
	unlabelled.LoadedNode = "node-9"
	for name, c := range map[string]struct {
		env domain.Environment
		exp *domain.Experiment
	}{
		"not loaded":                {unloaded, job(nil)},
		"loaded on a node not seen": {unlabelled, job(nil)},
		"a job on two nodes":        {loaded, job(func(e *domain.Experiment) { e.Job.NumNodes = 2 })},
		"a job pinned elsewhere":    {loaded, job(func(e *domain.Experiment) { e.Job.NodeSelector = map[string]string{domain.NodeHostnameLabel: "host-2"} })},
		"a job of another org":      {loaded, job(func(e *domain.Experiment) { e.OrgID = "org-b" })},
	} {
		waits, reasons := held(c.env, c.exp)
		if !waits || !strings.HasPrefix(reasons["job-1"], domain.NotAdmittedEnvironmentBuilding+": ") || c.exp.ClusterName != "" || c.exp.EnvironmentImage != "" {
			t.Errorf("%s: held %v with %q on %q in %q, want it waiting for the push, unpinned", name, waits, reasons["job-1"], c.exp.ClusterName, c.exp.EnvironmentImage)
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

// An agent read environment_building for 19 minutes while its build pulled, installed and
// pushed, and another was woken for a turn by each new attempt. A job waiting for its environment
// says where the build runs, its attempt and the step it last reported, read with the job.
func TestAJobWaitingForItsEnvironmentSaysWhereItsBuildIs(t *testing.T) {
	at := time.Date(2026, 10, 8, 9, 14, 10, 0, time.UTC)
	svc := &Service{store: builtStore{envs: map[string]*domain.Environment{
		"k1": {Key: "k1", Base: "registry.test/job:1", State: domain.EnvironmentBuilding, ClusterName: "builder-a", Attempt: 2, Progress: "[2/3] RUN apt-get install gcc: started", ProgressAt: &at},
		"k2": {Key: "k2", Base: "registry.test/job:1", State: domain.EnvironmentBuilding, Attempt: 1, Progress: "stale"},
	}}}
	waiting := func(id, key string) *domain.Experiment {
		return &domain.Experiment{ID: id, Status: domain.StatusQueued, EnvironmentKey: key, NotAdmittedReason: domain.NotAdmittedEnvironmentBuilding, NotAdmittedMessage: "the platform builds it"}
	}
	building, unassigned, placed := waiting("job-1", "k1"), waiting("job-2", "k2"), &domain.Experiment{ID: "job-3", Status: domain.StatusSubmitted, EnvironmentKey: "k1"}
	if err := svc.mergeBuildProgress(context.Background(), []*domain.Experiment{building, unassigned, placed}); err != nil {
		t.Fatal(err)
	}
	if want := "the platform builds the environment its commit declares in .hypothesisloop/Dockerfile, on registry.test/job:1. The job runs in it once it is built. " +
		"It builds on cluster builder-a, attempt 2 of 3 (the platform builds a file again only after a failure of its own), and reported at 2026-10-08T09:14:10Z: [2/3] RUN apt-get install gcc: started"; building.NotAdmittedMessage != want {
		t.Errorf("building: %q, want %q", building.NotAdmittedMessage, want)
	}
	if !strings.HasSuffix(unassigned.NotAdmittedMessage, "It waits for a live cluster that builds for its org to have room") || placed.NotAdmittedMessage != "" {
		t.Errorf("a build no cluster runs said %q and a placed job %q, want waiting for a cluster and nothing", unassigned.NotAdmittedMessage, placed.NotAdmittedMessage)
	}
	if err := svc.mergeBuildProgress(context.Background(), []*domain.Experiment{waiting("job-4", "gone")}); err == nil {
		t.Error("a job naming no environment that exists: no error")
	}
}

// A build was loaded on its builder 7.5 minutes before its push to the registry ended. A
// job waiting for it says where it is loaded and that it runs there now, or that it waits for the
// push, and so after a push that failed.
func TestAJobWaitingForALoadedEnvironmentSaysWhereItRunsNow(t *testing.T) {
	loaded := &domain.Environment{Key: "k1", Base: "registry.test/job:1", State: domain.EnvironmentBuilding, ClusterName: "builder-a", Attempt: 1,
		LoadedImage: "registry.test/env@sha256:1", LoadedNode: "node-1", LoadedCluster: "builder-a"}
	if got := buildProgress(loaded); !strings.HasPrefix(got, "It is built and loaded on node node-1 of cluster builder-a, which pushes it to the registry now: "+
		"a job of one node that fits there runs there at once, any other once the push ends. It builds on cluster builder-a, attempt 1 of 3") {
		t.Errorf("loaded: %q", got)
	}
	failed := *loaded
	failed.ClusterName = ""
	if got := buildProgress(&failed); !strings.Contains(got, "of cluster builder-a, whose push to the registry failed: ") ||
		!strings.HasSuffix(got, "It waits for a live cluster that builds for its org to have room") {
		t.Errorf("after a failed push: %q", got)
	}
	answer := answerEnvironment(loaded)
	if answer.State != "loaded" || answer.LoadedCluster != "builder-a" || answer.LoadedNode != "node-1" || answer.Image != "" ||
		!strings.Contains(answer.Message, "loaded on node node-1 of cluster builder-a") {
		t.Errorf("answered %+v, want it loaded on builder-a's node-1", answer)
	}
}
