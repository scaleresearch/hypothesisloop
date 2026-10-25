package platformexperiments

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
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
func (s *Service) chooseRepository(ctx context.Context, projectID, named, account string) (RepositoryChoice, error) {
	if named != "" {
		return RepositoryChoice{Named: named, URL: s.repos.URL(named)}, nil
	}
	on, err := s.repos.CreatesOn(ctx, projectID, account)
	return RepositoryChoice{CreateOn: on}, err
}

// ProvideRepositories creates the repository of every platform experiment and draft that awaits
// one, from the experiment template, and records it, from which its machines are started. One whose
// repository the git host does not create is closed repository_not_created with what the host
// answered.
func (s *Service) ProvideRepositories(ctx context.Context) error {
	pending, err := s.store.RepositoriesToCreate(ctx)
	if err != nil {
		return fmt.Errorf("platform_experiments.ProvideRepositories: %w", err)
	}
	for _, one := range pending {
		pe := &domain.PlatformExperiment{ID: one.ID, Name: one.Name}
		created, branch, err := s.repos.Create(ctx, one.ProjectID, one.Account+"/"+RepositoryName(pe), "Platform experiment "+pe.ID+": "+pe.Name)
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
		if err := s.store.GiveRepository(ctx, one.ID, created, s.repos.URL(created), branch); err != nil {
			log.Error("provide_repositories: record the repository", zap.String("repository", created), zap.Error(err))
			continue
		}
		log.Info("provide_repositories: created", zap.String("repository", created))
	}
	return nil
}

// wakeRepositories wakes the repository pass for pe when it awaits its repository.
func (s *Service) wakeRepositories(pe *domain.PlatformExperiment) {
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
func (s *Service) StartRepositoryPass(ctx context.Context, interval, passTimeout time.Duration) {
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

// repositoryOf checks what a request says of its repository and branch: the repository it names
// is created from the experiment template when it does not exist (EnsureRepository) and returned
// as the git host names it, and must have the branch. An account is said only for a repository
// the platform creates later, which states no branch: its default one is recorded with it.
func (s *Service) repositoryOf(ctx context.Context, projectID, repository, branch, account string) (string, error) {
	if repository == "" {
		if branch != "" {
			return "", refusal.Refuse(http.StatusUnprocessableEntity, "invalid_request", "branch names a branch of the repository named, and none is: a repository the platform creates is worked on at its default branch")
		}
		return "", nil
	}
	if account != "" {
		return "", refusal.Refuse(http.StatusUnprocessableEntity, "invalid_request", "repository_account says where a new repository is created and repository names one: state one of them")
	}
	if err := experimentspec.ValidateBranch(branch); err != nil {
		return "", refusal.Refuse(http.StatusUnprocessableEntity, "invalid_branch", err.Error()+": branch is required with repository")
	}
	named, err := s.EnsureRepository(ctx, projectID, repository)
	if err != nil {
		return "", err
	}
	has, err := s.repos.HasBranch(ctx, projectID, named, branch)
	if err != nil {
		return "", err
	}
	if !has {
		return "", refusal.Refuse(http.StatusUnprocessableEntity, "branch_not_found", named+" has no branch "+branch+": push it, or name a branch it has")
	}
	return named, nil
}

// EnsureRepository returns the repository as the git host names it, created from the experiment
// template when the platform cannot reach it.
func (s *Service) EnsureRepository(ctx context.Context, projectID, repository string) (string, error) {
	named, err := s.repos.Reachable(ctx, projectID, repository)
	if refusal.ReasonOf(err) != "repository_not_reachable" {
		return named, err
	}
	created, _, err := s.repos.Create(ctx, projectID, repository, "")
	if refusal.ReasonOf(err) == "repository_exists" {
		return "", refusal.Refuse(http.StatusUnprocessableEntity, "repository_not_reachable", repository+" exists and the platform cannot reach it: its owner gives the platform access to it")
	}
	if err != nil {
		return "", err
	}
	s.logger.Info("repository created from the experiment template", zap.String("project_id", projectID), zap.String("repository", created))
	return created, nil
}

// RepositoryURL is where the repository is cloned from.
func (s *Service) RepositoryURL(repository string) string { return s.repos.URL(repository) }

// ExperimentFile is the experiment file of a repository with where it was read, which a form
// shows before anything is created.
type ExperimentFile struct {
	experimentspec.File
}

// ReadExperimentFile reads the experiment file of a repository the project's org reaches at
// branch, and creates nothing. It answers the file with where it was read: one that places the
// experiment elsewhere is refused experiment_file_elsewhere, and one that still holds the
// template's placeholders experiment_file_unfilled.
func (s *Service) ReadExperimentFile(ctx context.Context, projectID, repository, branch string) (*ExperimentFile, error) {
	if err := experimentspec.ValidateBranch(branch); err != nil {
		return nil, refusal.Refuse(http.StatusUnprocessableEntity, "invalid_branch", err.Error())
	}
	named, err := s.repos.Reachable(ctx, projectID, repository)
	if err != nil {
		return nil, err
	}
	at := experimentspec.ExperimentFile + " at " + branch + " of " + named
	data, err := s.repos.File(ctx, projectID, named, branch, experimentspec.ExperimentFile)
	if errors.Is(err, githost.ErrFileNotFound) {
		return nil, refusal.Refuse(http.StatusUnprocessableEntity, "no_experiment_file", named+" has no "+experimentspec.ExperimentFile+" at "+branch)
	}
	if err != nil {
		return nil, err
	}
	if n := experimentspec.Unfilled(data); n > 0 {
		return nil, refusal.Refuse(http.StatusUnprocessableEntity, "experiment_file_unfilled", fmt.Sprintf("%s still has %d %s to fill in, as the experiment template leaves it", at, n, experimentspec.Placeholder))
	}
	file, err := experimentspec.Parse(data)
	if err != nil {
		return nil, refusal.Refuse(http.StatusUnprocessableEntity, "invalid_experiment_file", at+" cannot be used: "+err.Error())
	}
	stated := file.Placement
	if (stated.ProjectID != "" && stated.ProjectID != projectID) || (stated.Repository != "" && !strings.EqualFold(stated.Repository, named)) || (stated.Branch != "" && stated.Branch != branch) {
		return nil, refusal.Refuse(http.StatusUnprocessableEntity, "experiment_file_elsewhere", fmt.Sprintf("%s places the experiment in project %q, repository %q, branch %q: read it there", at, stated.ProjectID, stated.Repository, stated.Branch))
	}
	file.Placement = experimentspec.Placement{ProjectID: projectID, Repository: named, Branch: branch}
	return &ExperimentFile{File: file}, nil
}

// CheckRepository answers where a platform experiment of projectID naming repository on account
// would keep its code, and creates nothing: a repository named must be reachable.
func (s *Service) CheckRepository(ctx context.Context, projectID, repository, account string) (RepositoryCheck, error) {
	if repository != "" && account != "" {
		return RepositoryCheck{}, refusal.Refuse(http.StatusUnprocessableEntity, "invalid_request", "repository_account says where a new repository is created and repository names one: state one of them")
	}
	named := ""
	if repository != "" {
		var err error
		if named, err = s.repos.Reachable(ctx, projectID, repository); err != nil {
			return RepositoryCheck{}, err
		}
	}
	choice, err := s.chooseRepository(ctx, projectID, named, account)
	if err != nil {
		return RepositoryCheck{}, err
	}
	return RepositoryCheck{Repository: choice.Named, RepositoryURL: choice.URL, RepositoryAccount: choice.CreateOn}, nil
}
