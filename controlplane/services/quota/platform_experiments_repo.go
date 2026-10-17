package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/contract/repotemplate"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
)

// RepositoryName is the name of the repository the platform creates for a platform experiment:
// what the experiment is called, cut short, and its id, which keeps two of one name apart.
func RepositoryName(pe *domain.PlatformExperiment) string {
	var slug []byte
	for _, c := range []byte(strings.ToLower(pe.Name)) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			slug = append(slug, c)
		case len(slug) > 0 && slug[len(slug)-1] != '-':
			slug = append(slug, '-')
		}
		if len(slug) == 40 {
			break
		}
	}
	name := strings.Trim(string(slug), "-")
	if name == "" {
		name = "experiment"
	}
	return name + "-" + strings.TrimPrefix(pe.ID, "pe-")
}

// RepositoryChoice is where a platform experiment or draft keeps its code: Named, the repository
// the request named, at URL, or with none named CreateOn, the account the repository pass creates
// one on after the create answered (ProvideRepositories).
type RepositoryChoice struct {
	Named, URL, CreateOn string
}

// RepositoryCheck is what a create would keep its code in: the repository it names, or the account
// a new one is created on.
type RepositoryCheck struct {
	Repository        string `json:"repository,omitempty" doc:"The repository named, as the git host names it."`
	RepositoryURL     string `json:"repository_url,omitempty" doc:"Where it is cloned from."`
	RepositoryAccount string `json:"repository_account,omitempty" doc:"The account a new repository would be created on, when none is named."`
}

// chooseRepository is where a create keeps its code: named, which repositoryOf checked, or the
// account a new one is created on, which the git host refuses here when it would refuse the
// creation for the account alone.
func (h *PlatformExperimentsHandler) chooseRepository(ctx context.Context, projectID, named, account string) (RepositoryChoice, error) {
	if named != "" {
		return RepositoryChoice{Named: named, URL: h.repos.URL(named)}, nil
	}
	on, err := h.repos.CreatesOn(ctx, projectID, account)
	return RepositoryChoice{CreateOn: on}, err
}

// ProvideRepositories creates the repository of every platform experiment and draft that awaits
// one, from the template, and records it, from which its machines are started. One whose
// repository the git host does not create is closed repository_not_created with what the host
// answered.
func (s *PlatformExperimentsService) ProvideRepositories(ctx context.Context) error {
	pending, err := s.store.RepositoriesToCreate(ctx)
	if err != nil {
		return fmt.Errorf("platform_experiments.ProvideRepositories: %w", err)
	}
	for _, one := range pending {
		pe := &domain.PlatformExperiment{ID: one.ID, Name: one.Name}
		created, err := s.repos.Create(ctx, one.ProjectID, one.Account, RepositoryName(pe), "Platform experiment "+pe.ID+": "+pe.Name, repotemplate.Files())
		if ctx.Err() != nil {
			return nil
		}
		log := s.logger.With(zap.String("platform_experiment_id", one.ID))
		if err != nil {
			log.Warn("provide_repositories: not created", zap.Error(err))
			if err := s.store.FailRepository(ctx, one.ID, err.Error()); err != nil {
				log.Error("provide_repositories: close it", zap.Error(err))
			}
			continue
		}
		if err := s.store.GiveRepository(ctx, one.ID, created, s.repos.URL(created)); err != nil {
			log.Error("provide_repositories: record the repository", zap.String("repository", created), zap.Error(err))
			continue
		}
		log.Info("provide_repositories: created", zap.String("repository", created))
	}
	return nil
}

// wakeRepositories wakes the repository pass for pe when it awaits its repository.
func (s *PlatformExperimentsService) wakeRepositories(pe *domain.PlatformExperiment) {
	if !pe.AwaitsRepository() {
		return
	}
	select {
	case s.repositoryWake <- struct{}{}:
	default:
	}
}

// StartRepositoryPass runs ProvideRepositories when a create wakes it and every interval, until
// ctx is cancelled, each pass bounded by passTimeout.
func (s *PlatformExperimentsService) StartRepositoryPass(ctx context.Context, interval, passTimeout time.Duration) {
	if s.repos == nil {
		panic("quota: StartRepositoryPass without WithRepositories: no platform experiment would get its repository")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.repositoryWake:
		case <-ticker.C:
		}
		passCtx, cancel := context.WithTimeout(ctx, passTimeout)
		if err := s.ProvideRepositories(passCtx); err != nil {
			s.logger.Error("provide_repositories: scan", zap.Error(err))
		}
		cancel()
	}
}

