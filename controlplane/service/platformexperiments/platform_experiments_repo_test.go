package platformexperiments

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

const experimentBase = `
name: tune the model
description: beat the baseline in the README
budget_accelerator_hours: 40
max_agents: 6
report_interval_seconds: 30
duration_hours: 400
metrics:
  - key: val_accuracy
    direction: maximize
stages:
  - length_pct: 100
    evict_pct: 0
`

func TestTheExperimentFileIsShapedLikeTheCreateBody(t *testing.T) {
	base, err := decodeExperimentBase([]byte(experimentBase))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if base.Name != "tune the model" || base.ReportIntervalSeconds != 30 || len(base.Metrics) != 1 || base.Metrics[0].Key != "val_accuracy" ||
		len(base.Stages) != 1 || base.Stages[0].LengthPct != 100 {
		t.Fatalf("base = %+v", base)
	}
	// What a run costs and how long it lasts is never the file's to say.
	if base.BudgetAcceleratorHours != 0 || base.MaxAgents != 0 || base.DurationHours != 0 {
		t.Fatalf("base = %+v, want no budget, no agent count and no length taken from the file", base)
	}
	for name, file := range map[string]string{
		"a project of its own choosing": experimentBase + "project_id: prj_other\n",
		"from_repo":                     experimentBase + "from_repo: true\n",
		"a key the body does not have":  experimentBase + "budget_acelerator_hours: 4\n",
		"a list":                        "- name: a\n",
		"not yaml":                      "name: [unclosed\n",
		"a value of the wrong type":     "report_interval_seconds: often\n",
		"empty":                         "",
		"only a comment":                "# nothing yet\n",
		"a key that is a number":        experimentBase + "7: seven\n",
		"a nested key that is a number": "name: x\nmetrics:\n  - key: a\n    direction: maximize\n    1: one\n",
		"a nested key that is a list":   "name: x\nstages:\n  - ? [a, b]\n    : c\n",
	} {
		if _, err := decodeExperimentBase([]byte(file)); err == nil {
			t.Errorf("%s: the file was accepted", name)
		}
	}
}

// fakeRepositories is GitHub as the org of each project reaches it: files holds, by project, the
// experiment file of the one repository its org's installation covers, acme/model, which GitHub
// spells Acme/Model.
type fakeRepositories struct {
	files   map[string]string
	refusal error
	checked []string
	asked   []string
	// created holds each repository made for an experiment that names none, as "project name",
	// and committed the files the last one started from.
	created    []string
	committed  map[string]string
	notCreated error
	// notOn is what refuses an account before anything is created.
	notOn error
}

func (f *fakeRepositories) Create(_ context.Context, projectID, account, name, _ string, files map[string]string) (string, error) {
	if f.notCreated != nil {
		return "", f.notCreated
	}
	f.created = append(f.created, projectID+" "+name)
	f.committed = files
	if account == "" {
		account = "acme"
	}
	return account + "/" + name, nil
}

// CreatesOn answers the account named, or acme, the only one the org connected, and notOn when
// that is set.
func (f *fakeRepositories) CreatesOn(_ context.Context, _, account string) (string, error) {
	if f.notOn != nil {
		return "", f.notOn
	}
	if account == "" {
		return "acme", nil
	}
	return account, nil
}

const testRepository = "Acme/Model"

func notReachable() error {
	return refusal.Refuse(http.StatusUnprocessableEntity, "repository_not_reachable", "the GitHub installation the org connected does not cover that repository")
}

func (f *fakeRepositories) Reachable(_ context.Context, projectID, repository string) (string, error) {
	f.checked = append(f.checked, projectID+" "+repository)
	if _, ok := f.files[projectID]; !ok || !strings.EqualFold(repository, testRepository) {
		return "", notReachable()
	}
	return testRepository, nil
}

func (f *fakeRepositories) URL(repository string) string {
	return "https://github.com/" + repository + ".git"
}

func (f *fakeRepositories) Credential(context.Context, string, string, bool) (*githost.Credential, error) {
	panic("the handler asked for a git credential")
}

func (f *fakeRepositories) Serves(string, string, string, string) bool { return false }

func (f *fakeRepositories) FileAt(context.Context, string, string, string, string) ([]byte, error) {
	return nil, githost.ErrFileNotFound
}

func (f *fakeRepositories) File(_ context.Context, projectID, repository, _ string) ([]byte, error) {
	f.asked = append(f.asked, projectID+" "+repository)
	if f.refusal != nil {
		return nil, f.refusal
	}
	return []byte(f.files[projectID]), nil
}

type createdStore struct {
	fakeStore
	created []*domain.PlatformExperiment
	// prior are the prior runs of every platform experiment it holds.
	prior []string
	// messages are the messages sent to its agents.
	messages []domain.AgentMessage
	// researchers are the agents of role competitor that joined.
	researchers []string
	// baselines are the agents of role baseline that joined, and left the COMPLETED jobs on the
	// objective of those of them that finished.
	baselines []domain.AgentSignup
	left      []*domain.Experiment
	// objective are the jobs on the objective, oldest first.
	objective []*domain.Experiment
	// completed is every COMPLETED job, oldest first.
	completed []*domain.Experiment
	// atWork is the researchers at work by agent type and model, as the store counts them.
	atWork map[domain.PoolKey]int
	// started counts the starts that reached the store.
	started int
	// answered is when a launcher last asked for its machines, by name. One not in it asked now.
	answered map[string]*time.Time
}

func (s *createdStore) awaiting(pe *domain.PlatformExperiment) bool {
	return pe.Status != domain.PlatformExpClosed && pe.AwaitsRepository()
}

func (s *createdStore) PriorRuns(context.Context, string) ([]string, error) {
	return s.prior, nil
}

