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

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
)

func TestEstimatedAcceleratorCostIsZeroForCPUOnlyJob(t *testing.T) {
	exp := &domain.Experiment{AcceleratorCount: 0, EstimatedDurationHours: 2}
	if got := estimatedAcceleratorCost(exp); got != 0 {
		t.Fatalf("estimatedAcceleratorCost() = %v, want 0", got)
	}
}

// listingBytes serves one ListObjectsV2 page holding a single object of the given size — enough
// for the ceiling arithmetic, which only ever asks the store for a total.
func listingBytes(t *testing.T, size int64) *objectstore.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>pe-1/agent/exp/ckpt</Key><Size>%d</Size></Contents></ListBucketResult>`, size)
	}))
	t.Cleanup(srv.Close)
	client, err := objectstore.New(srv.URL, "us-east-1", "hl-data", "key", "secret")
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

func (poolOnlyStore) GetRunningAndQueued(_ context.Context) ([]*domain.Experiment, error) {
	return nil, errReachedScoring
}

func (poolOnlyStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: domain.PlatformExpRunning, Repository: "repo.git", RepositoryURL: "https://example.com/repo.git"}, nil
}
func (poolOnlyStore) IsSignedUp(_ context.Context, _, _ string) (bool, error) { return true, nil }
func (poolOnlyStore) IsAgentCut(_ context.Context, _, _ string) (bool, error) { return false, nil }
func (poolOnlyStore) IsAgentFinished(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (poolOnlyStore) HasUnsummarizedCompleted(_ context.Context, _, _ string) (bool, error) {
	return false, nil
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
		CodeRef: "https://example.com/repo.git@" + strings.Repeat("a", 40),
		Job: domain.JobSpec{
			Image: "img", AcceleratorType: h100, AcceleratorCount: 1, CPU: "1", Memory: "1Gi", Storage: "1Gi", MaxRetries: &retries,
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
	s := &Service{codeHost: testCodeHost{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}

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
		s := &Service{codeHost: testCodeHost{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
		exp := humanIdeaSubmission()
		exp.CodeRef = address + sha
		var refused *AdmissionError
		if err := s.Submit(context.Background(), exp); !errors.As(err, &refused) || refused.Reason != ReasonCodeRefNotInRepository ||
			!strings.Contains(refused.Message, "https://example.com/repo.git@<commit-sha>") || strings.Contains(refused.Message, "secret") {
			t.Errorf("%s: got = %v, want %s naming the repository", name, err, ReasonCodeRefNotInRepository)
		}
	}
	s := &Service{codeHost: testCodeHost{}, store: bareStore{}, hypotheses: humanHypothesis("pe-1")}
	var refused *AdmissionError
	if err := s.Submit(context.Background(), humanIdeaSubmission()); !errors.As(err, &refused) || refused.Reason != ReasonCodeRefNotInRepository {
		t.Errorf("an experiment from before every one had a repository: got = %v, want %s", err, ReasonCodeRefNotInRepository)
	}
}

// Quota is debited from, and standings are ranked by, the experiment's own agent_id. Nothing
// about the hypothesis reaches those paths except its text, so a human author cannot acquire a
// budget or a placement by having an agent run its idea — the run is the agent's.
func TestSubmitAgainstAHumanHypothesisLeavesTheRunningAgentAsTheJobsOnlyOwner(t *testing.T) {
	s := &Service{codeHost: testCodeHost{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
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
	s := &Service{codeHost: testCodeHost{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-other")}

	err := s.Submit(context.Background(), humanIdeaSubmission())
	var admission *AdmissionError
	if !errors.As(err, &admission) {
		t.Fatalf("Submit against a cross-experiment hypothesis: got = %v, want an AdmissionError", err)
	}
	if admission.Reason != ReasonMalformed {
		t.Errorf("reason: got = %v, want %v", admission.Reason, ReasonMalformed)
	}
}

type unsummarizedStore struct{ poolOnlyStore }

func (unsummarizedStore) HasUnsummarizedCompleted(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}

// A job that completed during the agent's turn is unknown to it, so the submit is not refused: it
// goes on to be queued, saying at once what holds it.
func TestSubmitWhileASummaryIsOwedIsAcceptedAndSaysWhatHoldsIt(t *testing.T) {
	s := &Service{codeHost: testCodeHost{}, store: unsummarizedStore{}, hypotheses: humanHypothesis("pe-1")}
	exp := humanIdeaSubmission()

	if err := s.Submit(context.Background(), exp); !errors.Is(err, errReachedScoring) {
		t.Fatalf("Submit with unsummarized jobs: got = %v, want it to pass the summary gate", err)
	}
	if exp.NotAdmittedReason != domain.NotAdmittedSummaryGate {
		t.Errorf("not_admitted_reason: got = %q, want %q", exp.NotAdmittedReason, domain.NotAdmittedSummaryGate)
	}
}

type openStore struct{ poolOnlyStore }

func (openStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: domain.PlatformExpOpen}, nil
}

func TestSubmitToAnOpenPlatformExperimentSaysItHasNotStarted(t *testing.T) {
	s := &Service{codeHost: testCodeHost{}, store: openStore{}, hypotheses: humanHypothesis("pe-1")}

	// An incomplete spec too: the start is what the agent must wait for, before any field.
	incomplete := humanIdeaSubmission()
	incomplete.Theory = ""
	for _, exp := range []*domain.Experiment{humanIdeaSubmission(), incomplete} {
		var admission *AdmissionError
		if err := s.Submit(context.Background(), exp); !errors.As(err, &admission) || admission.Reason != "experiment_not_started" || !strings.Contains(admission.Message, "has not started") {
			t.Fatalf("Submit to an open platform experiment: got = %v, want a message saying it has not started", err)
		}
	}
}

type finishedJobStore struct {
	poolOnlyStore
	status domain.PlatformExperimentStatus
	filed  int
	// filedBy is whose credential filed each finding.
	filedBy []domain.Actor
}

func (*finishedJobStore) GetExperiment(_ context.Context, id string) (*domain.Experiment, error) {
	return &domain.Experiment{ID: id, AgentID: "agent-1", HypothesisID: "hyp-1", PlatformExperimentID: "pe-1", Status: domain.StatusCompleted}, nil
}

func (s *finishedJobStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: s.status}, nil
}

func (s *finishedJobStore) CreateHypothesisFinding(_ context.Context, _, _, _, _ string, actor domain.Actor, _ string) (*domain.HypothesisFinding, error) {
	s.filed++
	s.filedBy = append(s.filedBy, actor)
	return &domain.HypothesisFinding{}, nil
}

// The summary is where an agent has the evidence in hand, so its reply names the hypothesis
// whose verdict that agent may set, open or already given: its own or a human's, never another
// agent's. A planner told the verdict of its baseline's hypothesis was its to set tried confirmed
// against the baseline itself, which no rule allows: a draft's job is the control and names none.
func TestASummaryNamesTheHypothesisWhoseVerdictIsTheAgentsToSet(t *testing.T) {
	for _, tc := range []struct {
		h      domain.Hypothesis
		status domain.PlatformExperimentStatus
		want   bool
	}{
		{domain.Hypothesis{ID: "hyp-1", AgentID: "agent-1", Status: domain.HypothesisOpen}, domain.PlatformExpRunning, true},
		{domain.Hypothesis{ID: "hyp-1", Actor: domain.Actor{Kind: domain.ActorUser, ID: "user-jane"}, Status: domain.HypothesisOpen}, domain.PlatformExpRunning, true},
		{domain.Hypothesis{ID: "hyp-1", Actor: domain.Actor{Kind: domain.ActorUser, ID: "user-jane"}, Status: domain.HypothesisConfirmed}, domain.PlatformExpRunning, true},
		{domain.Hypothesis{ID: "hyp-1", AgentID: "agent-2", Status: domain.HypothesisOpen}, domain.PlatformExpRunning, false},
		{domain.Hypothesis{ID: "hyp-1", AgentID: "agent-1", Status: domain.HypothesisOpen}, domain.PlatformExpDraft, false},
	} {
		store := &finishedJobStore{status: tc.status}
		s := &Service{codeHost: testCodeHost{}, store: store, hypotheses: oneHypothesis{h: &tc.h}}
		got, err := s.WriteExperimentSummary(context.Background(), "job-1", domain.Actor{Kind: domain.ActorKey, ID: "key-1"}, "it held")
		if len(store.filedBy) != 1 || store.filedBy[0] != (domain.Actor{Kind: domain.ActorKey, ID: "key-1"}) {
			t.Fatalf("%+v: the finding was filed by %+v, want the credential that sent it", tc.h, store.filedBy)
		}
		if err != nil || (got != nil) != tc.want || tc.want && *got != tc.h || store.filed != 1 {
			t.Fatalf("%+v: got = (%+v, %v), filed %d, want it named: %v, filed 1", tc.h, got, err, store.filed, tc.want)
		}
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
		s := &Service{codeHost: testCodeHost{}, store: tc.store, hypotheses: humanHypothesis("pe-1")}
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
		s := &Service{codeHost: testCodeHost{}, store: poolOnlyStore{}, hypotheses: hypotheses}
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

func (twoExperimentsStore) GetRunningAndQueued(context.Context) ([]*domain.Experiment, error) {
	return []*domain.Experiment{
		{ID: "mine-1", PlatformExperimentID: "pe-1"}, {ID: "theirs-1", PlatformExperimentID: "pe-other"},
		{ID: "mine-2", PlatformExperimentID: "pe-1"}, {ID: "human-idea-job", PlatformExperimentID: "pe-1"},
	}, nil
}

type recordingNovelty struct{ compared []string }

func (n *recordingNovelty) ComputeNovelty(_ context.Context, _ string, existing []*domain.Experiment) (float64, error) {
	for _, e := range existing {
		n.compared = append(n.compared, e.ID)
	}
	return 0, errReachedScoring
}

// Novelty is a comparison within one platform experiment: a hypothesis belongs to one, and
// another's jobs may be another org's.
func TestNoveltyComparesAgainstTheJobsOfTheSamePlatformExperimentOnly(t *testing.T) {
	novelty := &recordingNovelty{}
	s := &Service{codeHost: testCodeHost{}, store: twoExperimentsStore{}, hypotheses: humanHypothesis("pe-1"), novelty: novelty}
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
	s := &Service{codeHost: testCodeHost{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
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
	if err := s.Submit(context.Background(), clash); !errors.As(err, &refused) || refused.Reason != ReasonMalformed || admissionHTTPStatus(refused.Reason) != 400 {
		t.Fatalf("a name in both env and secret_env: got = %v, want a malformed submission answered 400", err)
	}
	if strings.Contains(refused.Message, "the-secret-value") {
		t.Fatalf("the refusal repeats the secret: %s", refused.Message)
	}
}

type summarisedJobStore struct{ finishedJobStore }

func (*summarisedJobStore) CreateHypothesisFinding(_ context.Context, _, _, experimentID, _ string, _ domain.Actor, _ string) (*domain.HypothesisFinding, error) {
	return nil, fmt.Errorf("hypotheses_store.CreateHypothesisFinding: %w: %s", db.ErrFindingExists, experimentID)
}

// A job has one summary. A second one is the caller's to stop sending, so it is a 409 with a
// reason to branch on and none of the store's text.
func TestASecondSummaryOnAJobIsRefusedAsAConflict(t *testing.T) {
	s := &Service{codeHost: testCodeHost{}, store: &summarisedJobStore{}, hypotheses: oneHypothesis{h: &domain.Hypothesis{ID: "hyp-1", AgentID: "agent-1"}}}
	_, err := s.WriteExperimentSummary(context.Background(), "job-1", domain.Actor{Kind: domain.ActorAgent, ID: "agent-1"}, "again")
	refusal := admissionStatusError(err)
	if refusal == nil || refusal.GetStatus() != http.StatusConflict {
		t.Fatalf("a second summary: got = %v (%v), want a 409", refusal, err)
	}
	body, _ := json.Marshal(refusal)
	var answer struct{ Reason, Message string }
	if json.Unmarshal(body, &answer) != nil || answer.Reason != ReasonSummaryExists || answer.Message == "" || strings.Contains(string(body), "hypotheses_store") {
		t.Fatalf("a second summary: body = %s, want reason %s, a message and no store text", body, ReasonSummaryExists)
	}
}
