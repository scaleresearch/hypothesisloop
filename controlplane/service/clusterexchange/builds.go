package clusterexchange

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

func noEnvironments() error {
	return refusal.Reject(http.StatusConflict, "this deployment builds no environments")
}

func notAssigned(cluster, key string, attempt int) error {
	return refusal.Reject(http.StatusConflict, "the build of "+key+" is not assigned to cluster "+cluster+" at attempt "+strconv.Itoa(attempt))
}

// BuildCredential is a login that pushes the build of key, while it is assigned to cluster at
// attempt and still building.
func (s *Service) BuildCredential(ctx context.Context, cluster, key string, attempt int) (domain.RegistryLogin, error) {
	if s.environments == nil {
		return domain.RegistryLogin{}, noEnvironments()
	}
	env, err := s.store.AssignedEnvironmentBuild(ctx, cluster, key, attempt)
	if err != nil {
		return domain.RegistryLogin{}, auth.Internal(s.logger, "environment build credential", err, zap.String("cluster", cluster), zap.String("key", key))
	}
	if env == nil {
		return domain.RegistryLogin{}, notAssigned(cluster, key, attempt)
	}
	login, err := s.environments.Logins.Push(ctx)
	if err != nil {
		return domain.RegistryLogin{}, auth.Internal(s.logger, "environment build credential", err, zap.String("cluster", cluster), zap.String("key", key))
	}
	return login, nil
}

// BuildGitCredential is a read of the repository the build of key is of, through the git proxy,
// while it is assigned to cluster at attempt and still building.
func (s *Service) BuildGitCredential(ctx context.Context, cluster, key string, attempt int) (domain.EnvironmentGitCredential, error) {
	if s.environments == nil {
		return domain.EnvironmentGitCredential{}, noEnvironments()
	}
	env, err := s.store.AssignedEnvironmentBuild(ctx, cluster, key, attempt)
	if err != nil {
		return domain.EnvironmentGitCredential{}, auth.Internal(s.logger, "environment git credential", err, zap.String("cluster", cluster), zap.String("key", key))
	}
	if env == nil {
		return domain.EnvironmentGitCredential{}, notAssigned(cluster, key, attempt)
	}
	if env.GitRepository == "" {
		return domain.EnvironmentGitCredential{}, refusal.Reject(http.StatusNotFound, "the environment "+key+" is of no repository")
	}
	if s.proxyURL == "" {
		return domain.EnvironmentGitCredential{}, auth.Internal(s.logger, "environment git credential", errors.New("the address of the git proxy is not configured"), zap.String("key", key))
	}
	token, err := auth.Mint(s.signingSecret, auth.BuildScope(cluster, key, attempt))
	if err != nil {
		return domain.EnvironmentGitCredential{}, auth.Internal(s.logger, "environment git credential", err, zap.String("cluster", cluster), zap.String("key", key))
	}
	return domain.EnvironmentGitCredential{
		ProxyURL:      domain.BuildGitProxyURL(s.proxyURL, key),
		RepositoryURL: s.environments.Git.URL(env.GitRepository),
		Token:         token,
	}, nil
}

// ReportBuild records the outcome of the build of key.
func (s *Service) ReportBuild(ctx context.Context, cluster, key string, report domain.EnvironmentBuildReport) error {
	if s.environments == nil {
		return noEnvironments()
	}
	if len(report.LogTail) > domain.MaxEnvironmentLogBytes {
		return refusal.Reject(http.StatusBadRequest, "log_tail is longer than "+strconv.Itoa(domain.MaxEnvironmentLogBytes)+" bytes")
	}
	outcome := domain.EnvironmentBuildOutcome{LogTail: report.LogTail}
	switch report.Outcome {
	case domain.EnvironmentBuildPushed:
		parsed, err := digest.Parse(report.Digest)
		if err != nil || parsed.Algorithm() != digest.SHA256 {
			return refusal.Reject(http.StatusBadRequest, "a pushed build names its digest, sha256:<hex>")
		}
		outcome.Image = s.environments.Repository + "@" + report.Digest
	case domain.EnvironmentBuildFileFault:
		outcome.Fault = domain.EnvironmentFaultFile
	case domain.EnvironmentBuildPlatformFault:
		outcome.Fault = domain.EnvironmentFaultPlatform
	default:
		return refusal.Reject(http.StatusBadRequest, "outcome is pushed, file or platform")
	}
	if report.Outcome != domain.EnvironmentBuildPushed && report.Digest != "" {
		return refusal.Reject(http.StatusBadRequest, "only a pushed build names a digest")
	}
	current, err := s.store.FinishEnvironmentBuild(ctx, cluster, key, report.Attempt, outcome)
	if err != nil {
		return auth.Internal(s.logger, "environment build report", err, zap.String("cluster", cluster), zap.String("key", key))
	}
	if !current {
		return notAssigned(cluster, key, report.Attempt)
	}
	s.logger.Info("environment build reported", zap.String("cluster", cluster), zap.String("key", key),
		zap.Int("attempt", report.Attempt), zap.String("outcome", string(report.Outcome)))
	s.wakeAdmission()
	return nil
}

// BuildLoaded records the image of the build of key held on one node while it is pushed.
func (s *Service) BuildLoaded(ctx context.Context, cluster, key string, loaded domain.EnvironmentBuildLoaded) error {
	if s.environments == nil {
		return noEnvironments()
	}
	if parsed, err := digest.Parse(loaded.Digest); err != nil || parsed.Algorithm() != digest.SHA256 {
		return refusal.Reject(http.StatusBadRequest, "digest is the image's, sha256:<hex>")
	}
	if loaded.Node == "" || len(loaded.Node) > domain.MaxNodeNameBytes || strings.ContainsFunc(loaded.Node, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return refusal.Reject(http.StatusBadRequest, "node is the name of the node, 1 to "+strconv.Itoa(domain.MaxNodeNameBytes)+" bytes")
	}
	current, err := s.store.RecordEnvironmentLoaded(ctx, cluster, key, loaded.Attempt, loaded.Node, s.environments.Repository+"@"+loaded.Digest)
	if err != nil {
		return auth.Internal(s.logger, "environment build loaded", err, zap.String("cluster", cluster), zap.String("key", key))
	}
	if !current {
		return notAssigned(cluster, key, loaded.Attempt)
	}
	s.logger.Info("environment build loaded", zap.String("cluster", cluster), zap.String("key", key),
		zap.Int("attempt", loaded.Attempt), zap.String("node", loaded.Node), zap.String("digest", loaded.Digest))
	s.wakeAdmission()
	return nil
}

// BuildProgress records the step the build of key is at.
func (s *Service) BuildProgress(ctx context.Context, cluster, key string, progress domain.EnvironmentBuildProgress) error {
	if s.environments == nil {
		return noEnvironments()
	}
	step := progress.Step
	if step == "" || len(step) > domain.MaxEnvironmentProgressBytes || strings.ContainsAny(step, "\r\n") {
		return refusal.Reject(http.StatusBadRequest, "step is one line of 1 to "+strconv.Itoa(domain.MaxEnvironmentProgressBytes)+" bytes")
	}
	current, err := s.store.RecordEnvironmentProgress(ctx, cluster, key, progress.Attempt, step)
	if err != nil {
		return auth.Internal(s.logger, "environment build progress", err, zap.String("cluster", cluster), zap.String("key", key))
	}
	if !current {
		return notAssigned(cluster, key, progress.Attempt)
	}
	return nil
}