func (s *createdStore) RunsToRank(_ context.Context, ids []string) ([]db.RunToRank, error) {
	var out []db.RunToRank
	for _, id := range ids {
		for _, pe := range s.created {
			if pe.ID == id {
				out = append(out, db.RunToRank{PE: pe})
			}
		}
	}
	return out, nil
}

func (s *createdStore) RepositoriesToCreate(context.Context) ([]db.RepositoryToCreate, error) {
	var out []db.RepositoryToCreate
	for _, pe := range s.created {
		if s.awaiting(pe) {
			out = append(out, db.RepositoryToCreate{ID: pe.ID, ProjectID: pe.ProjectID, Name: pe.Name, Account: pe.RepositoryAccount})
		}
	}
	return out, nil
}

func (s *createdStore) GiveRepository(_ context.Context, id, repository, url string) error {
	for _, pe := range s.created {
		if pe.ID == id && s.awaiting(pe) {
			pe.Repository, pe.RepositoryURL = repository, url
			return nil
		}
	}
	return db.ErrRepositoryNotAwaited
}

func (s *createdStore) FailRepository(_ context.Context, id, answer string) error {
	for _, pe := range s.created {
		if pe.ID == id && s.awaiting(pe) {
			pe.Status, pe.CloseReason, pe.RepositoryError = domain.PlatformExpClosed, domain.CloseRepositoryNotCreated, answer
		}
	}
	return nil
}

func (s *createdStore) ListExperimentsTesting(context.Context, []string) ([]*domain.Experiment, error) {
	return s.objective, nil
}

func (s *createdStore) FinishedBaselineJobs(context.Context, string) ([]*domain.Experiment, error) {
	return s.left, nil
}

func (s *createdStore) CompletedJobs(context.Context, string) ([]*domain.Experiment, error) {
	return s.completed, nil
}

// StartPlatformExperimentTx starts the open platform experiment id with a researcher at work.
func (s *createdStore) StartPlatformExperimentTx(_ context.Context, id string, _ time.Duration, quotasFor func([]db.StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error)) (bool, []*domain.AgentQuota, error) {
	participants := make([]db.StartParticipant, len(s.researchers))
	for i, agentID := range s.researchers {
		participants[i] = db.StartParticipant{AgentID: agentID, Role: domain.SignupRoleCompetitor, AgentExists: true, QuotaTier: domain.QuotaTierGuaranteed}
	}
	quotas, _, err := quotasFor(participants)
	if err != nil {
		return false, nil, err
	}
	for _, pe := range s.created {
		if pe.ID == id {
			pe.Status, pe.StartedAt = domain.PlatformExpRunning, time.Now().UTC()
			pe.SetClosesAt(pe.StartedAt.Add(time.Hour))
		}
	}
	s.started++
	return true, quotas, nil
}

func (s *createdStore) ListSignupsByRole(_ context.Context, _ string, role domain.SignupRole) ([]string, error) {
	if role != domain.SignupRoleCompetitor {
		return nil, nil
	}
	return s.researchers, nil
}

func (s *createdStore) CreatePlatformExperiment(_ context.Context, pe *domain.PlatformExperiment, _ []*domain.Agent, signups []domain.AgentSignup) error {
	s.created = append(s.created, pe)
	for _, signup := range signups {
		if signup.Role != domain.SignupRoleBaseline {
			s.researchers = append(s.researchers, signup.AgentID)
			continue
		}
		signup.Name = "Baseline 1"
		s.baselines = append(s.baselines, signup)
	}
	return nil
}

func (s *createdStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	for _, pe := range s.created {
		if pe.ID == id {
			out := *pe
			return &out, nil
		}
	}
	return nil, nil
}

func (s *createdStore) ResearchersAtWork(context.Context, string) (map[domain.PoolKey]int, error) {
	return s.atWork, nil
}

func (s *createdStore) ListSignups(context.Context, string) ([]string, error) { return nil, nil }

func (s *createdStore) ListAgentSignups(context.Context, string) ([]domain.AgentSignup, error) {
	signups := make([]domain.AgentSignup, len(s.researchers))
	for i, agentID := range s.researchers {
		signups[i] = domain.AgentSignup{AgentID: agentID, Role: domain.SignupRoleCompetitor}
	}
	return append(signups, s.baselines...), nil
}

// UpdatePlatformExperiment holds max_agents to the researchers that joined, every one at work.
func (s *createdStore) UpdatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment, status domain.PlatformExperimentStatus, researchers domain.ResearcherPool) error {
	return s.update(pe, status, researchers, len(s.researchers))
}

// update refuses a max_agents below atWork before the run starts; once it runs the store cuts the
// newest above it.
func (s *createdStore) update(pe *domain.PlatformExperiment, status domain.PlatformExperimentStatus, researchers domain.ResearcherPool, atWork int) error {
	if pe.MaxAgents < atWork && status != domain.PlatformExpRunning {
		return &db.PoolBelowAtWorkError{Count: pe.MaxAgents, AtWork: atWork}
	}
	if researchers != nil {
		pe.Researchers = researchers
	}
	for i, stored := range s.created {
		if stored.ID == pe.ID {
			s.created[i] = pe
		}
	}
	return nil
}

func (s *createdStore) RaiseBaseline(ctx context.Context, pe *domain.PlatformExperiment, raise domain.BaselineRaise) error {
	raise.At = time.Now().UTC()
	pe.BaselineRaises = append(pe.BaselineRaises, raise)
	return s.UpdatePlatformExperiment(ctx, pe, domain.PlatformExpRunning, nil)
}

func (s *createdStore) ListAgentMessages(context.Context, string, string) ([]domain.AgentMessage, error) {
	return s.messages, nil
}
