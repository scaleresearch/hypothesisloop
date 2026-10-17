package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
)

// listingBytes serves one ListObjectsV2 page holding a single object of the given size — enough
// for the ceiling arithmetic, which only ever asks the store for a total.
func listingBytes(t *testing.T, size int64) *objectstore.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>pe-1/agent/exp/ckpt</Key><Size>%d</Size></Contents></ListBucketResult>`, size)
	}))
	t.Cleanup(srv.Close)
	client, err := objectstore.New("minio", srv.URL, srv.URL, "us-east-1", "hl-data", "key", "secret")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// The ceiling is checked here and nowhere else. A job is never killed mid-write for exceeding it,
// because a job killed as it saves loses the run it was about to preserve — so the refusal has to
// land on the next submission, before anything has been spent, or it never lands at all.
func TestSubmitRefusesAnAgentAtItsDurableDataCeiling(t *testing.T) {
	s := &Service{dataStore: listingBytes(t, 1200), maxDataBytesPerAgent: 1000}
	exp := &domain.Experiment{ID: "over", AgentID: "agent", PlatformExperimentID: "pe-1"}

	err := s.enforceDataCeiling(context.Background(), exp)
	var admission *AdmissionError
	if !errors.As(err, &admission) {
		t.Fatalf("enforceDataCeiling = %v, want an AdmissionError so the submitter is told what to delete", err)
	}
	if admission.Reason != ReasonDataQuotaExceeded {
		t.Fatalf("reason = %q, want %q", admission.Reason, ReasonDataQuotaExceeded)
	}
}

// An agent under its ceiling must be admitted on the measurement alone: the figure comes from the
// store, which is the only thing that knows what is really held, and there is no counter in
// PostgreSQL that could drift from the bytes.
func TestSubmitAdmitsAnAgentUnderItsDurableDataCeiling(t *testing.T) {
	s := &Service{dataStore: listingBytes(t, 999), maxDataBytesPerAgent: 1000}
	exp := &domain.Experiment{ID: "under", AgentID: "agent", PlatformExperimentID: "pe-1"}

	if err := s.enforceDataCeiling(context.Background(), exp); err != nil {
		t.Fatalf("enforceDataCeiling = %v, want nil for an agent holding less than the ceiling", err)
	}
}

// A store that is down is the platform's failure and says so: the handler answers the error 503
// object_store_unavailable through auth.Internal, where a bare error would be a 500.
func TestSubmitWithTheObjectStoreDownAnswers503NamingIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	client, err := objectstore.New("minio", srv.URL, srv.URL, "us-east-1", "hl-data", "key", "secret")
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{dataStore: client, maxDataBytesPerAgent: 1000}

	err = s.enforceDataCeiling(context.Background(), &domain.Experiment{ID: "job", AgentID: "agent", PlatformExperimentID: "pe-1"})
	var admission *AdmissionError
	if err == nil || errors.As(err, &admission) {
		t.Fatalf("enforceDataCeiling = %v, want a platform failure and no refusal of the job", err)
	}
	refusal := auth.Internal(zap.NewNop(), "submit experiment", err)
	if refusal.GetStatus() != http.StatusServiceUnavailable || refusal.Reason != "object_store_unavailable" {
		t.Fatalf("answered %d %q, want 503 object_store_unavailable", refusal.GetStatus(), refusal.Reason)
	}
}

// errReachedScoring is returned by the first store call Submit makes *after* the hypothesis
// binding. A test getting it back has proved the binding admitted the job, without standing up
// quota, novelty and persistence to watch a row be written.
var errReachedScoring = errors.New("submit reached novelty scoring")

// poolOnlyStore answers just the lookups Submit makes before it reaches the hypothesis binding,
// then stops the pass at scoring. It embeds Store so anything else Submit reaches panics rather
// than silently answering zero.
type poolOnlyStore struct {
	Store
}

func (poolOnlyStore) GetRunningAndQueued(context.Context, string) ([]*domain.Experiment, error) {
	return nil, errReachedScoring
}

// No job completed before: a submit past scoring reads the completed jobs of its hypothesis.
func (poolOnlyStore) ListExperiments(context.Context, domain.ExperimentFilter) ([]*domain.Experiment, error) {
	return nil, nil
}

func (poolOnlyStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: domain.PlatformExpRunning, Repository: "repo.git", RepositoryURL: "https://example.com/repo.git"}, nil
}
func (poolOnlyStore) GetAgentSignup(_ context.Context, _, agentID string) (*domain.AgentSignup, error) {
	return &domain.AgentSignup{AgentID: agentID, Role: domain.SignupRoleCompetitor, AgentType: domain.DefaultAgentFlavor}, nil
}
func (poolOnlyStore) IsAgentCut(_ context.Context, _, _ string) (bool, error) { return false, nil }
func (poolOnlyStore) IsAgentFinished(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (poolOnlyStore) ProjectOrg(context.Context, string) (string, error) { return "org-a", nil }
func (poolOnlyStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return map[string]domain.Cluster{"k8s": {Name: "k8s", OrgID: "org-a"}}, nil
}
func (poolOnlyStore) GetAgent(_ context.Context, id string) (*domain.Agent, error) {
	return &domain.Agent{ID: id}, nil
}

// oneHypothesis is the registry reduced to the single row a submission names.
type oneHypothesis struct {
	h *domain.Hypothesis
}

func (s oneHypothesis) GetHypothesis(_ context.Context, _ string) (*domain.Hypothesis, error) {
	return s.h, nil
}

func (oneHypothesis) Links(context.Context, string) (domain.HypothesisLinks, error) {
	return domain.HypothesisLinks{}, nil
}

// testCodeHost serves each repository under https://example.com/.
type testCodeHost struct{}

func (testCodeHost) Serves(repository, protocol, host, path string) bool {
	return protocol+"://"+host+path == "https://example.com/"+repository
}

func humanIdeaSubmission() *domain.Experiment {
	retries := 0
	return &domain.Experiment{
		ID: "human-idea-job", AgentID: "agent-1", PlatformExperimentID: "pe-1",
		HypothesisID: "h-human", Theory: "t", EstimatedDurationHours: 1, AcceleratorCount: 1,
		CodeRef: "https://example.com/repo.git@" + strings.Repeat("a", 40), Branch: "hl/pe-1/researcher-1/warmup-helps-abc123",
		Job: domain.JobSpec{
			Image: "registry.test/job:1", AcceleratorType: h100, AcceleratorCount: 1, CPU: "1", Memory: "1Gi", Storage: "1Gi", MaxRetries: &retries,
		},
	}
}

// humanHypothesis is the one human-submitted row a submission names, in platformExperimentID.
func humanHypothesis(platformExperimentID string) oneHypothesis {
	return oneHypothesis{h: &domain.Hypothesis{
		ID: "h-human", Actor: domain.Actor{Kind: domain.ActorUser, ID: "user-ada"},
		PlatformExperimentID: platformExperimentID, Text: "warmup helps",
	}}
}

// An idea nobody can test is inert, and the whole point of a human putting one in the pool is
// that the fleet can act on it. Forcing an agent to register its own near-duplicate instead
// fights the pool's dedup and splits one claim's evidence across two rows, so the binding must
// admit a job against a human row exactly as it does against an agent's.
func TestSubmitAdmitsAnAgentsJobAgainstAHumanSubmittedHypothesis(t *testing.T) {
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}

	err := s.Submit(context.Background(), humanIdeaSubmission())
	if !errors.Is(err, errReachedScoring) {
		t.Fatalf("Submit against a human-submitted hypothesis: got = %v, want it to pass the binding and reach scoring", err)
	}
}

type bareStore struct{ poolOnlyStore }

func (bareStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: domain.PlatformExpRunning}, nil
}

// A job runs a commit of its platform experiment's repository. A code_ref that names any other
// address used to be stored as it was, and the job then cloned whatever it named.
func TestSubmitRefusesACodeRefThatIsNotTheExperimentsRepository(t *testing.T) {
	sha := "@" + strings.Repeat("a", 40)
	for name, address := range map[string]string{
		"another repository of the host": "https://example.com/other.git",
		"another host":                   "https://elsewhere.example/repo.git",
		"another scheme":                 "git://example.com/repo.git",
		"a credential in the address":    "https://agent:secret@example.com/repo.git",
		"no address of any host":         "git://hypothesisloop",
	} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
		exp := humanIdeaSubmission()
		exp.CodeRef = address + sha
		var refused *AdmissionError
		if err := s.Submit(context.Background(), exp); !errors.As(err, &refused) || refused.Reason != ReasonCodeRefNotInRepository ||
			!strings.Contains(refused.Message, "https://example.com/repo.git@<commit-sha>") || strings.Contains(refused.Message, "secret") {
			t.Errorf("%s: got = %v, want %s naming the repository", name, err, ReasonCodeRefNotInRepository)
		}
	}
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: bareStore{}, hypotheses: humanHypothesis("pe-1")}
	var refused *AdmissionError
	if err := s.Submit(context.Background(), humanIdeaSubmission()); !errors.As(err, &refused) || refused.Reason != ReasonCodeRefNotInRepository {
		t.Errorf("an experiment from before every one had a repository: got = %v, want %s", err, ReasonCodeRefNotInRepository)
	}
}

// Quota is debited from, and standings are ranked by, the experiment's own agent_id. Nothing
// about the hypothesis reaches those paths except its text, so a human author cannot acquire a
// budget or a placement by having an agent run its idea — the run is the agent's.
func TestSubmitAgainstAHumanHypothesisLeavesTheRunningAgentAsTheJobsOnlyOwner(t *testing.T) {
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
	exp := humanIdeaSubmission()

	if err := s.Submit(context.Background(), exp); !errors.Is(err, errReachedScoring) {
		t.Fatalf("Submit: got = %v, want it to reach scoring", err)
	}
	if exp.AgentID != "agent-1" {
		t.Errorf("job owner: got = %v, want %v", exp.AgentID, "agent-1")
	}
	if exp.Hypothesis != "warmup helps" {
		t.Errorf("denormalized hypothesis text: got = %v, want %v", exp.Hypothesis, "warmup helps")
	}
}

// Dropping the human-row refusal must not loosen the check next to it: a hypothesis still has to
// belong to the job's own platform experiment, or a job could point at another program's idea
// pool and contaminate that program's scope and ranking.
func TestSubmitRefusesAJobAgainstAHumanHypothesisFromAnotherPlatformExperiment(t *testing.T) {
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-other")}

	err := s.Submit(context.Background(), humanIdeaSubmission())
	var admission *AdmissionError
	if !errors.As(err, &admission) {
		t.Fatalf("Submit against a cross-experiment hypothesis: got = %v, want an AdmissionError", err)
	}
	if admission.Reason != ReasonMalformed {
		t.Errorf("reason: got = %v, want %v", admission.Reason, ReasonMalformed)
	}
}

type unstartedStore struct {
	poolOnlyStore
	status domain.PlatformExperimentStatus
	signup *domain.AgentSignup
}

func (s unstartedStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: s.status, Repository: "repo.git", RepositoryURL: "https://example.com/repo.git"}, nil
}

func (s unstartedStore) GetAgentSignup(context.Context, string, string) (*domain.AgentSignup, error) {
	return s.signup, nil
}

// A draft once took its planner's baseline job. It takes none: nothing runs before a person
// accepted. Open, a platform experiment takes the one job there is to run before the start, the
// baseline's, from an agent that was added to measure it, and tells everyone else to wait.
func TestBeforeTheStartOnlyTheJobThatMeasuresTheBaselineIsAccepted(t *testing.T) {
	researcher := &domain.AgentSignup{Role: domain.SignupRoleCompetitor, AgentType: domain.DefaultAgentFlavor}
	baseline := &domain.AgentSignup{Role: domain.SignupRoleBaseline, AgentType: domain.DefaultAgentFlavor}
	planner := &domain.AgentSignup{Role: domain.SignupRolePlanner, AgentType: domain.PlannerFlavor}
	caller := auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.KindAgent, AgentID: "agent-1"})
	for name, store := range map[string]unstartedStore{
		"a draft's planner":           {status: domain.PlatformExpDraft, signup: planner},
		"an open one's researcher":    {status: domain.PlatformExpOpen, signup: researcher},
		"an open one's reviewer":      {status: domain.PlatformExpOpen, signup: &domain.AgentSignup{Role: domain.SignupRoleReviewer}},
		"the planner once it is open": {status: domain.PlatformExpOpen, signup: planner},
		"nobody of the open one":      {status: domain.PlatformExpOpen},
		"a draft's baseline agent":    {status: domain.PlatformExpDraft, signup: baseline},
	} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: store, quota: waitingQuota{}, hypotheses: humanHypothesis("pe-1")}

		// An incomplete spec too: the start is what the agent must wait for, before any field.
		incomplete := humanIdeaSubmission()
		incomplete.Theory = ""
		for _, exp := range []*domain.Experiment{humanIdeaSubmission(), incomplete} {
			var admission *AdmissionError
			if err := s.Submit(caller, exp); !errors.As(err, &admission) || admission.Reason != "experiment_not_started" || !strings.Contains(admission.Message, "has not started") {
				t.Fatalf("%s: got = %v, want a message saying it has not started", name, err)
			}
			// The refusal of an open one is its status_reason as it is worded for the caller.
			if said := strings.Contains(admission.Message, "(baseline_required). step of agent agent-1, then why it waits"); said != (store.status == domain.PlatformExpOpen) {
				t.Errorf("%s: the refusal carries the status_reason worded for its caller = %v: %s", name, said, admission.Message)
			}
		}
	}

	// The refusal is worded for its caller, so a request with none is the platform's failure.
	nobody := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: unstartedStore{status: domain.PlatformExpOpen, signup: researcher}, quota: waitingQuota{}, hypotheses: humanHypothesis("pe-1")}
	var admission *AdmissionError
	if err := nobody.Submit(context.Background(), humanIdeaSubmission()); !errors.Is(err, ErrNoCaller) || errors.As(err, &admission) {
		t.Errorf("a submit with no caller in its context: got = %v, want the platform's error and no refusal", err)
	}

	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: unstartedStore{status: domain.PlatformExpOpen, signup: baseline}, hypotheses: oneHypothesis{}}
	if err := s.Submit(context.Background(), baselineSubmission()); !errors.Is(err, errReachedScoring) {
		t.Fatalf("the baseline agent of an open platform experiment: got = %v, want its job taken through every check a running one's is", err)
	}
}

// baselineSubmission is the job of a baseline agent as it is sent: no hypothesis, no theory, on
// the baseline's branch.
func baselineSubmission() *domain.Experiment {
	exp := humanIdeaSubmission()
	exp.HypothesisID, exp.Theory, exp.Branch = "", "", "hl/pe-1/baseline"
	return exp
}

// The baseline is the objective's own measurement and no claim. Its job states no hypothesis and
// no theory and is kept on the objective, so no hypothesis stays open under it for the run.
func TestTheBaselinesJobTestsNoHypothesisAndIsKeptOnTheObjective(t *testing.T) {
	baseline := &domain.AgentSignup{Role: domain.SignupRoleBaseline, AgentType: domain.DefaultAgentFlavor}
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpOpen, domain.PlatformExpRunning} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: unstartedStore{status: status, signup: baseline}, hypotheses: oneHypothesis{}}
		exp := baselineSubmission()
		if err := s.Submit(context.Background(), exp); !errors.Is(err, errReachedScoring) {
			t.Fatalf("%s: got = %v, want the job taken", status, err)
		}
		if exp.HypothesisID != "pe-1" || exp.Hypothesis != "" {
			t.Errorf("%s: the job is on %q with text %q, want the objective pe-1 and no text", status, exp.HypothesisID, exp.Hypothesis)
		}

		// It checks its code as a researcher would on a branch beside the baseline's.
		check := baselineSubmission()
		check.Branch = "hl/pe-1/baseline-check"
		if err := s.Submit(context.Background(), check); !errors.Is(err, errReachedScoring) {
			t.Errorf("%s, a baseline job on %s: got = %v, want the job taken", status, check.Branch, err)
		}

		var refused *AdmissionError
		named := baselineSubmission()
		named.HypothesisID = "h-human"
		if err := s.Submit(context.Background(), named); !errors.As(err, &refused) || refused.Reason != ReasonBaselineTestsNoHypothesis {
			t.Errorf("%s, a baseline job naming a hypothesis: got = %v, want %s", status, err, ReasonBaselineTestsNoHypothesis)
		}
		// A baseline agent has no hypothesis to check out, on a branch of the experiment or on
		// the default one: the refusal names its branch and the git command that makes it.
		for _, branch := range []string{"hl/pe-1/baseline-1/control-abc123", "hl/pe-1/baseline-", "hl/pe-1/baselinecheck", "main", ""} {
			elsewhere := baselineSubmission()
			elsewhere.Branch = branch
			if err := s.Submit(context.Background(), elsewhere); !errors.As(err, &refused) || refused.Reason != ReasonBranchNotOfExperiment ||
				!strings.Contains(refused.Message, "git checkout -b hl/pe-1/baseline") || strings.Contains(refused.Message, "hl hypothesis checkout") {
				t.Errorf("%s, a baseline job on branch %q: got = %v, want %s naming git checkout -b hl/pe-1/baseline", status, branch, err, ReasonBranchNotOfExperiment)
			}
		}
	}

	// Every other job tests a hypothesis and states what it predicts.
	for name, spoil := range map[string]func(*domain.Experiment){
		"no hypothesis": func(e *domain.Experiment) { e.HypothesisID = "" },
		"no theory":     func(e *domain.Experiment) { e.Theory = "" },
	} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
		exp := humanIdeaSubmission()
		spoil(exp)
		var refused *AdmissionError
		if err := s.Submit(context.Background(), exp); !errors.As(err, &refused) || refused.Reason != ReasonMalformed {
			t.Errorf("a researcher's job with %s: got = %v, want %s", name, err, ReasonMalformed)
		}
	}
}

// waitingQuota words an open platform experiment's status_reason as quota does, for whoever reads.
type waitingQuota struct{ QuotaService }

func (waitingQuota) Describe(_ context.Context, pe *domain.PlatformExperiment, reader auth.Principal) error {
	pe.StatusReason = &domain.StatusReason{Reason: "baseline_required", Message: "step of " + string(reader.Kind) + " " + reader.AgentID + ", then why it waits"}
	return nil
}

type finishedJobStore struct {
	poolOnlyStore
	// onObjective makes the job a baseline agent's, kept on the objective.
	onObjective bool
	filed       int
	// filedOn is what the last finding was filed against.
	filedOn string
	// filedBy is whose credential filed each finding.
	filedBy []domain.Actor
}

func (s *finishedJobStore) GetExperiment(_ context.Context, id string) (*domain.Experiment, error) {
	job := &domain.Experiment{ID: id, AgentID: "agent-1", HypothesisID: "hyp-1", PlatformExperimentID: "pe-1", Status: domain.StatusCompleted}
	if s.onObjective {
		job.HypothesisID = job.PlatformExperimentID
	}
	return job, nil
}

func (s *finishedJobStore) CreateHypothesisFinding(_ context.Context, hypothesisID, _, _, _ string, actor domain.Actor, _ string) (*domain.HypothesisFinding, error) {
	s.filed++
	s.filedOn = hypothesisID
	s.filedBy = append(s.filedBy, actor)
	return &domain.HypothesisFinding{}, nil
}

// The summary is where an agent has the evidence in hand, so its reply names the hypothesis
// whose verdict that agent may set, open or already given: its own or a human's, never another
// agent's. A planner told the verdict of its baseline's hypothesis was its to set tried confirmed
// against the baseline itself, which no rule allows: the job of an agent that measures the
// baseline is the control and names none, before the start and after it.
func TestASummaryNamesTheHypothesisWhoseVerdictIsTheAgentsToSet(t *testing.T) {
	for _, tc := range []struct {
		h    domain.Hypothesis
		want bool
	}{
		{domain.Hypothesis{ID: "hyp-1", AgentID: "agent-1", Status: domain.HypothesisOpen}, true},
		{domain.Hypothesis{ID: "hyp-1", Actor: domain.Actor{Kind: domain.ActorUser, ID: "user-jane"}, Status: domain.HypothesisOpen}, true},
		{domain.Hypothesis{ID: "hyp-1", Actor: domain.Actor{Kind: domain.ActorUser, ID: "user-jane"}, Status: domain.HypothesisConfirmed}, true},
		{domain.Hypothesis{ID: "hyp-1", AgentID: "agent-2", Status: domain.HypothesisOpen}, false},
	} {
		store := &finishedJobStore{}
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: store, hypotheses: oneHypothesis{h: &tc.h}}
		got, err := s.WriteExperimentSummary(context.Background(), "job-1", domain.Actor{Kind: domain.ActorKey, ID: "key-1"}, "it held")
		if len(store.filedBy) != 1 || store.filedBy[0] != (domain.Actor{Kind: domain.ActorKey, ID: "key-1"}) {
			t.Fatalf("%+v: the finding was filed by %+v, want the credential that sent it", tc.h, store.filedBy)
		}
		if err != nil || (got != nil) != tc.want || tc.want && *got != tc.h || store.filed != 1 {
			t.Fatalf("%+v: got = (%+v, %v), filed %d, want it named: %v, filed 1", tc.h, got, err, store.filed, tc.want)
		}
	}

	// The baseline's job is on the objective, which is no hypothesis: its finding is filed there
	// and the reply names none.
	store := &finishedJobStore{onObjective: true}
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: store, hypotheses: oneHypothesis{}}
	if got, err := s.WriteExperimentSummary(context.Background(), "job-1", domain.Actor{Kind: domain.ActorAgent, ID: "agent-1"}, "measured"); err != nil || got != nil || store.filedOn != "pe-1" {
		t.Fatalf("the baseline's summary: got = (%+v, %v), filed on %q, want no hypothesis named and the finding on the objective pe-1", got, err, store.filedOn)
	}
}

type closedStore struct{ poolOnlyStore }

func (closedStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: domain.PlatformExpClosed}, nil
}

type cutStore struct{ poolOnlyStore }

func (cutStore) IsAgentCut(_ context.Context, _, _ string) (bool, error) { return true, nil }

type finishedStore struct{ poolOnlyStore }

func (finishedStore) IsAgentFinished(_ context.Context, _, _ string) (bool, error) { return true, nil }

// An agent's token stays alive for a wind-down after a close or its cut, and after it finished.
// A submission through that window is refused all the same.
func TestSubmitIsRefusedOnceThePlatformExperimentClosedOrTheAgentWasCut(t *testing.T) {
	for name, tc := range map[string]struct {
		store  Store
		reason string
	}{
		"closed platform experiment": {closedStore{}, "experiment_closed"},
		"cut agent":                  {cutStore{}, "agent_held"},
		"finished agent":             {finishedStore{}, "agent_finished"},
	} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: tc.store, hypotheses: humanHypothesis("pe-1")}
		var admission *AdmissionError
		if err := s.Submit(context.Background(), humanIdeaSubmission()); !errors.As(err, &admission) || admission.Reason != tc.reason {
			t.Errorf("%s: got = %v, want a refusal with reason %s", name, err, tc.reason)
		}
	}
}

// A hypothesis of another platform experiment and one that does not exist are one answer, so a
// submission cannot be used to learn which ids exist elsewhere.
func TestSubmitAnswersAHypothesisElsewhereAsItDoesAnAbsentOne(t *testing.T) {
	refusal := func(hypotheses oneHypothesis) *AdmissionError {
		t.Helper()
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: poolOnlyStore{}, hypotheses: hypotheses}
		var admission *AdmissionError
		if err := s.Submit(context.Background(), humanIdeaSubmission()); !errors.As(err, &admission) {
			t.Fatalf("got = %v, want an AdmissionError", err)
		}
		return admission
	}
	elsewhere, absent := refusal(humanHypothesis("pe-other")), refusal(oneHypothesis{})
	if *elsewhere != *absent {
		t.Fatalf("elsewhere: %+v\n   absent: %+v\nwant the same refusal", *elsewhere, *absent)
	}
	if strings.Contains(elsewhere.Message, "pe-other") {
		t.Fatalf("message %q names the other platform experiment", elsewhere.Message)
	}
}

type twoExperimentsStore struct{ poolOnlyStore }

func (twoExperimentsStore) GetRunningAndQueued(_ context.Context, platformExpID string) ([]*domain.Experiment, error) {
	all := []*domain.Experiment{
		{ID: "mine-1", PlatformExperimentID: "pe-1"}, {ID: "theirs-1", PlatformExperimentID: "pe-other"},
		{ID: "mine-2", PlatformExperimentID: "pe-1"}, {ID: "human-idea-job", PlatformExperimentID: "pe-1"},
	}
	return slices.DeleteFunc(all, func(e *domain.Experiment) bool { return e.PlatformExperimentID != platformExpID }), nil
}

type recordingNovelty struct{ compared []string }

// Novelty is a comparison within one platform experiment: a hypothesis belongs to one, and
// another's jobs may be another org's.
func TestNoveltyComparesAgainstTheJobsOfTheSamePlatformExperimentOnly(t *testing.T) {
	novelty := &recordingNovelty{}
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: twoExperimentsStore{}, hypotheses: humanHypothesis("pe-1")}
	if err := s.Submit(context.Background(), humanIdeaSubmission()); !errors.Is(err, errReachedScoring) {
		t.Fatalf("Submit: got = %v, want it to reach novelty scoring", err)
	}
	if want := []string{"mine-1", "mine-2"}; !slices.Equal(novelty.compared, want) {
		t.Fatalf("compared against: got = %v, want = %v", novelty.compared, want)
	}
}

// Everything after Submit's first line reads exp.Job: the store that writes job_spec, the answer
// of POST /experiments, the events of the job. The secret has to be off it by then.
func TestSubmitTakesSecretEnvOffTheSpec(t *testing.T) {
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
	exp := humanIdeaSubmission()
	exp.Job.SecretEnv = map[string]string{"HF_TOKEN": "the-secret-value"}

	if err := s.Submit(context.Background(), exp); !errors.Is(err, errReachedScoring) {
		t.Fatalf("Submit: got = %v, want it to reach scoring", err)
	}
	if exp.Job.SecretEnv != nil || exp.SecretEnv["HF_TOKEN"] != "the-secret-value" {
		t.Fatalf("after Submit: job.secret_env = %v, experiment's = %v, want it moved off the spec", exp.Job.SecretEnv, exp.SecretEnv)
	}
	wire, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "the-secret-value") {
		t.Fatalf("the answer of a submission carries the secret: %s", wire)
	}

	clash := humanIdeaSubmission()
	clash.Job.Env = map[string]string{"HF_TOKEN": "open"}
	clash.Job.SecretEnv = map[string]string{"HF_TOKEN": "the-secret-value"}
	var refused *AdmissionError
	if err := s.Submit(context.Background(), clash); !errors.As(err, &refused) || refused.Reason != ReasonMalformed || AdmissionStatus(refused.Reason) != 400 {
		t.Fatalf("a name in both env and secret_env: got = %v, want a malformed submission answered 400", err)
	}
	if strings.Contains(refused.Message, "the-secret-value") {
		t.Fatalf("the refusal repeats the secret: %s", refused.Message)
	}
}

// A bare name is pulled from whatever registry a runtime defaults to, which is how a job naming
// "python" ran in Docker Hub's image without the platform's tools. A job names one of the
// platform's job images, stored as its address, or states a whole address.
func TestAJobImageIsStoredAsAnAddressAndABareNameIsRefused(t *testing.T) {
	offered := []domain.JobImage{{Name: "python", Image: "registry.test/platform/job:7"}, {Name: "pytorch-cuda", Image: "registry.test/platform/job-pytorch:7"}}
	s := (&Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}).WithJobImages(offered)
	for stated, want := range map[string]string{
		"python":                       "registry.test/platform/job:7",
		"registry.test/platform/job:7": "registry.test/platform/job:7",
		"ghcr.io/someone/mine:1":       "ghcr.io/someone/mine:1",
	} {
		exp := humanIdeaSubmission()
		exp.Job.Image = stated
		if err := s.Submit(context.Background(), exp); !errors.Is(err, errReachedScoring) || exp.Job.Image != want {
			t.Errorf("job.image %q: got = %v with image %q, want it taken as %q", stated, err, exp.Job.Image, want)
		}
	}
	for _, stated := range []string{"python:3.13", "pytorch/pytorch:2.4.0", "tensorflow"} {
		exp := humanIdeaSubmission()
		exp.Job.Image = stated
		var refused *AdmissionError
		if err := s.Submit(context.Background(), exp); !errors.As(err, &refused) || refused.Reason != domain.ReasonUnknownJobImage || AdmissionStatus(refused.Reason) != http.StatusUnprocessableEntity {
			t.Fatalf("job.image %q: got = %v, want a 422 %s", stated, err, domain.ReasonUnknownJobImage)
		}
		if !strings.Contains(refused.Message, "python, pytorch-cuda") {
			t.Errorf("job.image %q: the refusal names no valid image: %s", stated, refused.Message)
		}
	}
}

type clustersStore struct {
	poolOnlyStore
	registered map[string]domain.Cluster
}

func (s clustersStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return s.registered, nil
}

type installedCapacity struct {
	installed map[string]domain.Footprint
	scales    map[string]bool
	nodes     map[string]map[string]map[string]int64
}

func (c installedCapacity) GetNodeTotalCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return c.nodes, nil
}

func (c installedCapacity) GetTotalCapacity(context.Context) (map[string]domain.Footprint, error) {
	return c.installed, nil
}
func (c installedCapacity) GetAutoscalerCapability(context.Context) (map[string]bool, error) {
	return c.scales, nil
}
func (c installedCapacity) GetMultiNodeCapability(context.Context) (map[string]bool, error) {
	return nil, nil
}

// The server queued a job that named no image where a cluster with one job image could run it,
// and only hl refused it. Every job names its image, and the submit is what says so.
func TestAJobThatNamesNoImageIsRefusedAtSubmit(t *testing.T) {
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
	exp := humanIdeaSubmission()
	exp.Job.Image = ""
	var refused *AdmissionError
	if err := s.Submit(context.Background(), exp); !errors.As(err, &refused) || refused.Reason != ReasonImageRequired || AdmissionStatus(refused.Reason) != http.StatusUnprocessableEntity {
		t.Fatalf("a job with no image: got = %v, want a 422 %s", err, ReasonImageRequired)
	}
	if !strings.Contains(refused.Message, "job.image is required") || !strings.Contains(refused.Message, "hl capacity") {
		t.Errorf("the refusal does not name the field and where its values are listed: %s", refused.Message)
	}
}

// A cluster with one job image runs the jobs that name it. Where the cluster that runs a job's
// image has no cores to hold and one with another image has, the job once queued as asking for
// more than any cluster has installed. It is refused at submit with what keeps it out: its image.
func TestAJobItsImageKeepsOffEveryClusterIsRefusedAtSubmit(t *testing.T) {
	domain.SetAcceleratorRates(map[string]float64{h100: 1.0, string(domain.CPUType): 1.0})
	t.Cleanup(func() { domain.SetAcceleratorRates(map[string]float64{h100: 1.0}) })
	const fixed = "registry.test/platform/job-pytorch:7"
	cores := func(n int64) domain.Footprint {
		return domain.CapacityFootprint(float64(n), map[string]int64{string(domain.CPUType): n}, 64<<30, 500<<30)
	}
	registered := map[string]domain.Cluster{
		"rented": {Name: "rented", OrgID: "org-a", JobImage: fixed},
		"metal":  {Name: "metal", OrgID: "org-a"},
		"theirs": {Name: "theirs", OrgID: "org-b"},
	}
	noCores := domain.CapacityFootprint(32, map[string]int64{"nvidia.com/gpu.product=RTX-4090": 4}, 64<<30, 500<<30)
	job := func(image string) *domain.Experiment {
		retries := 0
		exp := humanIdeaSubmission()
		exp.AcceleratorCount = 0
		exp.Job = domain.JobSpec{Image: image, CPU: "1", Memory: "1Gi", Storage: "1Gi", MaxRetries: &retries}
		return exp
	}
	submit := func(installed map[string]domain.Footprint, scales map[string]bool, clusters map[string]domain.Cluster, image string) (*AdmissionError, error) {
		s := &Service{credits: testCaps, codeHost: testCodeHost{}, environments: declared{}, store: clustersStore{registered: clusters}, clusters: installedCapacity{installed: installed, scales: scales}, hypotheses: humanHypothesis("pe-1")}
		var refused *AdmissionError
		err := s.Submit(context.Background(), job(image))
		errors.As(err, &refused)
		return refused, err
	}
	const mine = "registry.test/mine:1"
	installed := map[string]domain.Footprint{"rented": cores(16), "metal": noCores}

	refused, err := submit(installed, nil, registered, mine)
	if refused == nil || refused.Reason != domain.NotAdmittedNoClusterForImage {
		t.Fatalf("an image named, and only the cluster with another one has cores: got = %v, want %s", err, domain.NotAdmittedNoClusterForImage)
	}
	for _, said := range []string{"names image " + mine, "metal has 0 x ", "has it (rented runs every job in " + fixed + ")", "state that image as job.image"} {
		if !strings.Contains(refused.Message, said) {
			t.Errorf("the refusal does not say %q: %s", said, refused.Message)
		}
	}
	if strings.Contains(refused.Message, "theirs") || strings.HasPrefix(refused.Message, domain.NotAdmittedNoClusterForImage) {
		t.Errorf("the refusal names another org's cluster or repeats its reason: %s", refused.Message)
	}

	// A second cluster that runs the job's image and is away holds nothing open: it once let the
	// job through to queue as one asking for more than any cluster has.
	withAway := map[string]domain.Cluster{"away": {Name: "away", OrgID: "org-a"}}
	for name, c := range registered {
		withAway[name] = c
	}
	if refused, err := submit(installed, nil, withAway, mine); refused == nil || refused.Reason != domain.NotAdmittedNoClusterForImage || !strings.Contains(refused.Message, "away reports nothing now") {
		t.Errorf("a cluster that runs the job's image is away: got = %v, want %s naming it", err, domain.NotAdmittedNoClusterForImage)
	}

	if _, err := submit(installed, nil, registered, fixed); !errors.Is(err, errReachedScoring) {
		t.Errorf("the image of the cluster with cores named: got = %v, want it accepted", err)
	}
	for name, c := range map[string]struct {
		installed map[string]domain.Footprint
		scales    map[string]bool
	}{
		"the cluster that runs it has cores":  {map[string]domain.Footprint{"rented": cores(16), "metal": cores(4)}, nil},
		"it scales up":                        {installed, map[string]bool{"metal": true}},
		"its installed capacity was not read": {map[string]domain.Footprint{"rented": cores(16)}, nil},
	} {
		if _, err := submit(c.installed, c.scales, registered, mine); !errors.Is(err, errReachedScoring) {
			t.Errorf("an image named, and %s: got = %v, want it accepted and left to the queue", name, err)
		}
	}
	// Where no image would help, the job is refused for its size.
	for name, installed := range map[string]map[string]domain.Footprint{
		"no cluster has cores, whatever the image":  {"rented": noCores, "metal": noCores},
		"the other cluster's capacity was not read": {"metal": noCores},
	} {
		if refused, err := submit(installed, nil, registered, mine); refused == nil || refused.Reason != domain.NotAdmittedExceedsClusterCapacity {
			t.Errorf("an image named, and %s: got = %v, want %s", name, err, domain.NotAdmittedExceedsClusterCapacity)
		}
	}

	onlyRented := map[string]domain.Cluster{"rented": registered["rented"], "theirs": registered["theirs"]}
	if refused, err := submit(nil, nil, onlyRented, mine); refused == nil || refused.Reason != domain.NotAdmittedNoClusterForImage || !strings.Contains(refused.Message, "state that image as job.image") {
		t.Errorf("an image named, and the org's one cluster runs another: got = %v, want %s", err, domain.NotAdmittedNoClusterForImage)
	}
	if _, err := submit(nil, nil, map[string]domain.Cluster{"theirs": registered["theirs"]}, mine); !errors.Is(err, errReachedScoring) {
		t.Errorf("an org without a cluster: got = %v, want the job queued, where it reads no_cluster_for_org", err)
	}
}

// A job that asks for more than the one cluster that runs it has with nothing on it queued for
// good: a chip job given 3.6 TiB of storage on a node whose disk is that size. It is refused at
// submit with what it asks for and what the cluster has. A cluster that scales up, or one whose
// capacity was not read, may yet hold it.
func TestAJobNoIdleClusterHoldsIsRefusedAtSubmit(t *testing.T) {
	const chip = "tenstorrent.com/chipArch=blackhole"
	domain.SetAcceleratorRates(map[string]float64{h100: 1.0, chip: 0.5})
	t.Cleanup(func() { domain.SetAcceleratorRates(map[string]float64{h100: 1.0}) })
	registered := map[string]domain.Cluster{"box-b": {Name: "box-b", OrgID: "org-a"}, "theirs": {Name: "theirs", OrgID: "org-b"}}
	const disk = 3935709487104
	installed := map[string]domain.Footprint{
		"box-b":  domain.CapacityFootprint(32, map[string]int64{chip: 1}, 132604628992, disk),
		"theirs": domain.CapacityFootprint(512, map[string]int64{chip: 64}, 1<<42, 1<<50),
	}
	oneNode := map[string]map[string]map[string]int64{"box-b": {"box": {
		domain.NodeResourceCPUMillicores: 32000, domain.NodeResourceMemoryBytes: 132604628992, domain.NodeResourceStorageBytes: disk,
	}}}
	submit := func(capacity installedCapacity, storage string, chips int) (*AdmissionError, error) {
		retries := 0
		exp := humanIdeaSubmission()
		exp.AcceleratorType, exp.AcceleratorCount = chip, chips
		exp.Job = domain.JobSpec{Image: "registry.test/mine:1", CPU: "8", Memory: "16Gi", Storage: storage, AcceleratorType: chip, AcceleratorCount: chips, MaxRetries: &retries}
		s := &Service{credits: testCaps, codeHost: testCodeHost{}, environments: declared{}, store: clustersStore{registered: registered}, clusters: capacity, hypotheses: humanHypothesis("pe-1")}
		var refused *AdmissionError
		err := s.Submit(context.Background(), exp)
		errors.As(err, &refused)
		return refused, err
	}

	if _, err := submit(installedCapacity{installed: installed, nodes: oneNode}, "4Gi", 1); !errors.Is(err, errReachedScoring) {
		t.Fatalf("a job the idle cluster holds: got = %v, want it accepted", err)
	}
	refused, err := submit(installedCapacity{installed: installed, nodes: oneNode}, "4000Gi", 1)
	if refused == nil || refused.Reason != domain.NotAdmittedExceedsClusterCapacity || AdmissionStatus(refused.Reason) != http.StatusUnprocessableEntity {
		t.Fatalf("more storage than the node's disk: got = %v, want a 422 %s", err, domain.NotAdmittedExceedsClusterCapacity)
	}
	for _, said := range []string{"3.9 TiB storage", "8 cores", "16 GiB memory", "box-b has 1 x " + strings.ToLower(chip), "3.6 TiB storage"} {
		if !strings.Contains(refused.Message, said) {
			t.Errorf("the refusal does not say %q: %s", said, refused.Message)
		}
	}
	if strings.Contains(refused.Message, "theirs") {
		t.Errorf("the refusal names another org's cluster: %s", refused.Message)
	}
	if refused, err := submit(installedCapacity{installed: installed, nodes: oneNode}, "4Gi", 2); refused == nil || refused.Reason != domain.NotAdmittedExceedsClusterCapacity {
		t.Errorf("two chips where one is installed: got = %v, want %s", err, domain.NotAdmittedExceedsClusterCapacity)
	}

	// The total covers the job and no node does.
	twoNodes := map[string]map[string]map[string]int64{"box-b": {
		"a": {domain.NodeResourceCPUMillicores: 16000, domain.NodeResourceMemoryBytes: 8 << 30, domain.NodeResourceStorageBytes: disk},
		"b": {domain.NodeResourceCPUMillicores: 16000, domain.NodeResourceMemoryBytes: 8 << 30, domain.NodeResourceStorageBytes: disk},
	}}
	if refused, err := submit(installedCapacity{installed: installed, nodes: twoNodes}, "4Gi", 1); refused == nil || !strings.Contains(refused.Message, "at most 16 cores, 8 GiB memory, 3.6 TiB storage on one node") {
		t.Errorf("16Gi where each node has 8: got = %v, want a refusal saying the most one node has", err)
	}

	for name, capacity := range map[string]installedCapacity{
		"scales up":                    {installed: installed, nodes: oneNode, scales: map[string]bool{"box-b": true}},
		"has not reported what it has": {installed: map[string]domain.Footprint{}},
	} {
		if _, err := submit(capacity, "4000Gi", 1); !errors.Is(err, errReachedScoring) {
			t.Errorf("a cluster that %s: got = %v, want the job queued", name, err)
		}
	}
}

type endingStore struct {
	poolOnlyStore
	status domain.PlatformExperimentStatus
	endsAt time.Time
}

func (s endingStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: s.status, EndsAt: s.endsAt, Repository: "repo.git", RepositoryURL: "https://example.com/repo.git"}, nil
}

func (endingStore) GetAgentSignup(_ context.Context, _, agentID string) (*domain.AgentSignup, error) {
	return &domain.AgentSignup{AgentID: agentID, Role: domain.SignupRoleBaseline, AgentType: domain.DefaultAgentFlavor}, nil
}

// The platform closes a platform experiment some seconds after its end and evicts what runs
// then. A job sent in between is refused as one sent after the close is.
func TestSubmitPastTheEndOfThePlatformExperimentIsRefused(t *testing.T) {
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpRunning, domain.PlatformExpOpen} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: endingStore{status: status, endsAt: time.Now().Add(-10 * time.Second)}, hypotheses: oneHypothesis{}}
		var refused *AdmissionError
		if err := s.Submit(context.Background(), baselineSubmission()); !errors.As(err, &refused) || refused.Reason != "experiment_closed" || !strings.Contains(refused.Message, "ended at") {
			t.Errorf("%s, 10 s past its end: got = %v, want experiment_closed naming the end", status, err)
		}
		s.store = endingStore{status: status, endsAt: time.Now().Add(time.Hour)}
		if err := s.Submit(context.Background(), baselineSubmission()); !errors.Is(err, errReachedScoring) {
			t.Errorf("%s, an hour before its end: got = %v, want the job taken through every check", status, err)
		}
	}
}

type summarisedJobStore struct{ finishedJobStore }

func (*summarisedJobStore) CreateHypothesisFinding(_ context.Context, _, _, experimentID, _ string, _ domain.Actor, _ string) (*domain.HypothesisFinding, error) {
	return nil, fmt.Errorf("hypotheses_store.CreateHypothesisFinding: %w: %s", db.ErrFindingExists, experimentID)
}

// A job has one summary. A second one is the caller's to stop sending, so it is a 409 with a
// reason to branch on and none of the store's text.
func TestASecondSummaryOnAJobIsRefusedAsAConflict(t *testing.T) {
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: &summarisedJobStore{}, hypotheses: oneHypothesis{h: &domain.Hypothesis{ID: "hyp-1", AgentID: "agent-1"}}}
	_, err := s.WriteExperimentSummary(context.Background(), "job-1", domain.Actor{Kind: domain.ActorAgent, ID: "agent-1"}, "again")
	refusal := ChangeRefusal(err)
	if refusal == nil || refusal.GetStatus() != http.StatusConflict {
		t.Fatalf("a second summary: got = %v (%v), want a 409", refusal, err)
	}
	body, _ := json.Marshal(refusal)
	var answer struct{ Reason, Message string }
	if json.Unmarshal(body, &answer) != nil || answer.Reason != ReasonSummaryExists || answer.Message == "" || strings.Contains(string(body), "hypotheses_store") {
		t.Fatalf("a second summary: body = %s, want reason %s, a message and no store text", body, ReasonSummaryExists)
	}
}

// A job says which branch its commit was pushed on, and that branch is one of its platform
// experiment's: a commit of the default branch, or of another experiment's branch, is refused.
func TestSubmitRefusesABranchThatIsNotThePlatformExperiments(t *testing.T) {
	for _, branch := range []string{"", "main", "hl/pe-1/", "hl/pe-other/researcher-1/warmup-abc123", "hl/pe-1/two words"} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
		exp := humanIdeaSubmission()
		exp.Branch = branch
		var refused *AdmissionError
		if err := s.Submit(context.Background(), exp); !errors.As(err, &refused) || refused.Reason != ReasonBranchNotOfExperiment || !strings.Contains(refused.Message, "hl hypothesis checkout") {
			t.Errorf("branch %q: got = %v, want %s naming hl hypothesis checkout", branch, err, ReasonBranchNotOfExperiment)
		}
	}
}
