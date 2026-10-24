package config

import "github.com/scaleresearch/hypothesisloop/controlplane/domain"

// ControlService is what control-service runs with: the settings and the secrets they call for.
type ControlService struct {
	*Settings
	DatabaseURL, MetricsDBURL, GraphDBURL, SigningSecret string
	ObjectStoreSecret                                    string
	// ModelCredentials is the credential of each model provider the platform serves, by provider:
	// those whose key is set.
	ModelCredentials map[string]string
	// GitHubKey and GitHubClientSecret are set on a github git host, and GitPassword on a plain
	// one with a username.
	GitHubKey, GitHubClientSecret, GitPassword string
	// SandboxSecret is the sandbox provider's key, empty with no sandbox.
	SandboxSecret string
	// CloudComputeSecrets is the account key of each cloud_compute entry, by cluster name.
	CloudComputeSecrets map[string]string
}

func LoadControlService() (*ControlService, error) {
	s, env, err := load(ProgramControlService)
	if err != nil {
		return nil, err
	}
	c := &ControlService{
		Settings: s, DatabaseURL: env[DatabaseURL], MetricsDBURL: env[MetricsDBURL], GraphDBURL: env[GraphDBURL], SigningSecret: env[AuthSigningSecret],
		ObjectStoreSecret: env[objectStoreKeys[s.ObjectStore.Provider]], ModelCredentials: map[string]string{},
		GitHubKey: env[GitHostGitHubAppPrivateKey], GitHubClientSecret: env[GitHostGitHubClientSecret], GitPassword: env[GitHostPlainPassword],
		CloudComputeSecrets: map[string]string{},
	}
	for _, provider := range domain.ModelProviders {
		if credential := lookup(modelKeys[provider]); credential != "" {
			c.ModelCredentials[provider] = credential
		}
	}
	if s.Sandbox != nil {
		c.SandboxSecret = env[AccountKey(SectionSandbox, s.Sandbox.Provider)]
	}
	for _, cc := range s.CloudCompute {
		c.CloudComputeSecrets[cc.ClusterName] = env[AccountKey(SectionCloudCompute, cc.ClusterName)]
	}
	return c, nil
}

// MetricsService is what metrics-service runs with.
type MetricsService struct {
	*Settings
	DatabaseURL, MetricsDBURL, SigningSecret string
}

func LoadMetricsService() (*MetricsService, error) {
	s, env, err := load(ProgramMetricsService)
	if err != nil {
		return nil, err
	}
	return &MetricsService{Settings: s, DatabaseURL: env[DatabaseURL], MetricsDBURL: env[MetricsDBURL], SigningSecret: env[AuthSigningSecret]}, nil
}

// Admin is what hl-admin runs with. GitHubKey is set on a github git host.
type Admin struct {
	*Settings
	DatabaseURL, SigningSecret, GitHubKey string
}

func LoadAdmin() (*Admin, error) {
	s, env, err := load(ProgramAdmin)
	if err != nil {
		return nil, err
	}
	return &Admin{Settings: s, DatabaseURL: env[DatabaseURL], SigningSecret: env[AuthSigningSecret], GitHubKey: env[GitHostGitHubAppPrivateKey]}, nil
}