// repositoryOf checks what a request says of its repository: the one it names must be reachable
// and is returned as the git host names it, and an account is said only for one to be created.
func (h *PlatformExperimentsHandler) repositoryOf(ctx context.Context, projectID, repository, account string) (string, error) {
	if repository == "" {
		return "", nil
	}
	if account != "" {
		return "", api.Refuse(http.StatusUnprocessableEntity, "invalid_request", "repository_account says where a new repository is created and repository names an existing one: state one of them")
	}
	return h.repos.Reachable(ctx, projectID, repository)
}

// neverFromTheFile are what one run costs and how long it lasts. Any agent's token can push to
// the branch the file is read from, so these come from the request and from nowhere else.
var neverFromTheFile = []string{"budget_accelerator_hours", "max_agents", "researchers", "duration_hours", "coordinator_hosted", "coordinator_model", "agent_image"}

// fromRepo returns req laid over the experiment file of the repository it names: every field the
// body carried is the request's, every other one the file's.
func (h *PlatformExperimentsHandler) fromRepo(ctx context.Context, req CreatePlatformExperimentRequest) (CreatePlatformExperimentRequest, error) {
	data, err := h.repos.File(ctx, req.ProjectID, req.Repository, domain.ExperimentFile)
	if err != nil {
		return req, err
	}
	merged, err := decodeExperimentBase(data)
	if err != nil {
		return req, invalidExperimentFile(req.Repository, err)
	}
	if err := json.Unmarshal(req.raw, (*createFields)(&merged)); err != nil {
		return req, fmt.Errorf("lay the request over the experiment file: %w", err)
	}
	return merged, nil
}

func invalidExperimentFile(repository string, err error) error {
	return api.Refuse(http.StatusUnprocessableEntity, "invalid_experiment_file", domain.ExperimentFile+" of "+repository+" cannot be used: "+err.Error())
}

// ExperimentFile is what a repository's experiment file states of a platform experiment: the
// fields a create with from_repo would take from it. A form shows them before anything is
// created.
type ExperimentFile struct {
	Repository                string                    `json:"repository"`
	Name                      string                    `json:"name,omitempty"`
	Description               string                    `json:"description,omitempty"`
	Metrics                   []domain.MetricDefinition `json:"metrics,omitempty"`
	ReportIntervalSeconds     int                       `json:"report_interval_seconds,omitempty"`
	Stages                    []domain.Stage            `json:"stages,omitempty"`
	DefaultQuotaTier          string                    `json:"default_quota_tier,omitempty"`
	MaxConcurrentAccelerators *int                      `json:"max_concurrent_accelerators,omitempty"`
}

// experimentFile reads the experiment file of a repository the project's org reaches, by the
// decoding a create with from_repo uses, so that what is shown is what such a create would take.
func (h *PlatformExperimentsHandler) experimentFile(ctx context.Context, projectID, repository string) (*ExperimentFile, error) {
	named, err := h.repos.Reachable(ctx, projectID, repository)
	if err != nil {
		return nil, err
	}
	data, err := h.repos.File(ctx, projectID, named, domain.ExperimentFile)
	if err != nil {
		return nil, err
	}
	base, err := decodeExperimentBase(data)
	if err != nil {
		return nil, invalidExperimentFile(named, err)
	}
	return &ExperimentFile{
		Repository: named, Name: base.Name, Description: base.Description, Metrics: base.Metrics,
		ReportIntervalSeconds: base.ReportIntervalSeconds, Stages: base.Stages,
		DefaultQuotaTier: base.DefaultQuotaTier, MaxConcurrentAccelerators: base.MaxConcurrentAccelerators,
	}, nil
}

// decodeExperimentBase reads the experiment file: YAML shaped like the create body. It cannot
// choose its project, what neverFromTheFile names is left out of it, and a key the body does not
// have is an error, since a mistyped one would otherwise be dropped in silence.
func decodeExperimentBase(data []byte) (CreatePlatformExperimentRequest, error) {
	var base CreatePlatformExperimentRequest
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return base, fmt.Errorf("it is not a YAML mapping: %w", err)
	}
	if len(document) == 0 {
		return base, fmt.Errorf("it is empty")
	}
	for _, key := range []string{"project_id", "from_repo", "repository"} {
		if _, set := document[key]; set {
			return base, fmt.Errorf("it must not set %s: the request does", key)
		}
	}
	for _, key := range neverFromTheFile {
		delete(document, key)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return base, fmt.Errorf("it holds a value the create body cannot, such as a key that is not a string: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode((*createFields)(&base)); err != nil {
		return base, fmt.Errorf("it is not shaped like the create body: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	return base, nil
}
