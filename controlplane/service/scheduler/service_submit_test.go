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

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore/minio"
)

// listingBytes serves one ListObjectsV2 page holding a single object of the given size — enough
// for the ceiling arithmetic, which only ever asks the store for a total.
func listingBytes(t *testing.T, size int64) *objectstore.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>pe-1/agent/exp/ckpt</Key><Size>%d</Size></Contents></ListBucketResult>`, size)
	}))
	t.Cleanup(srv.Close)
	client, err := objectstore.New(srv.URL, srv.URL, "us-east-1", "hl-data", "key", "secret", func(c *objectstore.Client) objectstore.KeyIssuer { return minio.New(c, c.SecretAccessKey) })
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
	client, err := objectstore.New(srv.URL, srv.URL, "us-east-1", "hl-data", "key", "secret", func(c *objectstore.Client) objectstore.KeyIssuer { return minio.New(c, c.SecretAccessKey) })
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
	return &domain.PlatformExperiment{ID: id, Status: domain.PlatformExpRunning, Repository: "repo.git", RepositoryURL: "https://example.com/repo.git", Image: testExperimentImage}, nil
}
func (poolOnlyStore) GetAgentSignup(_ context.Context, _, agentID string) (*domain.AgentSignup, error) {
	return &domain.AgentSignup{AgentID: agentID, Role: domain.SignupRoleExecutor, AgentType: domain.DefaultAgentType}, nil
}
func (poolOnlyStore) IsAgentCut(_ context.Context, _, _ string) (bool, error) { return false, nil }
func (poolOnlyStore) IsAgentFinished(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (poolOnlyStore) ProjectOrg(context.Context, string) (string, error) { return "org-a", nil }
func (poolOnlyStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return map[string]domain.Cluster{"k8s": {Name: "k8s", OrgID: "org-a"}}, nil
}
func (poolOnlyStore) OrgComputeLimits(context.Context) (map[string]domain.ComputeLimit, error) {
	return nil, nil
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
		CodeRef: "https://example.com/repo.git@" + strings.Repeat("a", 40), Branch: "hl/pe-1/executor-1/warmup-helps-abc123",
		Job: domain.JobSpec{
			Command: []string{"python", "train.py"}, AcceleratorType: h100, AcceleratorCount: 1, CPU: "1", Memory: "1Gi", Storage: "1Gi", MaxRetries: &retries,
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
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}

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
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
		exp := humanIdeaSubmission()
		exp.CodeRef = address + sha
		var refused *AdmissionError
		if err := s.Submit(context.Background(), exp); !errors.As(err, &refused) || refused.Reason != ReasonCodeRefNotInRepository ||
			!strings.Contains(refused.Message, "https://example.com/repo.git@<commit-sha>") || strings.Contains(refused.Message, "secret") {
			t.Errorf("%s: got = %v, want %s naming the repository", name, err, ReasonCodeRefNotInRepository)
		}
	}
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: bareStore{}, hypotheses: humanHypothesis("pe-1")}
	var refused *AdmissionError
	if err := s.Submit(context.Background(), humanIdeaSubmission()); !errors.As(err, &refused) || refused.Reason != ReasonCodeRefNotInRepository {
		t.Errorf("an experiment from before every one had a repository: got = %v, want %s", err, ReasonCodeRefNotInRepository)
	}
}

// Quota is debited from, and standings are ranked by, the experiment's own agent_id. Nothing
// about the hypothesis reaches those paths except its text, so a human author cannot acquire a
// budget or a placement by having an agent run its idea — the run is the agent's.
func TestSubmitAgainstAHumanHypothesisLeavesTheRunningAgentAsTheJobsOnlyOwner(t *testing.T) {
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
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
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-other")}

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
	// proposal is the plan the platform experiment was opened on, none for one created whole.
	proposal []byte
}

func (s unstartedStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: s.status, Repository: "repo.git", RepositoryURL: "https://example.com/repo.git", Image: testExperimentImage, Proposal: s.proposal}, nil
}

func (s unstartedStore) GetAgentSignup(context.Context, string, string) (*domain.AgentSignup, error) {
	return s.signup, nil
}

// A draft once took its planner's baseline job. It takes none: nothing runs before a person
// accepted. Open, a platform experiment takes the one job there is to run before the start, the
// baseline's, from an agent that was added to measure it, and tells everyone else to wait.
func TestBeforeTheStartOnlyTheJobThatMeasuresTheBaselineIsAccepted(t *testing.T) {
	executor := &domain.AgentSignup{Role: domain.SignupRoleExecutor, AgentType: domain.DefaultAgentType}
	baseline := &domain.AgentSignup{Role: domain.SignupRoleBaseline, AgentType: domain.DefaultAgentType}
	planner := &domain.AgentSignup{Role: domain.SignupRolePlanner, AgentType: domain.PlannerAgentType}
	caller := auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.KindAgent, AgentID: "agent-1"})
	for name, store := range map[string]unstartedStore{
		"a draft's planner":           {status: domain.PlatformExpDraft, signup: planner},
		"an open one's executor":      {status: domain.PlatformExpOpen, signup: executor},
		"an open one's reviewer":      {status: domain.PlatformExpOpen, signup: &domain.AgentSignup{Role: domain.SignupRoleReviewer}},
		"the planner once it is open": {status: domain.PlatformExpOpen, signup: planner},
		"nobody of the open one":      {status: domain.PlatformExpOpen},
		"a draft's baseline agent":    {status: domain.PlatformExpDraft, signup: baseline},
	} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: store, quota: waitingQuota{}, hypotheses: humanHypothesis("pe-1")}

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
	nobody := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: unstartedStore{status: domain.PlatformExpOpen, signup: executor}, quota: waitingQuota{}, hypotheses: humanHypothesis("pe-1")}
	var admission *AdmissionError
	if err := nobody.Submit(context.Background(), humanIdeaSubmission()); !errors.Is(err, ErrNoCaller) || errors.As(err, &admission) {
		t.Errorf("a submit with no caller in its context: got = %v, want the platform's error and no refusal", err)
	}

	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: unstartedStore{status: domain.PlatformExpOpen, signup: baseline}, hypotheses: oneHypothesis{}}
	if err := s.Submit(context.Background(), baselineSubmission()); !errors.Is(err, errReachedScoring) {
		t.Fatalf("the baseline agent of an open platform experiment: got = %v, want its job taken through every check a running one's is", err)
	}
}

// A person changed the plan's machine while the baseline was measured: a baseline job sized for
// the machine before is refused, saying the plan's, and one on it taken.
func TestABaselineJobIsMeasuredOnThePlansMachine(t *testing.T) {
	baseline := &domain.AgentSignup{Role: domain.SignupRoleBaseline, AgentType: domain.DefaultAgentType}
	plan := []byte(`{"accelerator_type": "` + string(h100) + `", "cpu": "2", "memory": "1Gi", "storage": "1Gi"}`)
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: unstartedStore{status: domain.PlatformExpOpen, signup: baseline, proposal: plan}, hypotheses: oneHypothesis{}}
	var refused *AdmissionError
	if err := s.Submit(context.Background(), baselineSubmission()); !errors.As(err, &refused) || refused.Reason != ReasonNotThePlansMachine || !strings.Contains(refused.Message, "cpu 2") {
		t.Fatalf("a baseline job of 1 core on a plan of 2: got = %v, want %s naming the plan's machine", err, ReasonNotThePlansMachine)
	}
	exp := baselineSubmission()
	exp.Job.CPU = "2"
	if err := s.Submit(context.Background(), exp); !errors.Is(err, errReachedScoring) {
		t.Fatalf("a baseline job on the plan's machine: got = %v, want it taken", err)
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
	baseline := &domain.AgentSignup{Role: domain.SignupRoleBaseline, AgentType: domain.DefaultAgentType}
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpOpen, domain.PlatformExpRunning} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: unstartedStore{status: status, signup: baseline}, hypotheses: oneHypothesis{}}
		exp := baselineSubmission()
		if err := s.Submit(context.Background(), exp); !errors.Is(err, errReachedScoring) {
			t.Fatalf("%s: got = %v, want the job taken", status, err)
		}
		if exp.HypothesisID != "pe-1" || exp.Hypothesis != "" {
			t.Errorf("%s: the job is on %q with text %q, want the objective pe-1 and no text", status, exp.HypothesisID, exp.Hypothesis)
		}

		// It checks its code as an executor would on a branch beside the baseline's.
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
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
		exp := humanIdeaSubmission()
		spoil(exp)
		var refused *AdmissionError
		if err := s.Submit(context.Background(), exp); !errors.As(err, &refused) || refused.Reason != ReasonMalformed {
			t.Errorf("an executor's job with %s: got = %v, want %s", name, err, ReasonMalformed)
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
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: store, hypotheses: oneHypothesis{h: &tc.h}}
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
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: store, hypotheses: oneHypothesis{}}
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
		"cut agent":                  {cutStore{}, "stage_cut"},
		"finished agent":             {finishedStore{}, "agent_finished"},
	} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: tc.store, hypotheses: humanHypothesis("pe-1")}
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
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: poolOnlyStore{}, hypotheses: hypotheses}
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

type twoExperimentsStore struct {
	poolOnlyStore
	asked *[]string
}

func (s twoExperimentsStore) GetRunningAndQueued(_ context.Context, platformExpID string) ([]*domain.Experiment, error) {
	*s.asked = append(*s.asked, platformExpID)
	all := []*domain.Experiment{
		{ID: "mine-1", PlatformExperimentID: "pe-1", HypothesisID: "h-human"}, {ID: "theirs-1", PlatformExperimentID: "pe-other", HypothesisID: "h-human"},
		{ID: "mine-2", PlatformExperimentID: "pe-1", HypothesisID: "h-human"}, {ID: "human-idea-job", PlatformExperimentID: "pe-1", HypothesisID: "h-human"},
	}
	return slices.DeleteFunc(all, func(e *domain.Experiment) bool { return e.PlatformExperimentID != platformExpID }), nil
}

// stopAtPriority ends a submit where priority reads the agent's quota, after novelty is scored.
type stopAtPriority struct{ QuotaService }

func (stopAtPriority) GetAgentQuota(context.Context, string, string) (*domain.AgentQuota, error) {
	return nil, errReachedScoring
}

// Novelty is a comparison within one platform experiment: a hypothesis belongs to one, and
// another's jobs may be another org's. The job never counts against itself.
func TestNoveltyComparesAgainstTheJobsOfTheSamePlatformExperimentOnly(t *testing.T) {
	var asked []string
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: twoExperimentsStore{asked: &asked}, hypotheses: humanHypothesis("pe-1"), quota: stopAtPriority{}}
	exp := humanIdeaSubmission()
	if err := s.Submit(context.Background(), exp); !errors.Is(err, errReachedScoring) {
		t.Fatalf("Submit: got = %v, want it to reach priority scoring", err)
	}
	if !slices.Equal(asked, []string{"pe-1"}) || exp.NoveltyScore != 1.0/3 {
		t.Fatalf("asked for the jobs of %v, novelty %v, want pe-1's alone and 1/3 for mine-1 and mine-2", asked, exp.NoveltyScore)
	}
}

// Everything after Submit's first line reads exp.Job: the store that writes job_spec, the answer
// of POST /experiments, the events of the job. The secret has to be off it by then.
func TestSubmitTakesSecretEnvOffTheSpec(t *testing.T) {
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
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
	if err := s.Submit(context.Background(), clash); !errors.As(err, &refused) || refused.Reason != ReasonMalformed || AdmissionStatus(refused.Reason) != 422 {
		t.Fatalf("a name in both env and secret_env: got = %v, want a malformed submission answered 422", err)
	}
	if strings.Contains(refused.Message, "the-secret-value") {
		t.Fatalf("the refusal repeats the secret: %s", refused.Message)
	}
}

type clustersStore struct {
	poolOnlyStore
	registered map[string]domain.Cluster
	limits     map[string]domain.ComputeLimit
}

func (s clustersStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return s.registered, nil
}
func (s clustersStore) OrgComputeLimits(context.Context) (map[string]domain.ComputeLimit, error) {
	return s.limits, nil
}

type installedCapacity struct {
	installed map[string]domain.Footprint
	scales    map[string]bool
	nodes     map[string]map[string]map[string]int64
	labels    map[string]map[string]map[string]string
}

func (c installedCapacity) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	return c.labels, nil
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
		exp.Job = domain.JobSpec{Command: []string{"python", "train.py"}, CPU: "8", Memory: "16Gi", Storage: storage, AcceleratorType: chip, AcceleratorCount: chips, MaxRetries: &retries}
		s := &Service{credits: testCaps, codeHost: testCodeHost{}, store: clustersStore{registered: registered}, clusters: capacity, hypotheses: humanHypothesis("pe-1")}
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
	for _, said := range []string{"3.9 TiB storage", "8 cores", "16 GiB memory", "box-b has 1 x " + chip, "3.6 TiB storage"} {
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

// A job whose image runs on an architecture no node of the clusters that run it has queued for
// good. It is refused at submit naming the image's architecture and the ones the org has. A
// cluster that scales up may yet add a node of it.
func TestAJobOfAnArchitectureNoClusterHasIsRefusedAtSubmit(t *testing.T) {
	registered := map[string]domain.Cluster{"box-b": {Name: "box-b", OrgID: "org-a"}, "theirs": {Name: "theirs", OrgID: "org-b"}}
	installed := map[string]domain.Footprint{
		"box-b":  domain.CapacityFootprint(32, map[string]int64{h100: 8}, 1<<40, 1<<42),
		"theirs": domain.CapacityFootprint(32, map[string]int64{h100: 8}, 1<<40, 1<<42),
	}
	nodesOf := func(arch string) map[string]map[string]map[string]string {
		return map[string]map[string]map[string]string{
			"box-b":  {"box": {domain.NodeArchitectureLabel: arch}},
			"theirs": {"node": {domain.NodeArchitectureLabel: testExperimentImage.Architecture}},
		}
	}
	submit := func(capacity installedCapacity) (*AdmissionError, error) {
		s := &Service{credits: testCaps, codeHost: testCodeHost{}, store: clustersStore{registered: registered}, clusters: capacity, hypotheses: humanHypothesis("pe-1")}
		var refused *AdmissionError
		err := s.Submit(context.Background(), humanIdeaSubmission())
		errors.As(err, &refused)
		return refused, err
	}

	accepted := humanIdeaSubmission()
	s := &Service{credits: testCaps, codeHost: testCodeHost{}, store: clustersStore{registered: registered}, clusters: installedCapacity{installed: installed, labels: nodesOf("amd64")}, hypotheses: humanHypothesis("pe-1")}
	if err := s.Submit(context.Background(), accepted); !errors.Is(err, errReachedScoring) {
		t.Fatalf("an amd64 image on amd64 nodes: got = %v, want it accepted", err)
	}
	if accepted.ImageArchitecture != "amd64" || len(accepted.Job.NodeSelector) != 0 {
		t.Errorf("accepted on %q with node selector %v, want amd64 kept beside the image and the selector as submitted", accepted.ImageArchitecture, accepted.Job.NodeSelector)
	}
	refused, err := submit(installedCapacity{installed: installed, labels: nodesOf("arm64")})
	if refused == nil || refused.Reason != domain.NotAdmittedNoClusterForArchitecture || AdmissionStatus(refused.Reason) != http.StatusUnprocessableEntity {
		t.Fatalf("an amd64 image on arm64 nodes: got = %v, want a 422 %s", err, domain.NotAdmittedNoClusterForArchitecture)
	}
	for _, said := range []string{"runs on amd64", "nodes of arm64 only", "box-b"} {
		if !strings.Contains(refused.Message, said) {
			t.Errorf("the refusal does not say %q: %s", said, refused.Message)
		}
	}
	if strings.Contains(refused.Message, "theirs") {
		t.Errorf("the refusal names another org's cluster: %s", refused.Message)
	}
	if _, err := submit(installedCapacity{installed: installed, labels: nodesOf("arm64"), scales: map[string]bool{"box-b": true}}); !errors.Is(err, errReachedScoring) {
		t.Errorf("a cluster that scales up: got = %v, want the job queued", err)
	}
}

// A rented cluster names on its slots every architecture its provider rents, an offerless slot
// too, so a job of an architecture it has no machine of right now is accepted to wait for one.
func TestAJobOfAnArchitectureARentedClusterCanProvideIsAccepted(t *testing.T) {
	registered := map[string]domain.Cluster{"rented": {Name: "rented", OrgID: "org-a"}}
	installed := map[string]domain.Footprint{"rented": domain.CapacityFootprint(32, map[string]int64{h100: 8}, 1<<40, 1<<42)}
	slots := map[string]map[string]map[string]string{"rented": {
		"h100-0": {domain.NodeSlotLabel: "h100-0", domain.NodeArchitectureLabel: "arm64"},
		"h100-1": {domain.NodeSlotLabel: "h100-1", domain.NodeArchitectureLabel: testExperimentImage.Architecture, domain.NodeSlotNoOfferLabel: "shape h100 on amd64: no offer passes the filters"},
	}}
	s := &Service{credits: testCaps, codeHost: testCodeHost{}, store: clustersStore{registered: registered}, clusters: installedCapacity{installed: installed, labels: slots}, hypotheses: humanHypothesis("pe-1")}
	if err := s.Submit(context.Background(), humanIdeaSubmission()); !errors.Is(err, errReachedScoring) {
		t.Fatalf("an amd64 image on a cluster whose amd64 slot waits for an offer: got = %v, want it accepted", err)
	}
}

type endingStore struct {
	poolOnlyStore
	status domain.PlatformExperimentStatus
	endsAt time.Time
}

func (s endingStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: s.status, EndsAt: s.endsAt, Repository: "repo.git", RepositoryURL: "https://example.com/repo.git", Image: testExperimentImage}, nil
}

func (endingStore) GetAgentSignup(_ context.Context, _, agentID string) (*domain.AgentSignup, error) {
	return &domain.AgentSignup{AgentID: agentID, Role: domain.SignupRoleBaseline, AgentType: domain.DefaultAgentType}, nil
}

// The platform closes a platform experiment some seconds after its end and evicts what runs
// then. A job sent in between is refused as one sent after the close is.
func TestSubmitPastTheEndOfThePlatformExperimentIsRefused(t *testing.T) {
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpRunning, domain.PlatformExpOpen} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: endingStore{status: status, endsAt: time.Now().Add(-10 * time.Second)}, hypotheses: oneHypothesis{}}
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
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: &summarisedJobStore{}, hypotheses: oneHypothesis{h: &domain.Hypothesis{ID: "hyp-1", AgentID: "agent-1"}}}
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
	for _, branch := range []string{"", "main", "hl/pe-1/", "hl/pe-other/executor-1/warmup-abc123", "hl/pe-1/two words"} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
		exp := humanIdeaSubmission()
		exp.Branch = branch
		var refused *AdmissionError
		if err := s.Submit(context.Background(), exp); !errors.As(err, &refused) || refused.Reason != ReasonBranchNotOfExperiment || !strings.Contains(refused.Message, "hl hypothesis checkout") {
			t.Errorf("branch %q: got = %v, want %s naming hl hypothesis checkout", branch, err, ReasonBranchNotOfExperiment)
		}
	}
}

var testExperimentImage = &domain.ExperimentImage{Image: "registry.test/image@sha256:abababababababababababababababababababababababababababababababab", Architecture: "amd64"}

// A researcher of a running platform experiment steers the work and submits no job, whatever the
// job holds.
func TestAResearcherSubmitsNoJob(t *testing.T) {
	store := unstartedStore{status: domain.PlatformExpRunning, signup: &domain.AgentSignup{Role: domain.SignupRoleResearcher, AgentType: domain.ResearcherAgentType}}
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: store, quota: waitingQuota{}, hypotheses: humanHypothesis("pe-1")}
	caller := auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.KindAgent, AgentID: "agent-1"})
	var admission *AdmissionError
	if err := s.Submit(caller, humanIdeaSubmission()); !errors.As(err, &admission) || admission.Reason != platformapi.ReasonResearcherSubmitsNoJob {
		t.Fatalf("got = %v, want researcher_submits_no_job", err)
	}
}
